package bots

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"gorm.io/gorm"

	"github.com/ai-code-101/rag-backend/api/internal/models"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrLimitReached = errors.New("limit reached")
	ErrVersionClash = errors.New("config was modified by someone else; reload and try again")
	errKeyCollision = errors.New("public key collision")
)

type Store struct {
	db             *gorm.DB
	maxBotsPerUser int
	maxDocsPerBot  int

	// Short-lived cache of public-key lookups: widget traffic hits this on every request.
	cacheMu  sync.RWMutex
	cache    map[string]cachedBot
	cacheTTL time.Duration
}

type cachedBot struct {
	bot     *PublicBot
	expires time.Time
}

func NewStore(db *gorm.DB, maxBots, maxDocs int) *Store {
	return &Store{db: db, maxBotsPerUser: maxBots, maxDocsPerBot: maxDocs, cache: map[string]cachedBot{}, cacheTTL: 30 * time.Second}
}

func (s *Store) DB() *gorm.DB { return s.db }

// ---- Bots ------------------------------------------------------------------

func (s *Store) ListBots(ctx context.Context, owner uuid.UUID) ([]models.Bot, error) {
	var out []models.Bot
	err := s.db.WithContext(ctx).Where("owner_id = ?", owner).Order("created_at DESC").Find(&out).Error
	return out, err
}

func (s *Store) GetBot(ctx context.Context, owner, id uuid.UUID) (*models.Bot, error) {
	var b models.Bot
	err := s.db.WithContext(ctx).Where("id = ? AND owner_id = ?", id, owner).Take(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &b, err
}

func (s *Store) CreateBot(ctx context.Context, owner uuid.UUID, in BotInput, defaultModel string) (*models.Bot, error) {
	if in.Name == nil {
		return nil, invalid("name", "is required")
	}
	b := &models.Bot{
		OwnerID: owner, Name: *in.Name, Model: defaultModel, Temperature: 0.3, AllowedOrigins: pq.StringArray{},
		FallbackMessage: "I'm not sure about that. Could you rephrase, or contact our team for help?",
	}
	applyBotInput(b, in)
	defaults := DefaultWidgetSettings(b.Name)

	for attempt := 0; attempt < 3; attempt++ {
		key, err := NewPublicKey()
		if err != nil {
			return nil, err
		}
		b.PublicKey = key
		b.ID = uuid.New()
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// Lock the owner row so concurrent creates cannot exceed the per-user limit.
			if err := tx.Exec("SELECT 1 FROM users WHERE id = ? FOR UPDATE", owner).Error; err != nil {
				return err
			}
			var n int64
			if err := tx.Model(&models.Bot{}).Where("owner_id = ?", owner).Count(&n).Error; err != nil {
				return err
			}
			if int(n) >= s.maxBotsPerUser {
				return ErrLimitReached
			}
			if err := tx.Create(b).Error; err != nil {
				if errors.Is(err, gorm.ErrDuplicatedKey) {
					return errKeyCollision
				}
				return err
			}
			wc := widgetConfigFrom(b.ID, defaults)
			return tx.Create(&wc).Error
		})
		if errors.Is(err, errKeyCollision) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return nil, errors.New("could not allocate public key")
}

func applyBotInput(b *models.Bot, in BotInput) {
	if in.Name != nil {
		b.Name = *in.Name
	}
	if in.SystemInstructions != nil {
		b.SystemInstructions = *in.SystemInstructions
	}
	if in.FallbackMessage != nil {
		b.FallbackMessage = *in.FallbackMessage
	}
	if in.Model != nil {
		b.Model = *in.Model
	}
	if in.Temperature != nil {
		b.Temperature = *in.Temperature
	}
	if in.AllowedOrigins != nil {
		b.AllowedOrigins = pq.StringArray(*in.AllowedOrigins)
	}
}

