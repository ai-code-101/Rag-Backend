package mail

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-code-101/rag-backend/api/internal/models"
)

func TestOTPEmail(t *testing.T) {
	for _, p := range []models.OTPPurpose{models.PurposeVerifyEmail, models.PurposeLogin, models.PurposeResetPassword} {
		m, err := OTPEmail("PeakChat", "a@example.com", p, "123456", 10)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(m.Text, "123456") || !strings.Contains(m.HTML, "123456") || m.Subject == "" {
			t.Fatalf("%s: code missing from email: %+v", p, m)
		}
	}
	if _, err := OTPEmail("PeakChat", "a@example.com", "bogus", "1", 10); err == nil {
		t.Fatal("unknown purpose should fail")
	}
}

func TestMIMEBuild(t *testing.T) {
	s := &SMTPSender{From: "roadyridge@gmail.com", FromName: "PeakChat"}
	b, err := s.build(Message{To: "a@example.com", Subject: "Hello ✓", Text: "plain body", HTML: "<p>html body</p>"})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	for _, want := range []string{"From: \"PeakChat\" <roadyridge@gmail.com>", "multipart/alternative", "text/plain; charset=utf-8", "text/html; charset=utf-8", "plain body", "=?utf-8?q?"} {
		if !strings.Contains(raw, want) {
			t.Errorf("message missing %q", want)
		}
	}
	if strings.Index(raw, "text/plain") > strings.Index(raw, "text/html") {
		t.Error("plain-text part must come first (clients prefer the last alternative)")
	}
}

func TestSMTPRejectsHeaderInjection(t *testing.T) {
	s := &SMTPSender{Host: "127.0.0.1", Port: 1}
	err := s.Send(context.Background(), Message{To: "a@example.com\r\nBcc: x@evil.com", Subject: "x"})
	if err == nil || !strings.Contains(err.Error(), "injection") {
		t.Fatalf("expected injection error, got %v", err)
	}
}

type flaky struct{ calls atomic.Int32 }

func (f *flaky) Send(context.Context, Message) error {
	if f.calls.Add(1) < 2 {
		return errors.New("temporary")
	}
	return nil
}

func TestQueueRetriesAndDrains(t *testing.T) {
	f := &flaky{}
	q := NewQueue(f, slog.New(slog.NewTextHandler(io.Discard, nil)), 1, 10)
	q.backoff = time.Millisecond
	if err := q.Enqueue(Message{To: "a@example.com", Subject: "s"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 2 {
		t.Fatalf("expected 1 retry, got %d calls", f.calls.Load())
	}
	if err := q.Enqueue(Message{}); err == nil {
		t.Fatal("enqueue after close should fail")
	}
}
