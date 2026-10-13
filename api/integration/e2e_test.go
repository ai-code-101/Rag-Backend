//go:build integration

// Package integration runs an end-to-end test across both services:
//
//	signup → create bot → upload document → rag worker ingests it → widget chat
//
// The API runs in-process; the rag service must already be running (see
// scripts/integration.sh) against the same database and storage directory, configured
// with EMBEDDING_PROVIDER=hash and LLM_PROVIDER=fake so no external API is called.
//
// Required env: INTEGRATION_DATABASE_URL, INTEGRATION_RAG_URL, INTEGRATION_RAG_TOKEN,
// INTEGRATION_STORAGE_DIR.
package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ai-code-101/rag-backend/api/internal/auth"
	"github.com/ai-code-101/rag-backend/api/internal/bots"
	"github.com/ai-code-101/rag-backend/api/internal/config"
	"github.com/ai-code-101/rag-backend/api/internal/database"
	"github.com/ai-code-101/rag-backend/api/internal/httpapi"
	"github.com/ai-code-101/rag-backend/api/internal/ragclient"
	"github.com/ai-code-101/rag-backend/api/internal/ratelimit"
	"github.com/ai-code-101/rag-backend/api/internal/storage"
	"github.com/ai-code-101/rag-backend/api/internal/testutil"
)

const knowledge = `# Acme Coffee FAQ

## Opening hours

Our café on Main Street is open Monday to Saturday from 7am to 7pm. On Sundays we open at 9am and close at 3pm.

## Loyalty card

Every tenth coffee is free with the Acme loyalty card. The card is free and available at the counter.
`

func env(t *testing.T, k string) string {
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("%s not set; run scripts/integration.sh", k)
	}
	return v
}

