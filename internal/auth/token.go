// Package auth verifies the short-lived identity tokens the CaptainCore
// Manager signs for its logged-in users.
//
// Format (docs/protocol.md has the PHP signer):
//
//	v1.<payload>.<signature>
//
// payload   = base64url(JSON claims), no padding
// signature = base64url(Ed25519(secret, "captaincore-chat.v1." + payload)), no padding
//
// One fixed algorithm and a domain-separated signing string: there is no
// "alg" header to downgrade and a signature made for anything else never
// verifies here. The service holds only public keys.
package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	tokenPrefix = "v1"
	sigContext  = "captaincore-chat.v1."
	// MaxTokenBytes caps input before any decoding work.
	MaxTokenBytes = 16 << 10

	RoleCustomer = "customer"
	RoleOperator = "operator"
)

// Claims is what the Manager asserts about the user.
type Claims struct {
	Issuer   string  `json:"iss"`
	Audience string  `json:"aud"`
	Subject  int64   `json:"sub"` // WordPress user ID on the Manager
	Email    string  `json:"email"`
	Name     string  `json:"name"`
	Role     string  `json:"role"`     // customer | operator
	Accounts []int64 `json:"accounts"` // CaptainCore account IDs the user belongs to
	Sites    []int64 `json:"sites"`    // site IDs the user may link a conversation to
	IssuedAt int64   `json:"iat"`
	Expires  int64   `json:"exp"`
	ID       string  `json:"jti"`
}

// IsOperator reports whether the user staffs the inbox.
func (c *Claims) IsOperator() bool { return c.Role == RoleOperator }

// HasAccount reports whether id is one of the user's accounts.
func (c *Claims) HasAccount(id int64) bool { return contains(c.Accounts, id) }

// HasSite reports whether id is one of the user's sites.
func (c *Claims) HasSite(id int64) bool { return contains(c.Sites, id) }

func contains(list []int64, id int64) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

// ErrInvalid is returned for every rejected token. Callers should not tell
// clients which check failed.
var ErrInvalid = errors.New("invalid token")

// Verifier checks tokens against the configured keys, issuer and audience.
type Verifier struct {
	Keys        []ed25519.PublicKey
	Issuer      string
	Audience    string
	MaxLifetime time.Duration // exp - iat may not exceed this
	Skew        time.Duration // tolerated clock difference with the Manager
	Now         func() time.Time
}

// NewVerifier returns a Verifier with the default 15 minute lifetime cap
// and 60 second clock skew.
func NewVerifier(keys []ed25519.PublicKey, issuer, audience string) *Verifier {
	return &Verifier{Keys: keys, Issuer: issuer, Audience: audience, MaxLifetime: 15 * time.Minute, Skew: 60 * time.Second, Now: time.Now}
}

// Verify returns the claims of a valid token. The wrapped detail is for
// server logs only.
func (v *Verifier) Verify(token string) (*Claims, error) {
	if len(token) == 0 || len(token) > MaxTokenBytes {
		return nil, fmt.Errorf("%w: size", ErrInvalid)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return nil, fmt.Errorf("%w: shape", ErrInvalid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature encoding", ErrInvalid)
	}
	msg := []byte(sigContext + parts[1])
	ok := false
	for _, k := range v.Keys {
		if ed25519.Verify(k, msg, sig) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("%w: signature", ErrInvalid)
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload encoding", ErrInvalid)
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%w: payload json", ErrInvalid)
	}

	now := v.Now().Unix()
	skew := int64(v.Skew / time.Second)
	switch {
	case c.Issuer != v.Issuer:
		return nil, fmt.Errorf("%w: issuer %q", ErrInvalid, c.Issuer)
	case c.Audience != v.Audience:
		return nil, fmt.Errorf("%w: audience %q", ErrInvalid, c.Audience)
	case c.Subject <= 0:
		return nil, fmt.Errorf("%w: subject", ErrInvalid)
	case c.Role != RoleCustomer && c.Role != RoleOperator:
		return nil, fmt.Errorf("%w: role %q", ErrInvalid, c.Role)
	case c.IssuedAt == 0 || c.Expires <= c.IssuedAt:
		return nil, fmt.Errorf("%w: times", ErrInvalid)
	case c.Expires-c.IssuedAt > int64(v.MaxLifetime/time.Second):
		return nil, fmt.Errorf("%w: lifetime over %s", ErrInvalid, v.MaxLifetime)
	case c.IssuedAt > now+skew:
		return nil, fmt.Errorf("%w: issued in the future", ErrInvalid)
	case c.Expires <= now-skew:
		return nil, fmt.Errorf("%w: expired", ErrInvalid)
	}
	return &c, nil
}

// Sign produces a token. The Manager signs in PHP; this exists for tests
// and the `dev token` command.
func Sign(priv ed25519.PrivateKey, c *Claims) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	sig := ed25519.Sign(priv, []byte(sigContext+payload))
	return tokenPrefix + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