func (s *Store) UpdateBot(ctx context.Context, owner, id uuid.UUID, in BotInput) (*models.Bot, error) {
	b, err := s.GetBot(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	applyBotInput(b, in)
	b.UpdatedAt = time.Now().UTC()
	err = s.db.WithContext(ctx).Model(b).Select(
		"name", "system_instructions", "fallback_message", "model", "temperature", "allowed_origins", "updated_at",
	).Updates(b).Error
	if err != nil {
		return nil, err
	}
	s.invalidate(b.PublicKey)
	return b, nil
}

// DeleteBot removes a bot and (via cascades) everything below it. It returns the storage
// keys of the bot's documents so the caller can delete the objects.
func (s *Store) DeleteBot(ctx context.Context, owner, id uuid.UUID) ([]string, error) {
	b, err := s.GetBot(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	var keys []string
	if err := s.db.WithContext(ctx).Model(&models.Document{}).Where("bot_id = ? AND storage_key IS NOT NULL", id).
		Pluck("storage_key", &keys).Error; err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Where("id = ? AND owner_id = ?", id, owner).Delete(&models.Bot{}).Error; err != nil {
		return nil, err
	}
	s.invalidate(b.PublicKey)
	return keys, nil
}

// RotatePublicKey issues a new public key, immediately disabling embeds using the old one.
func (s *Store) RotatePublicKey(ctx context.Context, owner, id uuid.UUID) (*models.Bot, error) {
	b, err := s.GetBot(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	old := b.PublicKey
	key, err := NewPublicKey()
	if err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(b).Updates(map[string]any{"public_key": key, "updated_at": time.Now().UTC()}).Error; err != nil {
		return nil, err
	}
	b.PublicKey = key
	s.invalidate(old)
	return b, nil
}

// ---- Widget config ---------------------------------------------------------

func widgetConfigFrom(botID uuid.UUID, w WidgetSettings) models.WidgetConfig {
	mj := func(v any) models.JSONB { b, _ := json.Marshal(v); return models.JSONB(b) }
	return models.WidgetConfig{BotID: botID, Theme: mj(w.Theme), Content: mj(w.Content), Behavior: mj(w.Behavior), Features: mj(w.Features), ConfigVersion: 1}
}

// SettingsFrom decodes the stored jsonb columns over the defaults, so fields added in
// later releases get sensible values for old rows.
func SettingsFrom(wc *models.WidgetConfig, botName string) WidgetSettings {
	w := DefaultWidgetSettings(botName)
	_ = json.Unmarshal(wc.Theme, &w.Theme)
	_ = json.Unmarshal(wc.Content, &w.Content)
	_ = json.Unmarshal(wc.Behavior, &w.Behavior)
	_ = json.Unmarshal(wc.Features, &w.Features)
	return w
}

func (s *Store) GetWidgetConfig(ctx context.Context, owner, botID uuid.UUID) (*models.Bot, *models.WidgetConfig, error) {
	b, err := s.GetBot(ctx, owner, botID)
	if err != nil {
		return nil, nil, err
	}
	var wc models.WidgetConfig
	if err := s.db.WithContext(ctx).Where("bot_id = ?", botID).Take(&wc).Error; err != nil {
		return nil, nil, err
	}
	return b, &wc, nil
}

// UpdateWidgetConfig replaces the settings. If expectedVersion > 0 it must match the
// current version (optimistic concurrency between two open dashboards).
func (s *Store) UpdateWidgetConfig(ctx context.Context, owner, botID uuid.UUID, w WidgetSettings, expectedVersion int) (*models.WidgetConfig, error) {
	b, err := s.GetBot(ctx, owner, botID)
	if err != nil {
		return nil, err
	}
	wc := widgetConfigFrom(botID, w)
	q := s.db.WithContext(ctx).Model(&models.WidgetConfig{}).Where("bot_id = ?", botID)
	if expectedVersion > 0 {
		q = q.Where("config_version = ?", expectedVersion)
	}
	res := q.Updates(map[string]any{
		"theme": wc.Theme, "content": wc.Content, "behavior": wc.Behavior, "features": wc.Features,
		"config_version": gorm.Expr("config_version + 1"), "updated_at": time.Now().UTC(),
	})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrVersionClash
	}
	var out models.WidgetConfig
	if err := s.db.WithContext(ctx).Where("bot_id = ?", botID).Take(&out).Error; err != nil {
		return nil, err
	}
	s.invalidate(b.PublicKey)
	return &out, nil
}

// ---- Public (widget) lookups -----------------------------------------------

// PublicBot is the subset of bot data the public widget endpoints need.
type PublicBot struct {
	Bot      models.Bot
	Settings WidgetSettings
	Version  int
}

func (s *Store) PublicBotByKey(ctx context.Context, key string) (*PublicBot, error) {
	if !ValidPublicKey(key) {
		return nil, ErrNotFound
	}
	now := time.Now()
	s.cacheMu.RLock()
	c, ok := s.cache[key]
	s.cacheMu.RUnlock()
	if ok && now.Before(c.expires) {
		if c.bot == nil {
			return nil, ErrNotFound
		}
		return c.bot, nil
	}
	var b models.Bot
	err := s.db.WithContext(ctx).Where("public_key = ?", key).Take(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		s.put(key, nil, now)
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var wc models.WidgetConfig
	if err := s.db.WithContext(ctx).Where("bot_id = ?", b.ID).Take(&wc).Error; err != nil {
		return nil, err
	}
	pb := &PublicBot{Bot: b, Settings: SettingsFrom(&wc, b.Name), Version: wc.ConfigVersion}
	s.put(key, pb, now)
	return pb, nil
}

func (s *Store) put(key string, pb *PublicBot, now time.Time) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if len(s.cache) > 10000 { // crude bound; entries are tiny and short-lived
		s.cache = map[string]cachedBot{}
	}
	s.cache[key] = cachedBot{bot: pb, expires: now.Add(s.cacheTTL)}
}

func (s *Store) invalidate(key string) {
	s.cacheMu.Lock()
	delete(s.cache, key)
	s.cacheMu.Unlock()
}

// ---- Documents ---------------------------------------------------------------

func (s *Store) ListDocuments(ctx context.Context, owner, botID uuid.UUID) ([]models.Document, error) {
	if _, err := s.GetBot(ctx, owner, botID); err != nil {
		return nil, err
	}
	var docs []models.Document
	err := s.db.WithContext(ctx).Where("bot_id = ?", botID).Order("created_at DESC").Find(&docs).Error
	return docs, err
}

// FindReadyByChecksum returns an existing ready document with identical content, used to
// skip re-ingesting a file that was uploaded before.
func (s *Store) FindByChecksum(ctx context.Context, botID uuid.UUID, checksum string) (*models.Document, error) {
	var d models.Document
	err := s.db.WithContext(ctx).Where("bot_id = ? AND checksum = ? AND status IN ?", botID, checksum,
		[]models.DocumentStatus{models.StatusPending, models.StatusProcessing, models.StatusReady}).
		Order("created_at DESC").Take(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &d, err
}

// CheckDocQuota returns ErrLimitReached if the bot already holds the maximum documents.
func (s *Store) CheckDocQuota(ctx context.Context, botID uuid.UUID) error {
	var n int64
	if err := s.db.WithContext(ctx).Model(&models.Document{}).Where("bot_id = ?", botID).Count(&n).Error; err != nil {
		return err
	}
	if int(n) >= s.maxDocsPerBot {
		return ErrLimitReached
	}
	return nil
}

func (s *Store) CreateDocument(ctx context.Context, d *models.Document) error {
	return s.db.WithContext(ctx).Create(d).Error
}

func (s *Store) GetDocument(ctx context.Context, owner, botID, docID uuid.UUID) (*models.Document, error) {
	var d models.Document
	err := s.db.WithContext(ctx).
		Joins("JOIN bots ON bots.id = documents.bot_id").
		Where("documents.id = ? AND documents.bot_id = ? AND bots.owner_id = ?", docID, botID, owner).
		Take(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &d, err
}

// DeleteDocument removes a document; its chunks go with it via ON DELETE CASCADE. The
// bot's knowledge_version is bumped so cached answers are invalidated.
func (s *Store) DeleteDocument(ctx context.Context, owner, botID, docID uuid.UUID) (*models.Document, error) {
	d, err := s.GetDocument(ctx, owner, botID, docID)
	if err != nil {
		return nil, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", d.ID).Delete(&models.Document{}).Error; err != nil {
			return err
		}
		return tx.Model(&models.Bot{}).Where("id = ?", botID).
			Update("knowledge_version", gorm.Expr("knowledge_version + 1")).Error
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// RequeueDocument resets a document to pending so the worker processes it again (e.g.
// to re-scrape a URL or retry a failure).
func (s *Store) RequeueDocument(ctx context.Context, owner, botID, docID uuid.UUID) (*models.Document, error) {
	d, err := s.GetDocument(ctx, owner, botID, docID)
	if err != nil {
		return nil, err
	}
	if d.Status == models.StatusPending || d.Status == models.StatusProcessing {
		return d, nil
	}
	err = s.db.WithContext(ctx).Model(d).Updates(map[string]any{
		"status": models.StatusPending, "error": nil, "attempts": 0, "locked_at": nil, "updated_at": time.Now().UTC(),
	}).Error
	d.Status = models.StatusPending
	return d, err
}
