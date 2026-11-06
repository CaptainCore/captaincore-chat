package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/CaptainCore/captaincore-chat/internal/auth"
	"github.com/CaptainCore/captaincore-chat/internal/hub"
	"github.com/gorilla/websocket"
)

const (
	wsAuthWait  = 10 * time.Second
	wsPongWait  = 60 * time.Second
	wsPingEvery = 25 * time.Second
	wsWriteWait = 10 * time.Second
	wsReadLimit = 16 << 10
	closeAuth   = 4001 // token missing, invalid or expired: fetch a new one and reconnect
)

// inbound is every frame a client may send.
//
//	{"type":"auth","token":"v1...."}            first frame, and to refresh before expiry
//	{"type":"typing","conversation_id":12}                       customer
//	{"type":"typing","conversation_id":12,"customer_user_id":42} operator
//	{"type":"ping"}
type inbound struct {
	Type           string `json:"type"`
	Token          string `json:"token"`
	ConversationID int64  `json:"conversation_id"`
	CustomerUserID int64  `json:"customer_user_id"`
}

// typingEvent is the only frame the relay builds itself.
type typingEvent struct {
	Type           string     `json:"type"`
	ConversationID int64      `json:"conversation_id"`
	User           typingUser `json:"user"`
}

type typingUser struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Operator bool   `json:"operator"`
}

func (s *Server) upgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:   2048,
		WriteBufferSize:  4096,
		HandshakeTimeout: 10 * time.Second,
		// Browsers always send Origin; it must be on the allowlist. A client
		// with no Origin is not a browser, so cross-site hijacking can't
		// apply, and it still has to present a valid token.
		CheckOrigin: func(r *http.Request) bool {
			o := r.Header.Get("Origin")
			return o == "" || s.cfg.OriginAllowed(o)
		},
	}
}

// GET /v1/ws. The token comes in the first frame, never in the URL.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader().Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader already wrote 403/400
	}
	defer conn.Close()
	conn.SetReadLimit(wsReadLimit)

	conn.SetReadDeadline(time.Now().Add(wsAuthWait))
	var first inbound
	if err := conn.ReadJSON(&first); err != nil || first.Type != "auth" {
		closeWith(conn, closeAuth, "auth required")
		return
	}
	claims, err := s.verifier.Verify(first.Token)
	if err != nil {
		s.log.Info("ws token rejected", "reason", err.Error())
		closeWith(conn, closeAuth, "invalid token")
		return
	}

	var cur atomic.Pointer[auth.Claims]
	cur.Store(claims)
	client := hub.NewClient(claims.Subject, claims.IsOperator(), func() { conn.Close() })
	ready, _ := json.Marshal(map[string]any{
		"type":             "ready",
		"user":             map[string]any{"id": claims.Subject, "name": displayName(claims), "role": claims.Role},
		"operators_online": s.hub.OperatorsOnline(),
		"expires":          claims.Expires,
	})
	client.Send(ready)
	s.hub.Register(client)
	defer s.hub.Unregister(client)
	defer client.Kill()

	go s.wsWriter(conn, client, &cur)

	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(wsPongWait)) })
	for {
		var in inbound
		if err := conn.ReadJSON(&in); err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		c := cur.Load()
		switch in.Type {
		case "auth":
			// Refresh: same user and role only.
			nc, err := s.verifier.Verify(in.Token)
			if err != nil || nc.Subject != c.Subject || nc.Role != c.Role {
				closeWith(conn, closeAuth, "invalid token")
				return
			}
			cur.Store(nc)
			b, _ := json.Marshal(map[string]any{"type": "reauthed", "expires": nc.Expires})
			client.Send(b)
		case "typing":
			s.wsTyping(c, client, in)
		case "ping":
			client.Send([]byte(`{"type":"pong"}`))
		}
	}
}

// wsTyping relays a typing indicator. The relay stores nothing, so it does
// not know who owns a conversation:
//   - an operator names the customer, and operators are trusted;
//   - a customer's frame goes to operators only, carrying the sender's id,
//     and the inbox ignores it unless that id owns the conversation. A
//     customer can't reach another customer this way.
func (s *Server) wsTyping(c *auth.Claims, client *hub.Client, in inbound) {
	if in.ConversationID <= 0 || !s.typingLimit.Allow(strconv.FormatInt(c.Subject, 10)+":"+strconv.FormatInt(in.ConversationID, 10)) {
		return
	}
	ev := typingEvent{Type: "typing", ConversationID: in.ConversationID, User: typingUser{ID: c.Subject, Name: displayName(c), Operator: c.IsOperator()}}
	if c.IsOperator() {
		s.hub.Publish(ev, in.CustomerUserID, in.CustomerUserID <= 0, client) // the customer, plus other operators
		return
	}
	s.hub.PublishToOperators(ev, nil)
}

// wsWriter is the only goroutine that writes to conn. It also enforces
// token expiry: an expired session is closed with 4001 so the client
// fetches a fresh token from the Manager and reconnects.
func (s *Server) wsWriter(conn *websocket.Conn, client *hub.Client, cur *atomic.Pointer[auth.Claims]) {
	ping := time.NewTicker(wsPingEvery)
	defer ping.Stop()
	expiry := time.NewTicker(5 * time.Second)
	defer expiry.Stop()
	for {
		select {
		case <-client.Done():
			return
		case b := <-client.Outbound():
			conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				client.Kill()
				return
			}
		case <-ping.C:
			conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				client.Kill()
				return
			}
		case <-expiry.C:
			if s.verifier.Now().Unix() >= cur.Load().Expires {
				closeWith(conn, closeAuth, "token expired")
				client.Kill()
				return
			}
		}
	}
}

func closeWith(conn *websocket.Conn, code int, reason string) {
	msg := websocket.FormatCloseMessage(code, reason)
	_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
}
