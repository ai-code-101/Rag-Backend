package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ai-code-101/rag-backend/api/internal/models"
)

var unsafeFilename = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeFilename keeps a readable, storage-safe base name.
func sanitizeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = unsafeFilename.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = "document"
	}
	if len(name) > 120 {
		ext := path.Ext(name)
		if len(ext) > 10 {
			ext = ""
		}
		name = name[:120-len(ext)] + ext
	}
	return name
}

// detectSource maps the extension to a source type and verifies the content signature,
// so a renamed executable cannot be ingested as a PDF.
func detectSource(filename string, head []byte) (models.SourceType, string, error) {
	switch strings.ToLower(path.Ext(filename)) {
	case ".pdf":
		if !bytes.HasPrefix(head, []byte("%PDF-")) {
			return "", "", errors.New("file is not a valid PDF")
		}
		return models.SourcePDF, "application/pdf", nil
	case ".docx":
		if !bytes.HasPrefix(head, []byte("PK\x03\x04")) {
			return "", "", errors.New("file is not a valid DOCX")
		}
		return models.SourceDOCX, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", nil
	case ".txt", ".text":
		if !looksLikeText(head) {
			return "", "", errors.New("file is not UTF-8 text")
		}
		return models.SourceTXT, "text/plain; charset=utf-8", nil
	case ".md", ".markdown", ".mdx":
		if !looksLikeText(head) {
			return "", "", errors.New("file is not UTF-8 text")
		}
		return models.SourceMD, "text/markdown; charset=utf-8", nil
	}
	return "", "", errors.New("unsupported file type; upload PDF, DOCX, TXT or Markdown")
}

func looksLikeText(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	// The sniff window may cut a multi-byte rune; trim up to 3 trailing bytes before checking.
	for i := 0; i < 4 && len(b) > 0; i++ {
		if utf8.Valid(b) {
			return true
		}
		b = b[:len(b)-1]
	}
	return utf8.Valid(b)
}

func (h *Handler) listDocuments(c *gin.Context) {
	botID, ok := parseID(c, "id")
	if !ok {
		return
	}
	docs, err := h.store.ListDocuments(c.Request.Context(), userID(c), botID)
	if err != nil {
		h.failStore(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"documents": docs})
}

// uploadDocument streams a multipart file straight to object storage (never buffering the
// whole file in memory), computing its checksum on the way.
func (h *Handler) uploadDocument(c *gin.Context) {
	botID, ok := parseID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if _, err := h.store.GetBot(ctx, userID(c), botID); err != nil {
		h.failStore(c, err)
		return
	}
	if err := h.store.CheckDocQuota(ctx, botID); err != nil {
		h.failStore(c, err)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.cfg.MaxUploadBytes+64<<10)
	mr, err := c.Request.MultipartReader()
	if err != nil {
		fail(c, http.StatusBadRequest, "bad_request", "Expected multipart/form-data.")
		return
	}
	var part interface {
		io.Reader
		FileName() string
		Close() error
	}
	for {
		p, err := mr.NextPart()
		if err != nil {
			fail(c, http.StatusBadRequest, "bad_request", "Missing file field.")
			return
		}
		if p.FormName() == "file" && p.FileName() != "" {
			part = p
			break
		}
		p.Close()
	}
	defer part.Close()

	filename := sanitizeFilename(part.FileName())
	br := bufio.NewReaderSize(io.LimitReader(part, h.cfg.MaxUploadBytes+1), 4096)
	head, _ := br.Peek(512)
	src, mime, err := detectSource(filename, head)
	if err != nil {
		fail(c, http.StatusUnsupportedMediaType, "unsupported_file", err.Error())
		return
	}
	docID := uuid.New()
	key := "bots/" + botID.String() + "/" + docID.String() + "/" + filename
	res, err := h.objects.Put(ctx, key, br, mime)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(c, http.StatusRequestEntityTooLarge, "file_too_large", "File is too large.")
			return
		}
		h.failInternal(c, err)
		return
	}
	if res.Size > h.cfg.MaxUploadBytes {
		h.deleteObjects([]string{key})
		fail(c, http.StatusRequestEntityTooLarge, "file_too_large", "File is too large.")
		return
	}
	if res.Size == 0 {
		h.deleteObjects([]string{key})
		fail(c, http.StatusUnprocessableEntity, "empty_file", "File is empty.")
		return
	}
	h.finishDocument(c, &models.Document{
		ID: docID, BotID: botID, Filename: filename, SourceType: src, StorageKey: &key,
		MimeType: mime, SizeBytes: res.Size, Checksum: &res.Checksum, Status: models.StatusPending,
	})
}

type urlReq struct {
	URL string `json:"url"`
}