func TestUploadIngestChat(t *testing.T) {
	dsn := env(t, "INTEGRATION_DATABASE_URL")
	ragURL := env(t, "INTEGRATION_RAG_URL")
	ragToken := env(t, "INTEGRATION_RAG_TOKEN")
	storageDir := env(t, "INTEGRATION_STORAGE_DIR")

	db, err := database.Open(t.Context(), dsn, database.Options{MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLife: time.Minute}, testutil.Logger())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Env: "test", WebOrigin: "http://localhost:3000", CookieName: "hv_rt", CookiePath: "/", CookieSameSite: "strict",
		MaxUploadBytes: 5 << 20, MaxTextBytes: 64 << 10, HistoryTurns: 6,
		AllowedModels: []string{"claude-haiku-5-5"}, DefaultModel: "claude-haiku-5-5", MaxBotsPerUser: 5, MaxDocsPerBot: 50,
	}
	svc, err := auth.NewService(auth.Options{
		DB: db, Hasher: auth.NewHasher(auth.DefaultArgon2, 2),
		Tokens:     auth.NewTokenIssuer([]byte(strings.Repeat("i", 32)), "it", 15*time.Minute),
		RefreshTTL: time.Hour, Log: testutil.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	router, err := httpapi.NewRouter(httpapi.Deps{
		Config: cfg, DB: db, Auth: svc, Store: bots.NewStore(db, 5, 50), Objects: &storage.Local{Root: storageDir},
		RAG: ragclient.New(ragURL, ragToken, time.Minute), Limiter: ratelimit.NewMemory(), Log: testutil.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	call := func(method, path, token, ctype string, body []byte, hdr map[string]string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	js := func(v any) []byte { b, _ := json.Marshal(v); return b }

	// 1. Sign up (returns a session straight away).
	email := "owner-" + time.Now().Format("150405.000") + "@example.com"
	st, b := call("POST", "/v1/auth/signup", "", "application/json", js(map[string]string{"email": email, "password": "integration-pass1"}), nil)
	if st != 201 {
		t.Fatalf("signup %d %s", st, b)
	}
	var sess struct {
		AccessToken string `json:"access_token"`
	}
	json.Unmarshal(b, &sess)

	// 2. Create a bot.
	st, b = call("POST", "/v1/bots", sess.AccessToken, "application/json",
		js(map[string]any{"name": "Acme Coffee", "fallback_message": "Please ask our baristas.", "allowed_origins": []string{"https://acme.example"}}), nil)
	if st != 201 {
		t.Fatalf("create bot %d %s", st, b)
	}
	var bot struct {
		ID        string `json:"id"`
		PublicKey string `json:"public_key"`
	}
	json.Unmarshal(b, &bot)

	// 3. Upload a Markdown document.
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	fw, _ := mw.CreateFormFile("file", "faq.md")
	fw.Write([]byte(knowledge))
	mw.Close()
	st, b = call("POST", "/v1/bots/"+bot.ID+"/documents", sess.AccessToken, mw.FormDataContentType(), form.Bytes(), nil)
	if st != 201 {
		t.Fatalf("upload %d %s", st, b)
	}

	// 4. Wait for the rag worker to ingest it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, b = call("GET", "/v1/bots/"+bot.ID+"/documents", sess.AccessToken, "", nil, nil)
		var docs struct {
			Documents []struct {
				Status     string  `json:"status"`
				ChunkCount int     `json:"chunk_count"`
				Error      *string `json:"error"`
			} `json:"documents"`
		}
		json.Unmarshal(b, &docs)
		if len(docs.Documents) == 1 && docs.Documents[0].Status == "ready" {
			if docs.Documents[0].ChunkCount < 1 {
				t.Fatal("ready document without chunks")
			}
			break
		}
		if len(docs.Documents) == 1 && docs.Documents[0].Status == "failed" {
			t.Fatalf("ingestion failed: %v", *docs.Documents[0].Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("document not ingested in time: %s", b)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 5. Chat through the public widget endpoint from an allowed origin.
	visitor := "v_integration_" + strings.Repeat("x", 10)
	st, b = call("POST", "/v1/widget/"+bot.PublicKey+"/chat", "", "application/json",
		js(map[string]string{"visitor_id": visitor, "message": "What time do you open on Sundays?"}),
		map[string]string{"Origin": "https://acme.example"})
	if st != 200 {
		t.Fatalf("chat %d %s", st, b)
	}
	answer, sources := readStream(t, b)
	if !strings.Contains(answer, "Sundays we open at 9am") {
		t.Fatalf("answer not grounded in the uploaded document: %q", answer)
	}
	if !strings.Contains(sources, "Opening hours") {
		t.Fatalf("sources missing: %s", sources)
	}

	// 6. Usage was recorded for the assistant message.
	var tokens int
	db.Raw("SELECT coalesce(sum(input_tokens),0) FROM messages WHERE bot_id = ? AND role = 'assistant'", bot.ID).Scan(&tokens)
	if tokens == 0 {
		t.Fatal("token usage not recorded")
	}

	// 7. An unrelated question gets the bot's fallback instead of invented facts.
	_, b = call("POST", "/v1/widget/"+bot.PublicKey+"/chat", "", "application/json",
		js(map[string]string{"visitor_id": visitor, "message": "Do you repair motorcycles?"}),
		map[string]string{"Origin": "https://acme.example"})
	if fallback, _ := readStream(t, b); strings.TrimSpace(fallback) != "Please ask our baristas." {
		t.Fatalf("fallback not used: %q", fallback)
	}
}

// readStream concatenates the streamed tokens and returns them with the sources payload.
func readStream(t *testing.T, body []byte) (string, string) {
	var answer strings.Builder
	var sources string
	ragclient.ParseSSE(bytes.NewReader(body), func(ev ragclient.Event) error {
		switch ev.Name {
		case "token":
			var tok struct {
				T string `json:"t"`
			}
			json.Unmarshal(ev.Data, &tok)
			answer.WriteString(tok.T)
		case "sources":
			sources = string(ev.Data)
		case "error":
			t.Errorf("stream error: %s", ev.Data)
		}
		return nil
	})
	return answer.String(), sources
}
