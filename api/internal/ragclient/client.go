// Package ragclient talks to the internal Python rag service.
package ragclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client // short requests
	stream  *http.Client // long-lived SSE streams (bounded by the timeout and ctx)
	idToken oauth2.TokenSource
}

// WithIDToken makes the client also send a Google-signed ID token for the given audience
// (the rag service URL) in X-Serverless-Authorization, so rag can be deployed with
// Cloud Run IAM (--no-allow-unauthenticated) in addition to the shared bearer token.
func (c *Client) WithIDToken(ctx context.Context, audience string) error {
	ts, err := idtoken.NewTokenSource(ctx, audience)
	if err != nil {
		return fmt.Errorf("id token source: %w", err)
	}
	c.idToken = ts
	return nil
}

func New(baseURL, token string, timeout time.Duration) *Client {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 50,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second, Transport: transport},
		stream:  &http.Client{Transport: transport, Timeout: timeout},
	}
}

type HistoryTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	BotID              string        `json:"bot_id"`
	KnowledgeVersion   int64         `json:"knowledge_version"`
	SystemInstructions string        `json:"system_instructions"`
	FallbackMessage    string        `json:"fallback_message"`
	Model              string        `json:"model"`
	Temperature        float32       `json:"temperature"`
	History            []HistoryTurn `json:"history"`
	Message            string        `json:"message"`
}

type Usage struct {
	InputTokens       int    `json:"input_tokens"`
	OutputTokens      int    `json:"output_tokens"`
	CachedInputTokens int    `json:"cached_input_tokens"`
	Model             string `json:"model"`
	Cached            bool   `json:"cached"`
}

// Event is one SSE event from the rag service.
type Event struct {
	Name string
	Data json.RawMessage
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.idToken != nil {
		tok, err := c.idToken.Token()
		if err != nil {
			return nil, fmt.Errorf("id token: %w", err)
		}
		req.Header.Set("X-Serverless-Authorization", "Bearer "+tok.AccessToken)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("rag %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// NotifyIngest asks the rag worker to pick up a document now. Failure is non-fatal: the
// worker also polls for pending documents.
func (c *Client) NotifyIngest(ctx context.Context, documentID string) error {
	resp, err := c.do(ctx, c.http, http.MethodPost, "/internal/ingest/"+documentID, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) Health(ctx context.Context) error {
	resp, err := c.do(ctx, c.http, http.MethodGet, "/healthz", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ChatStream starts a streamed chat completion and calls onEvent for each SSE event in
// order. It returns when the stream ends, onEvent returns an error, or ctx is cancelled.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, onEvent func(Event) error) error {
	resp, err := c.do(ctx, c.stream, http.MethodPost, "/internal/chat", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return ParseSSE(resp.Body, onEvent)
}

// ParseSSE reads a text/event-stream body. Only the "event" and "data" fields are used.
func ParseSSE(r io.Reader, onEvent func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var name string
	var data bytes.Buffer
	flush := func() error {
		if data.Len() == 0 {
			name = ""
			return nil
		}
		// Copy: data's backing array is reused for the next event.
		payload := bytes.Clone(bytes.TrimSuffix(data.Bytes(), []byte("\n")))
		ev := Event{Name: name, Data: json.RawMessage(payload)}
		if ev.Name == "" {
			ev.Name = "message"
		}
		name = ""
		data.Reset()
		return onEvent(ev)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(line[len("data:"):], " "))
			data.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return flush()
}
