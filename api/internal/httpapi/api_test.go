package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/ai-code-101/rag-backend/api/internal/auth"
	"github.com/ai-code-101/rag-backend/api/internal/bots"
	"github.com/ai-code-101/rag-backend/api/internal/config"
	"github.com/ai-code-101/rag-backend/api/internal/httpapi"
	pcmail "github.com/ai-code-101/rag-backend/api/internal/mail"
	"github.com/ai-code-101/rag-backend/api/internal/models"
	"github.com/ai-code-101/rag-backend/api/internal/ragclient"
	"github.com/ai-code-101/rag-backend/api/internal/ratelimit"
	"github.com/ai-code-101/rag-backend/api/internal/storage"
	"github.com/ai-code-101/rag-backend/api/internal/testutil"
)

const webOrigin = "https://app.peakchat.test"

type mailbox struct {
	mu   sync.Mutex
	msgs []pcmail.Message
}

func (m *mailbox) Enqueue(msg pcmail.Message) error {
	m.mu.Lock()
	m.msgs = append(m.msgs, msg)
	m.mu.Unlock()
	return nil
}

func (m *mailbox) code(t *testing.T) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.msgs) == 0 {
		t.Fatal("no mail")
	}
	return regexp.MustCompile(`\b\d{6}\b`).FindString(m.msgs[len(m.msgs)-1].Text)
}

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	db     *gorm.DB
	mail   *mailbox
	ragReq chan ragclient.ChatRequest
}

