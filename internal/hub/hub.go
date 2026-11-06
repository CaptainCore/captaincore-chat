// Package hub fans live events out to connected websocket clients.
//
// Routing rule, enforced here so no handler can get it wrong:
//   - operators receive every event;
//   - a customer receives an event only for their own conversation, and
//     never an internal one (notes, internal events).
package hub

import (
	"encoding/json"
	"sync"
)

// Client is one websocket connection as the hub sees it.
type Client struct {
	UserID   int64
	Operator bool

	send     chan []byte
	done     chan struct{}
	killOnce sync.Once
	onKill   func()
}

// NewClient returns a client with a bounded outbound queue. onKill runs once
// when the hub drops it (slow consumer) or the connection ends. It may run
// while the hub holds its lock, so it must only close the connection: never
// call back into the hub from it (Unregister belongs to the reader's defer).
func NewClient(userID int64, operator bool, onKill func()) *Client {
	return &Client{UserID: userID, Operator: operator, send: make(chan []byte, 64), done: make(chan struct{}), onKill: onKill}
}

// Outbound is the queue the connection's writer drains.
func (c *Client) Outbound() <-chan []byte { return c.send }

// Done is closed when the client is killed.
func (c *Client) Done() <-chan struct{} { return c.done }

// Kill stops the client. Safe to call more than once.
func (c *Client) Kill() {
	c.killOnce.Do(func() {
		close(c.done)
		if c.onKill != nil {
			c.onKill()
		}
	})
}

// Send queues a frame, killing the client if it cannot keep up. The send
// channel is never closed, so a racing Send can't panic.
func (c *Client) Send(b []byte) {
	select {
	case <-c.done:
	case c.send <- b:
	default:
		c.Kill()
	}
}

// Hub tracks connected clients.
type Hub struct {
	mu        sync.RWMutex
	clients   map[*Client]struct{}
	operators int
}

// New returns an empty hub.
func New() *Hub { return &Hub{clients: make(map[*Client]struct{})} }

// Register adds a client. The first operator to connect announces presence.
func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
	if c.Operator {
		h.operators++
		if h.operators == 1 {
			h.presenceLocked(true)
		}
	}
}

// Unregister removes a client. The last operator to leave announces absence.
func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	if c.Operator {
		h.operators--
		if h.operators == 0 {
			h.presenceLocked(false)
		}
	}
}

// OperatorsOnline reports whether any operator is connected.
func (h *Hub) OperatorsOnline() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.operators > 0
}

// Count returns the number of connected clients.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) presenceLocked(online bool) {
	b, _ := json.Marshal(map[string]any{"type": "presence", "operators_online": online})
	for c := range h.clients {
		if !c.Operator {
			c.Send(b)
		}
	}
}

// Publish delivers v to operators and, unless internal, to the customer
// who owns the conversation. skip (may be nil) is not sent to, e.g. the
// connection that produced a typing event.
func (h *Hub) Publish(v any, customerID int64, internal bool, skip *Client) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return h.PublishRaw(b, customerID, internal, skip)
}

// PublishRaw is Publish for an already encoded frame. It returns how many
// clients it was queued for.
func (h *Hub) PublishRaw(b []byte, customerID int64, internal bool, skip *Client) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for c := range h.clients {
		if c == skip {
			continue
		}
		if c.Operator || (!internal && customerID > 0 && c.UserID == customerID) {
			c.Send(b)
			n++
		}
	}
	return n
}

// PublishToOperators delivers v to operators only.
func (h *Hub) PublishToOperators(v any, skip *Client) { h.Publish(v, 0, true, skip) }

// CloseAll kills every client (shutdown).
func (h *Hub) CloseAll() {
	h.mu.RLock()
	list := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		list = append(list, c)
	}
	h.mu.RUnlock()
	for _, c := range list {
		c.Kill()
	}
}
