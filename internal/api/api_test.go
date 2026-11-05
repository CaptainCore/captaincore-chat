package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CaptainCore/captaincore-chat/internal/auth"
	"github.com/CaptainCore/captaincore-chat/internal/config"
	"github.com/CaptainCore/captaincore-chat/internal/store"
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
	st, err := store.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.hub.CloseAll(); ts.Close(); st.Close() })
	return &env{t: t, srv: srv, ts: ts, priv: priv}
}

func (e *env) token(sub int64, role string, sites ...int64) string {
	now := time.Now()
	tok, err := auth.Sign(e.priv, &auth.Claims{Issuer: manager, Audience: chatURL, Subject: sub, Name: "User " + string(rune('A'+sub%26)),
		Email: "u@example.com", Role: role, Accounts: []int64{sub * 10}, Sites: sites, IssuedAt: now.Unix(), Expires: now.Add(10 * time.Minute).Unix()})
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) do(method, path, tok string, body any) (int, map[string]any) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, r)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func id(m map[string]any, key string) float64 { return m[key].(map[string]any)["id"].(float64) }

func TestHealthAndAuth(t *testing.T) {
	e := newEnv(t)
	if code, out := e.do("GET", "/healthz", "", nil); code != 200 || out["status"] != "ok" {
		t.Fatalf("healthz %d %v", code, out)
	}
	if code, _ := e.do("GET", "/v1/me", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := e.do("GET", "/v1/me", "v1.garbage.sig", nil); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	if code, out := e.do("GET", "/v1/me", e.token(42, "customer"), nil); code != 200 || out["operators_online"] != false {
		t.Fatalf("me: %d %v", code, out)
	}
}

func TestCustomerAndOperatorRules(t *testing.T) {
	e := newEnv(t)
	priya, marcus, austin := e.token(42, "customer", 11), e.token(99, "customer"), e.token(3, "operator")

	if code, _ := e.do("POST", "/v1/conversations", priya, map[string]any{"body": "hi", "site_id": 12}); code != 403 {
		t.Fatalf("site not in token must be refused: %d", code)
	}
	code, out := e.do("POST", "/v1/conversations", priya, map[string]any{"body": "Checkout shows an error\nsince last night", "site_id": 11})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	conv := out["conversation"].(map[string]any)
	cid := int64(conv["id"].(float64))
	if conv["subject"] != "Checkout shows an error" || conv["account_id"].(float64) != 420 {
		t.Fatalf("subject/account defaults: %v", conv)
	}
	path := "/v1/conversations/" + itoa(cid)

	if code, _ := e.do("GET", path, marcus, nil); code != 404 {
		t.Fatalf("another customer must get 404, got %d", code)
	}
	if code, _ := e.do("POST", path+"/messages", marcus, map[string]any{"body": "x"}); code != 404 {
		t.Fatalf("another customer must not post: %d", code)
	}
	if code, _ := e.do("POST", path+"/messages", priya, map[string]any{"body": "x", "kind": "note"}); code != 403 {
		t.Fatalf("customer note: %d", code)
	}
	if code, _ := e.do("POST", "/v1/conversations", austin, map[string]any{"body": "x"}); code != 403 {
		t.Fatalf("operator create: %d", code)
	}

	code, out = e.do("GET", "/v1/conversations?status=waiting", austin, nil)
	list := out["conversations"].([]any)
	if code != 200 || len(list) != 1 || list[0].(map[string]any)["unread"].(float64) != 1 {
		t.Fatalf("operator waiting list: %d %v", code, out)
	}

	e.do("POST", path+"/messages", austin, map[string]any{"body": "Rolled back plugin X", "kind": "note"})
	code, out = e.do("POST", path+"/messages", austin, map[string]any{"body": "Fixed, try again?"})
	if code != 201 || out["conversation"].(map[string]any)["waiting_since"] != nil {
		t.Fatalf("operator reply should clear waiting: %v", out)
	}

	_, out = e.do("GET", path, priya, nil)
	for _, m := range out["messages"].([]any) {
		if strings.Contains(m.(map[string]any)["body"].(string), "Rolled back") {
			t.Fatal("customer can read an internal note")
		}
	}
	if _, ok := out["customer_last_read_message_id"]; ok {
		t.Fatal("read receipts are operator-only")
	}
	_, out = e.do("GET", path, austin, nil)
	if n := len(out["messages"].([]any)); n != 3 {
		t.Fatalf("operator should see 3 messages, got %d", n)
	}

	if code, _ := e.do("PATCH", path, priya, map[string]any{"site_id": 11}); code != 403 {
		t.Fatalf("customer relinking: %d", code)
	}
	if code, _ := e.do("PATCH", path, priya, map[string]any{"status": "snoozed"}); code != 403 {
		t.Fatalf("customer snooze: %d", code)
	}
	code, out = e.do("PATCH", path, priya, map[string]any{"status": "closed"})
	if code != 200 || out["conversation"].(map[string]any)["status"] != "closed" {
		t.Fatalf("customer resolve: %d %v", code, out)
	}
	code, out = e.do("POST", path+"/messages", priya, map[string]any{"body": "Broke again"})
	if code != 201 || out["conversation"].(map[string]any)["status"] != "open" {
		t.Fatalf("reply should reopen: %v", out)
	}

	if code, _ := e.do("PATCH", path, austin, map[string]any{"status": "snoozed", "snoozed_until": time.Now().Add(-time.Hour).Unix()}); code != 400 {
		t.Fatalf("snooze in the past: %d", code)
	}
	if code, _ := e.do("PATCH", path, austin, map[string]any{"status": "snoozed", "snoozed_until": time.Now().Add(time.Hour).Unix(), "site_id": 999}); code != 200 {
		t.Fatalf("operator patch: %d", code)
	}
	if code, _ := e.do("PATCH", path, austin, map[string]any{"bogus": 1}); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
}

func TestBodyHygiene(t *testing.T) {
	e := newEnv(t)
	priya := e.token(42, "customer")
	for name, body := range map[string]string{"empty": "   ", "too long": strings.Repeat("a", maxBodyRunes+1)} {
		if code, _ := e.do("POST", "/v1/conversations", priya, map[string]any{"body": body}); code != 400 {
			t.Errorf("%s: want 400, got %d", name, code)
		}
	}
	_, out := e.do("POST", "/v1/conversations", priya, map[string]any{"body": "hi\x00 <script>alert(1)</script>‮"})
	m := out["messages"].([]any)[0].(map[string]any)["body"].(string)
	if m != "hi <script>alert(1)</script>" {
		t.Fatalf("control chars should be stripped and markup kept as inert text: %q", m)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	priya := e.token(42, "customer")
	_, out := e.do("POST", "/v1/conversations", priya, map[string]any{"body": "hi"})
	path := "/v1/conversations/" + itoa(int64(id(out, "conversation"))) + "/messages"
	limited := false
	for i := 0; i < 25; i++ {
		if code, _ := e.do("POST", path, priya, map[string]any{"body": "spam"}); code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("expected 429 within 25 rapid messages")
	}
}

func TestCORS(t *testing.T) {
	e := newEnv(t)
	pre := func(origin string) *http.Response {
		req, _ := http.NewRequest("OPTIONS", e.ts.URL+"/v1/conversations", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if r := pre(manager); r.StatusCode != 204 || r.Header.Get("Access-Control-Allow-Origin") != manager || r.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("allowed origin: %d %v", r.StatusCode, r.Header)
	}
	if r := pre("https://evil.test"); r.StatusCode != 403 || r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign origin: %d %v", r.StatusCode, r.Header)
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

func TestWebsocketRouting(t *testing.T) {
	e := newEnv(t)
	priyaTok, austinTok := e.token(42, "customer"), e.token(3, "operator")
	priya, ready := e.dial(manager, priyaTok)
	if ready["type"] != "ready" || ready["operators_online"] != false {
		t.Fatalf("ready: %v", ready)
	}
	austin, _ := e.dial(manager, austinTok)
	if p := next(t, priya, "presence"); p["operators_online"] != true {
		t.Fatalf("presence: %v", p)
	}
	marcus, mready := e.dial(manager, e.token(99, "customer"))
	if mready["type"] != "ready" || mready["operators_online"] != true {
		t.Fatalf("late customer should see the operator online: %v", mready)
	}

	_, out := e.do("POST", "/v1/conversations", priyaTok, map[string]any{"body": "Checkout broken"})
	cid := int64(id(out, "conversation"))
	if ev := next(t, austin, "conversation.created"); ev["conversation_id"].(float64) != float64(cid) {
		t.Fatalf("operator should hear about the new conversation: %v", ev)
	}

	path := "/v1/conversations/" + itoa(cid) + "/messages"
	e.do("POST", path, austinTok, map[string]any{"body": "internal: plugin X", "kind": "note"})
	e.do("POST", path, austinTok, map[string]any{"body": "Looking now"})
	// Priya's next message.created must be the public reply, never the note.
	for {
		m := next(t, priya, "message.created")
		body := m["message"].(map[string]any)["body"]
		if body == "internal: plugin X" {
			t.Fatal("note pushed to the customer")
		}
		if body == "Looking now" {
			break
		}
	}

	austin.WriteJSON(map[string]any{"type": "typing", "conversation_id": cid})
	if ev := next(t, priya, "typing"); ev["user"].(map[string]any)["operator"] != true {
		t.Fatalf("typing: %v", ev)
	}

	// Marcus saw nothing about Priya's conversation.
	marcus.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		var m map[string]any
		if err := marcus.ReadJSON(&m); err != nil {
			break // timeout: nothing more queued
		}
		if m["type"] != "presence" {
			t.Fatalf("other customer received %v", m)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