func newHarness(t *testing.T) *harness {
	db, _ := testutil.NewDB(t)
	h := &harness{t: t, db: db, mail: &mailbox{}, ragReq: make(chan ragclient.ChatRequest, 10)}

	// Fake rag service: echoes a fixed answer as SSE and reports usage.
	rag := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-internal-token-123456789" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/internal/chat":
			var req ragclient.ChatRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			h.ragReq <- req
			w.Header().Set("Content-Type", "text/event-stream")
			for _, tok := range []string{"Hello", " there"} {
				fmt.Fprintf(w, "event: token\ndata: {\"t\":%q}\n\n", tok)
				w.(http.Flusher).Flush()
			}
			fmt.Fprint(w, "event: sources\ndata: [{\"document_id\":\"d\",\"title\":\"FAQ\"}]\n\n")
			fmt.Fprint(w, "event: done\ndata: {\"input_tokens\":120,\"output_tokens\":7,\"cached_input_tokens\":100,\"model\":\"claude-haiku-5-5\"}\n\n")
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(rag.Close)

	cfg := &config.Config{
		Env: "test", WebOrigin: webOrigin, CookieName: "__Secure-pc_rt", CookiePath: "/api/v1/auth",
		CookieSecure: true, CookieSameSite: "strict", MaxUploadBytes: 1 << 20, MaxTextBytes: 64 << 10,
		HistoryTurns: 6, AllowedModels: []string{"claude-opus-5-5", "claude-haiku-5-5"}, DefaultModel: "claude-opus-5-5",
		MaxBotsPerUser: 3, MaxDocsPerBot: 10,
	}
	svc, err := auth.NewService(auth.Options{
		DB: db, Hasher: auth.NewHasher(auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 4),
		Tokens:    auth.NewTokenIssuer([]byte(strings.Repeat("j", 32)), "test", 15*time.Minute),
		OTPPepper: []byte(strings.Repeat("p", 32)), Mailer: h.mail, AppName: "PeakChat", RefreshTTL: time.Hour, Log: testutil.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	router, err := httpapi.NewRouter(httpapi.Deps{
		Config: cfg, DB: db, Auth: svc, Store: bots.NewStore(db, cfg.MaxBotsPerUser, cfg.MaxDocsPerBot),
		Objects: &storage.Local{Root: t.TempDir()}, RAG: ragclient.New(rag.URL, "test-internal-token-123456789", 10*time.Second),
		Limiter: ratelimit.NewMemory(), Log: testutil.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(router)
	t.Cleanup(h.srv.Close)
	return h
}

type resp struct {
	status int
	body   []byte
	header http.Header
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (h *harness) do(method, path, token string, body any, hdr map[string]string) resp {
	h.t.Helper()
	var rdr io.Reader
	if b, ok := body.([]byte); ok {
		rdr = bytes.NewReader(b)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, body: b, header: res.Header}
}

// account signs up and verifies a user and returns its access token.
func (h *harness) account(email string) string {
	h.t.Helper()
	creds := map[string]string{"email": email, "password": "password-123"}
	if r := h.do("POST", "/v1/auth/signup", "", creds, nil); r.status != http.StatusAccepted {
		h.t.Fatalf("signup: %d %s", r.status, r.body)
	}
	r := h.do("POST", "/v1/auth/verify-email", "", map[string]string{"email": email, "password": "password-123", "code": h.mail.code(h.t)}, nil)
	if r.status != http.StatusOK {
		h.t.Fatalf("verify: %d %s", r.status, r.body)
	}
	var s struct {
		AccessToken string `json:"access_token"`
	}
	r.json(h.t, &s)
	return s.AccessToken
}

func (h *harness) createBot(token string, origins []string) map[string]any {
	h.t.Helper()
	r := h.do("POST", "/v1/bots", token, map[string]any{"name": "Support", "allowed_origins": origins}, nil)
	if r.status != http.StatusCreated {
		h.t.Fatalf("create bot: %d %s", r.status, r.body)
	}
	var b map[string]any
	r.json(h.t, &b)
	return b
}

func TestAuthHTTPFlow(t *testing.T) {
	h := newHarness(t)
	// Signup responses are identical for new and existing addresses.
	first := h.do("POST", "/v1/auth/signup", "", map[string]string{"email": "a@example.com", "password": "password-123"}, nil)
	h.account("b@example.com")
	again := h.do("POST", "/v1/auth/signup", "", map[string]string{"email": "b@example.com", "password": "password-123"}, nil)
	if first.status != again.status || string(first.body) != string(again.body) {
		t.Fatalf("signup leaks account existence: %s vs %s", first.body, again.body)
	}
	// Login for the unverified account returns verification_required and no cookie.
	r := h.do("POST", "/v1/auth/login", "", map[string]string{"email": "a@example.com", "password": "password-123"}, nil)
	if r.status != http.StatusAccepted || r.header.Get("Set-Cookie") != "" {
		t.Fatalf("unverified login: %d %s", r.status, r.body)
	}
	bad := h.do("POST", "/v1/auth/login", "", map[string]string{"email": "nobody@example.com", "password": "password-123"}, nil)
	wrong := h.do("POST", "/v1/auth/login", "", map[string]string{"email": "b@example.com", "password": "nope-12345"}, nil)
	if bad.status != http.StatusUnauthorized || string(bad.body) != string(wrong.body) {
		t.Fatalf("login errors must be generic: %s / %s", bad.body, wrong.body)
	}

	r = h.do("POST", "/v1/auth/login", "", map[string]string{"email": "b@example.com", "password": "password-123"}, nil)
	if r.status != http.StatusOK {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	cookie := r.header.Get("Set-Cookie")
	for _, attr := range []string{"__Secure-pc_rt=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/api/v1/auth"} {
		if !strings.Contains(cookie, attr) {
			t.Errorf("refresh cookie missing %q: %s", attr, cookie)
		}
	}
	rt := strings.SplitN(strings.TrimPrefix(cookie, "__Secure-pc_rt="), ";", 2)[0]

	// Refresh requires the CSRF header.
	if r := h.do("POST", "/v1/auth/refresh", "", nil, map[string]string{"Cookie": "__Secure-pc_rt=" + rt}); r.status != http.StatusForbidden {
		t.Fatalf("refresh without CSRF header: %d", r.status)
	}
	if r := h.do("POST", "/v1/auth/refresh", "", nil, map[string]string{"Cookie": "__Secure-pc_rt=" + rt, "X-Requested-With": "peakchat", "Origin": "https://evil.example"}); r.status != http.StatusForbidden {
		t.Fatalf("refresh from foreign origin: %d", r.status)
	}
	r = h.do("POST", "/v1/auth/refresh", "", nil, map[string]string{"Cookie": "__Secure-pc_rt=" + rt, "X-Requested-With": "peakchat", "Origin": webOrigin})
	if r.status != http.StatusOK {
		t.Fatalf("refresh: %d %s", r.status, r.body)
	}
	var s struct {
		AccessToken string `json:"access_token"`
	}
	r.json(t, &s)
	if me := h.do("GET", "/v1/me", s.AccessToken, nil, nil); me.status != http.StatusOK || !strings.Contains(string(me.body), "b@example.com") {
		t.Fatalf("me: %d %s", me.status, me.body)
	}
	if me := h.do("GET", "/v1/me", "garbage", nil, nil); me.status != http.StatusUnauthorized {
		t.Fatalf("bad token accepted: %d", me.status)
	}
}

func TestUnverifiedTokenCannotCreateBots(t *testing.T) {
	h := newHarness(t)
	tok, _ := auth.NewTokenIssuer([]byte(strings.Repeat("j", 32)), "test", time.Minute).Issue(uuid.New(), false, time.Now())
	if r := h.do("POST", "/v1/bots", tok, map[string]any{"name": "x"}, nil); r.status != http.StatusForbidden {
		t.Fatalf("expected 403 for unverified token, got %d", r.status)
	}
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.account("alice@example.com"), h.account("bob@example.com")
	bot := h.createBot(alice, nil)
	id := bot["id"].(string)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/bots/" + id}, {"PATCH", "/v1/bots/" + id}, {"DELETE", "/v1/bots/" + id},
		{"GET", "/v1/bots/" + id + "/widget"}, {"GET", "/v1/bots/" + id + "/documents"},
		{"POST", "/v1/bots/" + id + "/documents/text"}, {"POST", "/v1/bots/" + id + "/chat"},
		{"GET", "/v1/bots/" + id + "/leads"}, {"POST", "/v1/bots/" + id + "/rotate-key"},
	} {
		var body any
		if tc.method == "PATCH" {
			body = map[string]any{"name": "pwned"}
		} else if strings.HasSuffix(tc.path, "/text") {
			body = map[string]any{"title": "x", "text": "y"}
		} else if strings.HasSuffix(tc.path, "/chat") {
			body = map[string]any{"message": "hi"}
		}
		if r := h.do(tc.method, tc.path, bob, body, nil); r.status != http.StatusNotFound {
			t.Errorf("%s %s as another tenant: got %d, want 404", tc.method, tc.path, r.status)
		}
	}
	if r := h.do("GET", "/v1/bots", bob, nil, nil); strings.Contains(string(r.body), id) {
		t.Fatal("bot leaked into another tenant's list")
	}
	if r := h.do("GET", "/v1/bots/"+id, alice, nil, nil); r.status != http.StatusOK {
		t.Fatalf("owner cannot read own bot: %d", r.status)
	}
}

func TestBotLimitAndValidation(t *testing.T) {
	h := newHarness(t)
	tok := h.account("limit@example.com")
	if r := h.do("POST", "/v1/bots", tok, map[string]any{"name": "x", "model": "gpt-9"}, nil); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid model accepted: %d", r.status)
	}
	if r := h.do("POST", "/v1/bots", tok, map[string]any{"name": "x", "unknown_field": 1}, nil); r.status != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", r.status)
	}
	for i := 0; i < 3; i++ {
		h.createBot(tok, nil)
	}
	if r := h.do("POST", "/v1/bots", tok, map[string]any{"name": "x"}, nil); r.status != http.StatusForbidden {
		t.Fatalf("bot limit not enforced: %d", r.status)
	}
}

func multipartFile(name string, content []byte) ([]byte, string) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", name)
	fw.Write(content)
	w.Close()
	return buf.Bytes(), w.FormDataContentType()
}

func TestDocumentUpload(t *testing.T) {
	h := newHarness(t)
	tok := h.account("docs@example.com")
	id := h.createBot(tok, nil)["id"].(string)

	body, ct := multipartFile("fake.pdf", []byte("MZ this is an exe"))
	if r := h.do("POST", "/v1/bots/"+id+"/documents", tok, body, map[string]string{"Content-Type": ct}); r.status != http.StatusUnsupportedMediaType {
		t.Fatalf("fake pdf accepted: %d %s", r.status, r.body)
	}
	body, ct = multipartFile("../../etc/notes.md", []byte("# Hours\n\nWe open at 9am."))
	r := h.do("POST", "/v1/bots/"+id+"/documents", tok, body, map[string]string{"Content-Type": ct})
	if r.status != http.StatusCreated {
		t.Fatalf("upload: %d %s", r.status, r.body)
	}
	var out struct {
		Document  models.Document `json:"document"`
		Duplicate bool            `json:"duplicate"`
	}
	r.json(t, &out)
	if out.Document.Filename != "notes.md" || out.Document.SourceType != models.SourceMD || out.Document.Status != models.StatusPending || out.Document.Checksum == nil {
		t.Fatalf("unexpected document: %+v", out.Document)
	}
	// Same bytes again: deduplicated by checksum, no re-ingestion.
	body, ct = multipartFile("copy.md", []byte("# Hours\n\nWe open at 9am."))
	r = h.do("POST", "/v1/bots/"+id+"/documents", tok, body, map[string]string{"Content-Type": ct})
	r.json(t, &out)
	if r.status != http.StatusOK || !out.Duplicate {
		t.Fatalf("duplicate not detected: %d %s", r.status, r.body)
	}
	if r := h.do("POST", "/v1/bots/"+id+"/documents/url", tok, map[string]string{"url": "http://169.254.169.254/latest"}, nil); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("metadata URL accepted: %d", r.status)
	}
	if r := h.do("POST", "/v1/bots/"+id+"/documents/url", tok, map[string]string{"url": "https://example.com/faq"}, nil); r.status != http.StatusCreated {
		t.Fatalf("url: %d %s", r.status, r.body)
	}
	if r := h.do("DELETE", "/v1/bots/"+id+"/documents/"+out.Document.ID.String(), tok, nil, nil); r.status != http.StatusNoContent {
		t.Fatalf("delete: %d", r.status)
	}
	var kv int64
	h.db.Model(&models.Bot{}).Select("knowledge_version").Where("id = ?", id).Scan(&kv)
	if kv != 1 {
		t.Fatalf("knowledge_version not bumped on delete: %d", kv)
	}
}

func TestWidgetOriginAndChat(t *testing.T) {
	h := newHarness(t)
	tok := h.account("widget@example.com")
	bot := h.createBot(tok, []string{"https://shop.example.com"})
	key := bot["public_key"].(string)

	if r := h.do("GET", "/v1/widget/"+key+"/config", "", nil, map[string]string{"Origin": "https://evil.example"}); r.status != http.StatusForbidden {
		t.Fatalf("foreign origin got config: %d", r.status)
	}
	r := h.do("GET", "/v1/widget/"+key+"/config", "", nil, map[string]string{"Origin": "https://shop.example.com"})
	if r.status != http.StatusOK || r.header.Get("Access-Control-Allow-Origin") != "https://shop.example.com" || r.header.Get("ETag") == "" {
		t.Fatalf("config: %d %v", r.status, r.header)
	}
	if r2 := h.do("GET", "/v1/widget/"+key+"/config", "", nil, map[string]string{"Origin": "https://shop.example.com", "If-None-Match": r.header.Get("ETag")}); r2.status != http.StatusNotModified {
		t.Fatalf("ETag not honoured: %d", r2.status)
	}
	if r := h.do("GET", "/v1/widget/pk_doesnotexistdoesnotexist/config", "", nil, nil); r.status != http.StatusNotFound {
		t.Fatalf("unknown key: %d", r.status)
	}

	visitor := "v_" + strings.Repeat("a", 20)
	chat := map[string]any{"visitor_id": visitor, "message": "When do you open?"}
	if r := h.do("POST", "/v1/widget/"+key+"/chat", "", chat, nil); r.status != http.StatusForbidden {
		t.Fatalf("chat without Origin accepted: %d", r.status)
	}
	// The iframe (web origin) may always chat.
	r = h.do("POST", "/v1/widget/"+key+"/chat", "", chat, map[string]string{"Origin": webOrigin})
	if r.status != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("chat: %d %s", r.status, r.body)
	}
	stream := string(r.body)
	for _, want := range []string{"event: meta", `"t":"Hello"`, "event: sources", "event: done"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream missing %q:\n%s", want, stream)
		}
	}
	req := <-h.ragReq
	if req.Message != "When do you open?" || req.BotID != bot["id"] || req.Model != "claude-opus-5-5" {
		t.Fatalf("unexpected rag request: %+v", req)
	}

	var msgs []models.Message
	h.db.Order("created_at").Find(&msgs)
	if len(msgs) != 2 || msgs[1].Role != "assistant" || msgs[1].Content != "Hello there" ||
		msgs[1].InputTokens != 120 || msgs[1].OutputTokens != 7 || msgs[1].CachedInputTokens != 100 {
		t.Fatalf("messages/usage not recorded: %+v", msgs)
	}

	// Second turn on the same conversation sends history; a foreign visitor cannot read it.
	var meta struct {
		ConversationID string `json:"conversation_id"`
	}
	line := regexp.MustCompile(`event: meta\ndata: (.*)`).FindStringSubmatch(stream)
	json.Unmarshal([]byte(line[1]), &meta)
	chat["conversation_id"] = meta.ConversationID
	h.do("POST", "/v1/widget/"+key+"/chat", "", chat, map[string]string{"Origin": "https://shop.example.com"})
	if req := <-h.ragReq; len(req.History) != 2 {
		t.Fatalf("history not sent: %+v", req.History)
	}
	hist := h.do("GET", "/v1/widget/"+key+"/conversations/"+meta.ConversationID+"?visitor_id="+visitor, "", nil, map[string]string{"Origin": webOrigin})
	if !strings.Contains(string(hist.body), "Hello there") {
		t.Fatalf("history restore failed: %s", hist.body)
	}
	other := h.do("GET", "/v1/widget/"+key+"/conversations/"+meta.ConversationID+"?visitor_id=v_"+strings.Repeat("b", 20), "", nil, map[string]string{"Origin": webOrigin})
	if strings.Contains(string(other.body), "Hello there") {
		t.Fatal("another visitor read the conversation")
	}
}
