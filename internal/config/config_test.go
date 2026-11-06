package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pubB64(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func TestValidateDefaults(t *testing.T) {
	c := &Config{Issuer: "https://Anchor.Host/", Audience: "https://chat.example.com", PublicKeys: []string{pubB64(t)}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8490" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.Issuer != "https://anchor.host" {
		t.Fatalf("issuer not normalized: %q", c.Issuer)
	}
	if !c.OriginAllowed("https://anchor.host") || !c.OriginAllowed("https://ANCHOR.host/") {
		t.Fatal("issuer origin should be allowed by default")
	}
	if c.OriginAllowed("https://evil.example") || c.OriginAllowed("https://anchor.host.evil.example") || c.OriginAllowed("null") {
		t.Fatal("foreign origin allowed")
	}
}

func TestValidateRejects(t *testing.T) {
	good := pubB64(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	cases := map[string]Config{
		"http issuer":        {Issuer: "http://anchor.host", Audience: "https://chat.example.com", PublicKeys: []string{good}},
		"missing audience":   {Issuer: "https://anchor.host", PublicKeys: []string{good}},
		"no keys":            {Issuer: "https://anchor.host", Audience: "https://chat.example.com"},
		"private key pasted": {Issuer: "https://anchor.host", Audience: "https://chat.example.com", PublicKeys: []string{base64.StdEncoding.EncodeToString(priv)}},
		"origin with path":   {Issuer: "https://anchor.host", Audience: "https://chat.example.com", PublicKeys: []string{good}, AllowedOrigins: []string{"https://anchor.host/account"}},
	}
	for name, c := range cases {
		c := c
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLocalhostHTTPAllowed(t *testing.T) {
	c := &Config{Issuer: "https://anchor.localhost", Audience: "http://127.0.0.1:8490", PublicKeys: []string{pubB64(t)}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	body := `{"issuer":"https://anchor.host","audience":"https://chat.example.com","public_keys":["` + pubB64(t) + `"],"private_key":"x"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "private_key") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}
