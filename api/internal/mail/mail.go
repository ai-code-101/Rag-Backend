// Package mail sends transactional email through a provider-agnostic Sender.
package mail

import (
	"context"
	"log/slog"
	"strings"
)

// Message is a rendered email ready to send.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers a message. Implementations: SMTPSender (Gmail etc.), LogSender (dev).
// SendGrid, SES and others only need to implement this interface.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// LogSender writes emails to the log instead of sending them. Development only:
// config validation refuses it in production.
type LogSender struct{ Log *slog.Logger }

func (s LogSender) Send(_ context.Context, msg Message) error {
	s.Log.Info("email (log driver)", "to", msg.To, "subject", msg.Subject, "body", strings.TrimSpace(msg.Text))
	return nil
}