// validatePublicURL performs a syntactic check. The rag service re-validates at fetch time
// after DNS resolution (SSRF protection), so this is a fast first line of defence.
func validatePublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || len(raw) > 2048 {
		return nil, errors.New("enter a valid http(s) URL")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return nil, errors.New("URL must be publicly reachable")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsLinkLocalMulticast()) {
		return nil, errors.New("URL must be publicly reachable")
	}
	u.Fragment = ""
	return u, nil
}

func (h *Handler) addURL(c *gin.Context) {
	botID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req urlReq
	if !bindJSON(c, &req) {
		return
	}
	u, err := validatePublicURL(req.URL)
	if err != nil {
		fail(c, http.StatusUnprocessableEntity, "validation_error", err.Error())
		return
	}
	ctx := c.Request.Context()
	if _, err := h.store.GetBot(ctx, userID(c), botID); err != nil {
		h.failStore(c, err)
		return
	}
	if err := h.store.CheckDocQuota(ctx, botID); err != nil {
		h.failStore(c, err)
		return
	}
	s := u.String()
	name := u.Host + u.EscapedPath()
	if len(name) > 200 {
		name = name[:200]
	}
	h.finishDocument(c, &models.Document{
		ID: uuid.New(), BotID: botID, Filename: name, SourceType: models.SourceURL, SourceURL: &s,
		MimeType: "text/html", Status: models.StatusPending,
	})
}

type textReq struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

func (h *Handler) addText(c *gin.Context) {
	botID, ok := parseID(c, "id")
	if !ok {
		return
	}
	var req textReq
	if !bindJSON(c, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		req.Title = "Pasted text"
	}
	if utf8.RuneCountInString(req.Title) > 120 {
		fail(c, http.StatusUnprocessableEntity, "validation_error", "Title is too long.")
		return
	}
	if strings.TrimSpace(req.Text) == "" || int64(len(req.Text)) > h.cfg.MaxTextBytes {
		fail(c, http.StatusUnprocessableEntity, "validation_error", "Text must be non-empty and within the size limit.")
		return
	}
	ctx := c.Request.Context()
	if _, err := h.store.GetBot(ctx, userID(c), botID); err != nil {
		h.failStore(c, err)
		return
	}
	if err := h.store.CheckDocQuota(ctx, botID); err != nil {
		h.failStore(c, err)
		return
	}
	docID := uuid.New()
	key := "bots/" + botID.String() + "/" + docID.String() + "/text.md"
	// Store the title as a heading so the chunker records it as the section title.
	body := "# " + req.Title + "\n\n" + req.Text
	res, err := h.objects.Put(ctx, key, strings.NewReader(body), "text/markdown; charset=utf-8")
	if err != nil {
		h.failInternal(c, err)
		return
	}
	h.finishDocument(c, &models.Document{
		ID: docID, BotID: botID, Filename: req.Title, SourceType: models.SourceText, StorageKey: &key,
		MimeType: "text/markdown", SizeBytes: res.Size, Checksum: &res.Checksum, Status: models.StatusPending,
	})
}

// finishDocument de-duplicates by checksum, inserts the row and pokes the rag worker.
func (h *Handler) finishDocument(c *gin.Context, d *models.Document) {
	ctx := c.Request.Context()
	if d.Checksum != nil {
		existing, err := h.store.FindByChecksum(ctx, d.BotID, *d.Checksum)
		if err != nil {
			h.failInternal(c, err)
			return
		}
		if existing != nil {
			if d.StorageKey != nil {
				h.deleteObjects([]string{*d.StorageKey})
			}
			c.JSON(http.StatusOK, gin.H{"document": existing, "duplicate": true})
			return
		}
	}
	if err := h.store.CreateDocument(ctx, d); err != nil {
		h.failInternal(c, err)
		return
	}
	h.notifyIngest(d.ID)
	c.JSON(http.StatusCreated, gin.H{"document": d, "duplicate": false})
}

func (h *Handler) notifyIngest(id uuid.UUID) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.rag.NotifyIngest(ctx, id.String()); err != nil {
			h.log.Warn("notify rag ingest (worker will poll)", "document_id", id, "error", err)
		}
	}()
}

func (h *Handler) deleteDocument(c *gin.Context) {
	botID, ok := parseID(c, "id")
	if !ok {
		return
	}
	docID, ok := parseID(c, "docId")
	if !ok {
		return
	}
	d, err := h.store.DeleteDocument(c.Request.Context(), userID(c), botID, docID)
	if err != nil {
		h.failStore(c, err)
		return
	}
	if d.StorageKey != nil {
		h.deleteObjects([]string{*d.StorageKey})
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) reingestDocument(c *gin.Context) {
	botID, ok := parseID(c, "id")
	if !ok {
		return
	}
	docID, ok := parseID(c, "docId")
	if !ok {
		return
	}
	d, err := h.store.RequeueDocument(c.Request.Context(), userID(c), botID, docID)
	if err != nil {
		h.failStore(c, err)
		return
	}
	h.notifyIngest(d.ID)
	c.JSON(http.StatusAccepted, gin.H{"document": d})
}
