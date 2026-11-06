package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"
)

// publishContext separates publish signatures from token signatures, so a
// signature made for one can never be replayed as the other.
const publishContext = "captaincore-chat.publish.v1."

// PublishSigningString is what the Manager signs for POST /internal/publish:
// the context, the unix timestamp it sends in X-Chat-Timestamp, a dot, then
// the exact request body bytes.
func PublishSigningString(ts string, body []byte) []byte {
	b := make([]byte, 0, len(publishContext)+len(ts)+1+len(body))
	b = append(b, publishContext...)
	b = append(b, ts...)
	b = append(b, '.')
	return append(b, body...)
}

// VerifyPublish checks a Manager-signed publish request. ts must be within
// the verifier's skew of now. The wrapped detail is for server logs only.
func (v *Verifier) VerifyPublish(ts string, body []byte, sig string) error {
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("%w: timestamp", ErrInvalid)
	}
	now := v.Now().Unix()
	skew := int64(v.Skew / time.Second)
	if n < now-skew || n > now+skew {
		return fmt.Errorf("%w: timestamp outside %s", ErrInvalid, v.Skew)
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: signature encoding", ErrInvalid)
	}
	msg := PublishSigningString(ts, body)
	for _, k := range v.Keys {
		if ed25519.Verify(k, msg, raw) {
			return nil
		}
	}
	return fmt.Errorf("%w: signature", ErrInvalid)
}

// SignPublish signs a publish body. The Manager signs in PHP; this exists
// for tests and the `dev publish` command.
func SignPublish(priv ed25519.PrivateKey, ts string, body []byte) string {
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, PublishSigningString(ts, body)))
}
