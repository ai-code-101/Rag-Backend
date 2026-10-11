package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// SMTPSender sends mail over SMTP with mandatory STARTTLS (e.g. smtp.gmail.com:587 with a
// Google App Password).
type SMTPSender struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string // address, e.g. roadyridge@gmail.com
	FromName string
	Timeout  time.Duration
}

func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if strings.ContainsAny(msg.To, "\r\n") || strings.ContainsAny(msg.Subject, "\r\n") {
		return errors.New("mail: header injection attempt")
	}
	if _, err := mail.ParseAddress(msg.To); err != nil {
		return fmt.Errorf("mail: invalid recipient: %w", err)
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mail: dial: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: client: %w", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New("mail: server does not support STARTTLS")
	}
	if err := c.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
		return fmt.Errorf("mail: starttls: %w", err)
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(msg.To); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	body, err := s.build(msg)
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: close data: %w", err)
	}
	return c.Quit()
}

// build renders a multipart/alternative MIME message with a plain-text fallback.
func (s *SMTPSender) build(msg Message) ([]byte, error) {
	boundary := randomBoundary()
	from := (&mail.Address{Name: s.FromName, Address: s.From}).String()
	var b bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	hdr("From", from)
	hdr("To", msg.To)
	hdr("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	hdr("Date", time.Now().UTC().Format(time.RFC1123Z))
	hdr("Message-ID", fmt.Sprintf("<%s@%s>", randomBoundary(), domainOf(s.From)))
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", boundary))
	b.WriteString("\r\n")

	for _, part := range []struct{ ctype, body string }{
		{"text/plain; charset=utf-8", msg.Text},
		{"text/html; charset=utf-8", msg.HTML},
	} {
		if part.body == "" {
			continue
		}
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		fmt.Fprintf(&b, "Content-Type: %s\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", part.ctype)
		qp := quotedprintable.NewWriter(&b)
		if _, err := qp.Write([]byte(part.body)); err != nil {
			return nil, err
		}
		qp.Close()
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), nil
}

func randomBoundary() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}
