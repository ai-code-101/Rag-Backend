// Package bots holds bot, widget-config and document domain logic. Every query that
// takes an owner ID scopes by it, so one tenant can never read or modify another's data.
package bots

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ValidationError is returned for user-correctable input problems.
type ValidationError struct{ Field, Msg string }

func (e *ValidationError) Error() string { return e.Field + ": " + e.Msg }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// NewPublicKey returns a random, URL-safe widget key such as pk_Q2x...
func NewPublicKey() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "pk_" + base64.RawURLEncoding.EncodeToString(b), nil
}

var publicKeyRe = regexp.MustCompile(`^pk_[A-Za-z0-9_-]{24}$`)

func ValidPublicKey(k string) bool { return publicKeyRe.MatchString(k) }

const MaxOrigins = 20

// NormalizeOrigin canonicalises an origin ("https://Example.com:443/" → "https://example.com").
// A leading "*." wildcard label is allowed: "https://*.example.com".
func NormalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 255 {
		return "", errors.New("origin is empty or too long")
	}
	wildcard := false
	probe := raw
	if i := strings.Index(raw, "://*."); i > 0 {
		wildcard = true
		probe = raw[:i+3] + "wildcard." + raw[i+5:]
	}
	u, err := url.Parse(probe)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%q is not a valid http(s) origin", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%q must be an origin only (scheme://host[:port]) without a path", raw)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, " *") {
		return "", fmt.Errorf("%q has an invalid host", raw)
	}
	if wildcard && net.ParseIP(strings.TrimPrefix(host, "wildcard.")) != nil {
		return "", fmt.Errorf("%q: wildcards are not allowed with IP addresses", raw)
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if wildcard {
		host = "*." + strings.TrimPrefix(host, "wildcard.")
	}
	out := u.Scheme + "://" + host
	if port != "" {
		out += ":" + port
	}
	return out, nil
}

