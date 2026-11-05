// Package config loads and validates config.json.
//
// The file holds no secrets: the chat service only knows the Manager's
// PUBLIC signing key(s). A leaked config cannot mint identities.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config is the on-disk configuration.
type Config struct {
	// Listen is the address the HTTP server binds to. Caddy terminates TLS
	// in front of it, so the default stays on loopback.
	Listen string `json:"listen"`
	// Database is the SQLite file path.
	Database string `json:"database"`
	// Issuer is the Manager's home URL, e.g. https://anchor.host. Tokens
	// must carry it as "iss".
	Issuer string `json:"issuer"`
	// Audience is this service's public URL, e.g. https://chat.example.com.
	// Tokens must carry it as "aud".
	Audience string `json:"audience"`
	// PublicKeys are base64 Ed25519 public keys (32 bytes). More than one
	// is allowed so the Manager can rotate its signing key without downtime.
	PublicKeys []string `json:"public_keys"`
	// AllowedOrigins are the browser origins (scheme://host[:port]) allowed
	// to call the API and open websockets. Defaults to the Issuer's origin.
	AllowedOrigins []string `json:"allowed_origins"`

	keys    []ed25519.PublicKey
	origins map[string]bool
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate fills defaults and checks every field. Load calls it; tests that
// build a Config by hand must call it too.
func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8490"
	}
	if c.Database == "" {
		c.Database = "data/chat.db"
	}
	var err error
	if c.Issuer, err = normalizeBase("issuer", c.Issuer); err != nil {
		return err
	}
	if c.Audience, err = normalizeBase("audience", c.Audience); err != nil {
		return err
	}

	if len(c.PublicKeys) == 0 {
		return errors.New("public_keys: at least one Manager public key is required")
	}
	c.keys = c.keys[:0]
	for i, k := range c.PublicKeys {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k))
		if err != nil {
			return fmt.Errorf("public_keys[%d]: not base64: %w", i, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return fmt.Errorf("public_keys[%d]: want %d bytes, got %d (is this the private key?)", i, ed25519.PublicKeySize, len(raw))
		}
		c.keys = append(c.keys, ed25519.PublicKey(raw))
	}

	if len(c.AllowedOrigins) == 0 {
		u, _ := url.Parse(c.Issuer)
		c.AllowedOrigins = []string{u.Scheme + "://" + u.Host}
	}
	c.origins = make(map[string]bool, len(c.AllowedOrigins))
	for i, o := range c.AllowedOrigins {
		n, err := normalizeOrigin(o)
		if err != nil {
			return fmt.Errorf("allowed_origins[%d]: %w", i, err)
		}
		c.AllowedOrigins[i] = n
		c.origins[n] = true
	}
	return nil
}

// Keys returns the decoded Manager public keys.
func (c *Config) Keys() []ed25519.PublicKey { return c.keys }

// OriginAllowed reports whether a browser Origin header value is allowed.
// Matching is exact after normalization; there are no wildcards.
func (c *Config) OriginAllowed(origin string) bool {
	n, err := normalizeOrigin(origin)
	return err == nil && c.origins[n]
}

func isLocalHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "[::1]" || strings.HasSuffix(h, ".localhost")
}

// normalizeBase accepts https URLs, or http only for local development hosts.
func normalizeBase(field, raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s: not a URL: %q", field, raw)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocalHost(u.Host)) {
		return "", fmt.Errorf("%s: must be https (http only for localhost): %q", field, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s: no credentials, query or fragment allowed: %q", field, raw)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath(), nil
}

func normalizeOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("not an origin: %q", raw)
	}
	if (u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("origin must be scheme://host[:port] only: %q", raw)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}
