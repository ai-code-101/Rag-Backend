package bots

import (
	"strings"
	"testing"
)

func TestNormalizeOrigin(t *testing.T) {
	ok := map[string]string{
		"https://Example.com":         "https://example.com",
		"https://example.com:443/":    "https://example.com",
		"http://localhost:3000":       "http://localhost:3000",
		"https://*.Example.com":       "https://*.example.com",
		"https://shop.example.com:84": "https://shop.example.com:84",
	}
	for in, want := range ok {
		got, err := NormalizeOrigin(in)
		if err != nil || got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "example.com", "ftp://example.com", "https://example.com/path",
		"https://user:pw@example.com", "https://example.com?x=1", "javascript:alert(1)", "https://*.1.2.3.4"} {
		if _, err := NormalizeOrigin(in); err == nil {
			t.Errorf("NormalizeOrigin(%q) should fail", in)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	list := []string{"https://example.com", "https://*.shop.io"}
	cases := map[string]bool{
		"https://example.com":         true,
		"https://EXAMPLE.com:443":     true,
		"http://example.com":          false,
		"https://evil.com":            false,
		"https://example.com.evil.io": false,
		"https://a.shop.io":           true,
		"https://a.b.shop.io":         true,
		"https://shop.io":             false, // wildcard does not match the apex (same as CSP)
		"https://evilshop.io":         false,
		"null":                        false,
		"":                            false,
	}
	for origin, want := range cases {
		if got := OriginAllowed(list, origin); got != want {
			t.Errorf("OriginAllowed(%q) = %v, want %v", origin, got, want)
		}
	}
	if !OriginAllowed(nil, "https://anything.example") {
		t.Error("empty allow-list should allow any origin")
	}
}

func TestPublicKey(t *testing.T) {
	k, err := NewPublicKey()
	if err != nil || !ValidPublicKey(k) {
		t.Fatalf("bad key %q: %v", k, err)
	}
	k2, _ := NewPublicKey()
	if k == k2 {
		t.Fatal("keys must be random")
	}
	for _, bad := range []string{"", "pk_short", "pk_" + strings.Repeat("a", 23) + "!", "xx_" + strings.Repeat("a", 24)} {
		if ValidPublicKey(bad) {
			t.Errorf("ValidPublicKey(%q) should be false", bad)
		}
	}
}

func TestBotInputValidate(t *testing.T) {
	models := []string{"claude-opus-5-5", "claude-haiku-5-5"}
	s := func(v string) *string { return &v }
	f := func(v float32) *float32 { return &v }
	origins := []string{"https://A.com", "https://a.com/"}
	in := BotInput{Name: s("  Support bot "), Model: s("claude-haiku-5-5"), Temperature: f(0.5), AllowedOrigins: &origins}
	if err := in.Validate(models); err != nil {
		t.Fatal(err)
	}
	if *in.Name != "Support bot" || len(*in.AllowedOrigins) != 1 || (*in.AllowedOrigins)[0] != "https://a.com" {
		t.Fatalf("normalisation failed: %q %v", *in.Name, *in.AllowedOrigins)
	}
	for name, bad := range map[string]BotInput{
		"empty name":   {Name: s(" ")},
		"model":        {Model: s("gpt-unknown")},
		"temperature":  {Temperature: f(1.5)},
		"instructions": {SystemInstructions: s(strings.Repeat("x", MaxInstructionsLen+1))},
	} {
		if err := bad.Validate(models); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestWidgetSettingsValidate(t *testing.T) {
	good := DefaultWidgetSettings("Bot")
	if err := good.Validate(false); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	mutate := map[string]func(w *WidgetSettings){
		"css injection in colour": func(w *WidgetSettings) { w.Theme.PrimaryColor = "red;background:url(x)" },
		"radius":                  func(w *WidgetSettings) { w.Theme.Radius = 99 },
		"font":                    func(w *WidgetSettings) { w.Theme.FontFamily = "Comic Sans" },
		"logo javascript url":     func(w *WidgetSettings) { w.Theme.LogoURL = "javascript:alert(1)" },
		"logo http in prod":       func(w *WidgetSettings) { w.Theme.LogoURL = "http://example.com/logo.png" },
		"position":                func(w *WidgetSettings) { w.Behavior.Position = "top-center" },
		"hide path":               func(w *WidgetSettings) { w.Behavior.HideOnPaths = []string{"no-slash"} },
		"quick replies":           func(w *WidgetSettings) { w.Content.QuickReplies = []string{"1", "2", "3", "4", "5", "6", "7"} },
		"lead field":              func(w *WidgetSettings) { w.Features.LeadCapture.Fields = []string{"ssn"} },
		"title length":            func(w *WidgetSettings) { w.Content.Title = strings.Repeat("t", 61) },
	}
	for name, m := range mutate {
		w := DefaultWidgetSettings("Bot")
		m(&w)
		if err := w.Validate(false); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	w := DefaultWidgetSettings("Bot")
	w.Theme.LauncherIcon = "https://cdn.example.com/icon.svg"
	w.Behavior.HideOnPaths = []string{"/checkout/*", "/admin"}
	if err := w.Validate(false); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
}
