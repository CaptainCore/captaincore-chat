package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// maxPublishBytes fits a conversation plus a 10,000 character message with
// room to spare.
const maxPublishBytes = 512 << 10

// publishRequest is the body the Manager signs and sends.
//
//	{"event":{"type":"message.created",...},"customer_user_id":42,"internal":false}
type publishRequest struct {
	Event          json.RawMessage `json:"event"`
	CustomerUserID int64           `json:"customer_user_id"`
	Internal       bool            `json:"internal"`
}

// POST /internal/publish. The Manager's write path ends here: it stores the
// change, then signs the event with the same Ed25519 key that signs tokens.
// Headers: X-Chat-Timestamp (unix seconds), X-Chat-Signature (base64url).
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPublishBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "Body too large.")
		return
	}
	ts, sig := r.Header.Get("X-Chat-Timestamp"), r.Header.Get("X-Chat-Signature")
	if err := s.verifier.VerifyPublish(ts, body, sig); err != nil {
		s.log.Info("publish rejected", "reason", err.Error())
		writeError(w, http.StatusUnauthorized, "unauthorized", "Bad signature.")
		return
	}
	if !s.replay.first(sig, s.verifier.Now()) {
		writeError(w, http.StatusConflict, "replay", "Already delivered.")
		return
	}
	var req publishRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.CustomerUserID < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "Malformed publish body.")
		return
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(req.Event, &head) != nil || head.Type == "" || head.Type == "typing" || head.Type == "presence" || head.Type == "ready" {
		writeError(w, http.StatusBadRequest, "bad_request", "Event needs a publishable type.")
		return
	}
	// A guest conversation (customer 0) or an internal event reaches
	// operators only.
	internal := req.Internal || req.CustomerUserID == 0
	n, customer := s.hub.PublishRaw(req.Event, req.CustomerUserID, internal, nil)
	writeJSON(w, http.StatusOK, map[string]any{"delivered": n, "customer": customer})
}

// replayCache remembers signatures for twice the clock skew, which is
// longer than any timestamp the verifier will accept.
type replayCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	seen map[[32]byte]time.Time
}

func newReplayCache(ttl time.Duration) *replayCache {
	return &replayCache{ttl: ttl, seen: make(map[[32]byte]time.Time)}
}

// first reports whether sig has not been seen within the window.
func (c *replayCache) first(sig string, now time.Time) bool {
	k := sha256.Sum256([]byte(sig))
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, at := range c.seen {
		if now.Sub(at) > c.ttl {
			delete(c.seen, key)
		}
	}
	if _, dup := c.seen[k]; dup {
		return false
	}
	c.seen[k] = now
	return true
}