func NormalizeOrigins(in []string) ([]string, error) {
	if len(in) > MaxOrigins {
		return nil, invalid("allowed_origins", "at most %d origins", MaxOrigins)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, o := range in {
		n, err := NormalizeOrigin(o)
		if err != nil {
			return nil, invalid("allowed_origins", "%s", err.Error())
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

// OriginAllowed reports whether origin matches the allow-list. An empty list allows every
// origin (the dashboard warns about this).
func OriginAllowed(allowed []string, origin string) bool {
	if len(allowed) == 0 {
		return true
	}
	if origin == "" || origin == "null" {
		return false
	}
	norm, err := NormalizeOrigin(origin)
	if err != nil {
		return false
	}
	for _, a := range allowed {
		if a == norm {
			return true
		}
		if i := strings.Index(a, "://*."); i > 0 {
			scheme, suffix := a[:i], a[i+4:] // suffix keeps the leading dot: ".example.com[:port]"
			if strings.HasPrefix(norm, scheme+"://") && strings.HasSuffix(norm, suffix) &&
				len(norm) > len(scheme)+3+len(suffix) {
				return true
			}
		}
	}
	return false
}

// ---- Bot settings ----------------------------------------------------------

const (
	MaxNameLen         = 80
	MaxInstructionsLen = 8000
	MaxFallbackLen     = 500
)

type BotInput struct {
	Name               *string   `json:"name"`
	SystemInstructions *string   `json:"system_instructions"`
	FallbackMessage    *string   `json:"fallback_message"`
	Model              *string   `json:"model"`
	Temperature        *float32  `json:"temperature"`
	AllowedOrigins     *[]string `json:"allowed_origins"`
}

func (in *BotInput) Validate(allowedModels []string) error {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || utf8.RuneCountInString(n) > MaxNameLen {
			return invalid("name", "must be 1-%d characters", MaxNameLen)
		}
		in.Name = &n
	}
	if in.SystemInstructions != nil && utf8.RuneCountInString(*in.SystemInstructions) > MaxInstructionsLen {
		return invalid("system_instructions", "must be at most %d characters", MaxInstructionsLen)
	}
	if in.FallbackMessage != nil && utf8.RuneCountInString(*in.FallbackMessage) > MaxFallbackLen {
		return invalid("fallback_message", "must be at most %d characters", MaxFallbackLen)
	}
	if in.Model != nil {
		ok := false
		for _, m := range allowedModels {
			if m == *in.Model {
				ok = true
			}
		}
		if !ok {
			return invalid("model", "must be one of %s", strings.Join(allowedModels, ", "))
		}
	}
	if in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 1) {
		return invalid("temperature", "must be between 0 and 1")
	}
	if in.AllowedOrigins != nil {
		norm, err := NormalizeOrigins(*in.AllowedOrigins)
		if err != nil {
			return err
		}
		in.AllowedOrigins = &norm
	}
	return nil
}

// ---- Widget config ----------------------------------------------------------

type Theme struct {
	PrimaryColor    string `json:"primary_color"`
	BackgroundColor string `json:"background_color"`
	TextColor       string `json:"text_color"`
	UserBubbleColor string `json:"user_bubble_color"`
	BotBubbleColor  string `json:"bot_bubble_color"`
	Radius          int    `json:"radius"`
	FontFamily      string `json:"font_family"`
	LogoURL         string `json:"logo_url"`
	LauncherIcon    string `json:"launcher_icon"`
}

type Content struct {
	Title            string   `json:"title"`
	Subtitle         string   `json:"subtitle"`
	Greeting         string   `json:"greeting"`
	QuickReplies     []string `json:"quick_replies"`
	OfflineMessage   string   `json:"offline_message"`
	InputPlaceholder string   `json:"input_placeholder"`
}

type Behavior struct {
	Position      string   `json:"position"`
	AutoOpen      bool     `json:"auto_open"`
	AutoOpenDelay int      `json:"auto_open_delay_ms"`
	HideOnPaths   []string `json:"hide_on_paths"`
	OffsetX       int      `json:"offset_x"`
	OffsetY       int      `json:"offset_y"`
}

type LeadCapture struct {
	Enabled bool     `json:"enabled"`
	Fields  []string `json:"fields"`
	Prompt  string   `json:"prompt"`
}

type Features struct {
	LeadCapture LeadCapture `json:"lead_capture"`
	ShowSources bool        `json:"show_sources"`
	Branding    bool        `json:"branding"`
}

type WidgetSettings struct {
	Theme    Theme    `json:"theme"`
	Content  Content  `json:"content"`
	Behavior Behavior `json:"behavior"`
	Features Features `json:"features"`
}

func DefaultWidgetSettings(botName string) WidgetSettings {
	return WidgetSettings{
		Theme: Theme{
			PrimaryColor: "#4f46e5", BackgroundColor: "#ffffff", TextColor: "#111827",
			UserBubbleColor: "#4f46e5", BotBubbleColor: "#f3f4f6", Radius: 16,
			FontFamily: "system", LauncherIcon: "chat",
		},
		Content: Content{
			Title: botName, Subtitle: "We typically reply instantly", Greeting: "Hi! How can I help you today?",
			QuickReplies: []string{}, OfflineMessage: "We're having trouble connecting. Please try again shortly.",
			InputPlaceholder: "Type your message…",
		},
		Behavior: Behavior{Position: "bottom-right", HideOnPaths: []string{}, OffsetX: 20, OffsetY: 20},
		Features: Features{LeadCapture: LeadCapture{Fields: []string{"email"}}, Branding: true},
	}
}

var (
	colorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	pathRe  = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`)
	fonts   = map[string]bool{"system": true, "inter": true, "roboto": true, "open-sans": true, "lato": true, "serif": true, "mono": true}
	icons   = map[string]bool{"chat": true, "message": true, "help": true, "sparkle": true}
	fields  = map[string]bool{"name": true, "email": true, "phone": true}
)

func validText(field, v string, max int) error {
	if utf8.RuneCountInString(v) > max {
		return invalid(field, "must be at most %d characters", max)
	}
	if strings.ContainsAny(v, "\x00") {
		return invalid(field, "contains invalid characters")
	}
	return nil
}

func validImageURL(field, v string, allowHTTP bool) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || len(v) > 500 || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return invalid(field, "must be an https URL")
	}
	return nil
}

// Validate checks every field. Values end up as CSS variables and text on customers'
// sites, so colours and URLs are strictly pattern-checked rather than escaped later.
func (w *WidgetSettings) Validate(allowHTTP bool) error {
	t := &w.Theme
	for name, c := range map[string]string{
		"theme.primary_color": t.PrimaryColor, "theme.background_color": t.BackgroundColor,
		"theme.text_color": t.TextColor, "theme.user_bubble_color": t.UserBubbleColor,
		"theme.bot_bubble_color": t.BotBubbleColor,
	} {
		if !colorRe.MatchString(c) {
			return invalid(name, "must be a hex colour like #4f46e5")
		}
	}
	if t.Radius < 0 || t.Radius > 32 {
		return invalid("theme.radius", "must be between 0 and 32")
	}
	if !fonts[t.FontFamily] {
		return invalid("theme.font_family", "unsupported font")
	}
	if err := validImageURL("theme.logo_url", t.LogoURL, allowHTTP); err != nil {
		return err
	}
	if !icons[t.LauncherIcon] {
		if err := validImageURL("theme.launcher_icon", t.LauncherIcon, allowHTTP); err != nil || t.LauncherIcon == "" {
			return invalid("theme.launcher_icon", "must be chat, message, help, sparkle or an https image URL")
		}
	}

	c := &w.Content
	for _, f := range []struct {
		name string
		v    string
		max  int
	}{
		{"content.title", c.Title, 60}, {"content.subtitle", c.Subtitle, 120}, {"content.greeting", c.Greeting, 500},
		{"content.offline_message", c.OfflineMessage, 300}, {"content.input_placeholder", c.InputPlaceholder, 80},
	} {
		if err := validText(f.name, f.v, f.max); err != nil {
			return err
		}
	}
	if len(c.QuickReplies) > 6 {
		return invalid("content.quick_replies", "at most 6 quick replies")
	}
	for _, q := range c.QuickReplies {
		if strings.TrimSpace(q) == "" {
			return invalid("content.quick_replies", "quick replies cannot be empty")
		}
		if err := validText("content.quick_replies", q, 80); err != nil {
			return err
		}
	}
	if c.QuickReplies == nil {
		c.QuickReplies = []string{}
	}

	b := &w.Behavior
	if b.Position != "bottom-right" && b.Position != "bottom-left" {
		return invalid("behavior.position", "must be bottom-right or bottom-left")
	}
	if b.AutoOpenDelay < 0 || b.AutoOpenDelay > 60000 {
		return invalid("behavior.auto_open_delay_ms", "must be between 0 and 60000")
	}
	if b.OffsetX < 0 || b.OffsetX > 200 || b.OffsetY < 0 || b.OffsetY > 200 {
		return invalid("behavior.offset", "offsets must be between 0 and 200")
	}
	if len(b.HideOnPaths) > 50 {
		return invalid("behavior.hide_on_paths", "at most 50 paths")
	}
	for _, p := range b.HideOnPaths {
		if len(p) > 200 || !pathRe.MatchString(p) {
			return invalid("behavior.hide_on_paths", "%q must be a path starting with / (use * as a wildcard)", p)
		}
	}
	if b.HideOnPaths == nil {
		b.HideOnPaths = []string{}
	}

	lc := &w.Features.LeadCapture
	if len(lc.Fields) > 3 {
		return invalid("features.lead_capture.fields", "unknown field")
	}
	for _, f := range lc.Fields {
		if !fields[f] {
			return invalid("features.lead_capture.fields", "unknown field %q", f)
		}
	}
	if lc.Enabled && len(lc.Fields) == 0 {
		return invalid("features.lead_capture.fields", "choose at least one field")
	}
	if lc.Fields == nil {
		lc.Fields = []string{}
	}
	return validText("features.lead_capture.prompt", lc.Prompt, 200)
}

// DecodeStrict unmarshals JSON and rejects unknown fields.
func DecodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("body", "%s", err.Error())
	}
	return nil
}
