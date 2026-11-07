package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CaptainCore/captaincore-chat/internal/auth"
	"github.com/CaptainCore/captaincore-chat/internal/config"
	"github.com/gorilla/websocket"
)

const (
	manager = "https://manager.test"
	chatURL = "https://chat.test"
)

type env struct {
	t    *testing.T
	srv  *Server
	ts   *httptest.Server
	priv ed25519.PrivateKey
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := &config.Config{Issuer: manager, Audience: chatURL, PublicKeys: []string{base64.StdEncoding.EncodeToString(pub)}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.hub.CloseAll(); ts.Close() })
	return &env{t: t, srv: srv, ts: ts, priv: priv}
}

func (e *env) token(sub int64, role string) string {
	now := time.Now()
	tok, err := auth.Sign(e.priv, &auth.Claims{Issuer: manager, Audience: chatURL, Subject: sub, Name: "User " + strconv.FormatInt(sub, 10),
		Email: "u@example.com", Role: role, IssuedAt: now.Unix(), Expires: now.Add(10 * time.Minute).Unix()})
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// publish sends a signed event the way the Manager does. key and ts may be
// overridden to test rejections.
func (e *env) publish(body []byte, key ed25519.PrivateKey, ts string) (int, map[string]any) {
	e.t.Helper()
	if key == nil {
		key = e.priv
	}
	if ts == "" {
		ts = strconv.FormatInt(time.Now().Unix(), 10)
	}
	req, _ := http.NewRequest("POST", e.ts.URL+"/internal/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Chat-Timestamp", ts)
	req.Header.Set("X-Chat-Signature", auth.SignPublish(key, ts, body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func event(typ string, conv int64, customer int64, internal bool, extra map[string]any) []byte {
	ev := map[string]any{"type": typ, "conversation_id": conv}
	for k, v := range extra {
		ev[k] = v
	}
	b, _ := json.Marshal(map[string]any{"event": ev, "customer_user_id": customer, "internal": internal})
	return b
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	res, err := http.Get(e.ts.URL + "/healthz")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", err, res)
	}
	res.Body.Close()
	// The old REST surface is gone.
	res, _ = http.Get(e.ts.URL + "/v1/conversations")
	if res.StatusCode != 404 {
		t.Fatalf("REST should be gone, got %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestPublishSignature(t *testing.T) {
	e := newEnv(t)
	body := event("conversation.updated", 7, 42, false, nil)
	if code, out := e.publish(body, nil, ""); code != 200 || out["delivered"] != float64(0) {
		t.Fatalf("valid publish: %d %v", code, out)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if code, _ := e.publish(event("conversation.updated", 7, 42, false, map[string]any{"n": 1}), other, ""); code != 401 {
		t.Fatalf("foreign key: %d", code)
	}
	stale := strconv.FormatInt(time.Now().Add(-5*time.Minute).Unix(), 10)
	if code, _ := e.publish(event("conversation.updated", 7, 42, false, map[string]any{"n": 2}), nil, stale); code != 401 {
		t.Fatalf("stale timestamp: %d", code)
	}
	// A signature made over one body never verifies another.
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, _ := http.NewRequest("POST", e.ts.URL+"/internal/publish", bytes.NewReader(event("conversation.updated", 8, 42, false, nil)))
	req.Header.Set("X-Chat-Timestamp", ts)
	req.Header.Set("X-Chat-Signature", auth.SignPublish(e.priv, ts, body))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 401 {
		t.Fatalf("tampered body: %d", res.StatusCode)
	}
	// A token signature can't be replayed as a publish (domain separation).
	tok := e.token(3, "operator")
	parts := strings.Split(tok, ".")
	req, _ = http.NewRequest("POST", e.ts.URL+"/internal/publish", strings.NewReader(parts[1]))
	req.Header.Set("X-Chat-Timestamp", ts)
	req.Header.Set("X-Chat-Signature", parts[2])
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 401 {
		t.Fatalf("token signature accepted as publish: %d", res.StatusCode)
	}
}

func TestPublishReplayAndShape(t *testing.T) {
	e := newEnv(t)
	body := event("message.created", 7, 42, false, nil)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	if code, _ := e.publish(body, nil, ts); code != 200 {
		t.Fatalf("first: %d", code)
	}
	if code, _ := e.publish(body, nil, ts); code != 409 {
		t.Fatalf("replay should be refused: %d", code)
	}
	for _, bad := range [][]byte{
		[]byte(`{"event":{"type":"typing"},"customer_user_id":42,"internal":false}`),
		[]byte(`{"event":{"conversation_id":1},"customer_user_id":42,"internal":false}`),
		[]byte(`{"event":{"type":"x"},"customer_user_id":42,"internal":false,"extra":1}`),
		[]byte(`not json`),
	} {
		if code, _ := e.publish(bad, nil, ""); code != 400 {
			t.Fatalf("%s: want 400, got %d", bad, code)
		}
	}
}

// ── websocket ──

func (e *env) dial(origin, tok string) (*websocket.Conn, map[string]any) {
	e.t.Helper()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.ts.URL, "http")+"/v1/ws", h)
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	e.t.Cleanup(func() { c.Close() })
	c.WriteJSON(map[string]string{"type": "auth", "token": tok})
	return c, read(e.t, c)
}

func read(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m map[string]any
	if err := c.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

// next skips frames until one of type typ arrives.
func next(t *testing.T, c *websocket.Conn, typ string) map[string]any {
	t.Helper()
	for i := 0; i < 10; i++ {
		if m := read(t, c); m["type"] == typ {
			return m
		}
	}
	t.Fatalf("no %s frame", typ)
	return nil
}

// quiet fails if c receives anything other than presence frames.
func quiet(t *testing.T, c *websocket.Conn, who string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		var m map[string]any
		if err := c.ReadJSON(&m); err != nil {
			return // timeout: nothing more queued
		}
		if m["type"] != "presence" {
			t.Fatalf("%s received %v", who, m)
		}
	}
}

func TestWebsocketOriginAndAuth(t *testing.T) {
	e := newEnv(t)
	h := http.Header{"Origin": {"https://evil.test"}}
	if _, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.ts.URL, "http")+"/v1/ws", h); err == nil || res.StatusCode != 403 {
		t.Fatalf("foreign origin should be refused with 403")
	}
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.ts.URL, "http")+"/v1/ws", http.Header{"Origin": {manager}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.WriteJSON(map[string]string{"type": "auth", "token": "v1.bad.token"})
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = c.ReadMessage()
	if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != closeAuth {
		t.Fatalf("want close 4001, got %v", err)
	}
}

func TestPublishRouting(t *testing.T) {
	e := newEnv(t)
	priya, ready := e.dial(manager, e.token(42, "customer"))
	if ready["type"] != "ready" || ready["operators_online"] != false {
		t.Fatalf("ready: %v", ready)
	}
	austin, _ := e.dial(manager, e.token(3, "operator"))
	if p := next(t, priya, "presence"); p["operators_online"] != true {
		t.Fatalf("presence: %v", p)
	}
	marcus, mready := e.dial(manager, e.token(99, "customer"))
	if mready["operators_online"] != true {
		t.Fatalf("late customer should see the operator online: %v", mready)
	}

	// A note: operators only.
	if _, out := e.publish(event("message.created", 7, 42, true, map[string]any{"message": map[string]any{"body": "internal: plugin X"}}), nil, ""); out["delivered"] != float64(1) || out["customer"] != float64(0) {
		t.Fatalf("note delivered to %v (customer %v)", out["delivered"], out["customer"])
	}
	if m := next(t, austin, "message.created"); m["message"].(map[string]any)["body"] != "internal: plugin X" {
		t.Fatalf("operator note: %v", m)
	}
	// A reply: operators and the owner.
	if _, out := e.publish(event("message.created", 7, 42, false, map[string]any{"message": map[string]any{"body": "Looking now"}}), nil, ""); out["delivered"] != float64(2) || out["customer"] != float64(1) {
		t.Fatalf("reply delivered to %v (customer %v)", out["delivered"], out["customer"])
	}
	if m := next(t, priya, "message.created"); m["message"].(map[string]any)["body"] != "Looking now" {
		t.Fatalf("customer got %v (the note must never arrive)", m)
	}
	next(t, austin, "message.created")
	// A guest conversation (customer 0) reaches operators only, even if not internal.
	if _, out := e.publish(event("message.created", 8, 0, false, nil), nil, ""); out["delivered"] != float64(1) {
		t.Fatalf("guest event delivered to %v", out["delivered"])
	}
	next(t, austin, "message.created")

	// Typing: the operator names the customer.
	austin.WriteJSON(map[string]any{"type": "typing", "conversation_id": 7, "customer_user_id": 42})
	if ev := next(t, priya, "typing"); ev["user"].(map[string]any)["operator"] != true {
		t.Fatalf("typing: %v", ev)
	}
	// A customer's typing goes to operators with their id, never to another customer.
	marcus.WriteJSON(map[string]any{"type": "typing", "conversation_id": 7})
	if ev := next(t, austin, "typing"); ev["user"].(map[string]any)["id"] != float64(99) {
		t.Fatalf("customer typing should carry the sender id: %v", ev)
	}
	quiet(t, priya, "priya")
	quiet(t, marcus, "marcus")

	// Watching: only an operator's focus frame counts; a customer's is ignored.
	marcus.WriteJSON(map[string]any{"type": "focus", "active": true})
	austin.WriteJSON(map[string]any{"type": "focus", "active": true})
	time.Sleep(100 * time.Millisecond)
	if _, out := e.publish(event("conversation.updated", 7, 42, false, nil), nil, ""); fmt.Sprint(out["watching"]) != "[3]" {
		t.Fatalf("watching = %v, want [3]", out["watching"])
	}
	austin.WriteJSON(map[string]any{"type": "focus", "active": false})
	time.Sleep(100 * time.Millisecond)
	if _, out := e.publish(event("conversation.updated", 9, 42, false, nil), nil, ""); fmt.Sprint(out["watching"]) != "[]" {
		t.Fatalf("watching after blur = %v, want []", out["watching"])
	}
}
