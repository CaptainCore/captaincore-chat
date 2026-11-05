package hub

import (
	"encoding/json"
	"testing"
)

func drain(c *Client) []map[string]any {
	var out []map[string]any
	for {
		select {
		case b := <-c.Outbound():
			var m map[string]any
			json.Unmarshal(b, &m)
			out = append(out, m)
		default:
			return out
		}
	}
}

func TestRouting(t *testing.T) {
	h := New()
	op := NewClient(3, true, nil)
	priya := NewClient(42, false, nil)
	marcus := NewClient(99, false, nil)
	h.Register(priya)
	h.Register(marcus)
	if got := drain(priya); len(got) != 0 {
		t.Fatalf("no presence before an operator joins: %v", got)
	}
	h.Register(op)
	if got := drain(priya); len(got) != 1 || got[0]["operators_online"] != true {
		t.Fatalf("presence on: %v", got)
	}
	drain(marcus)

	h.Publish(map[string]any{"type": "message.created", "n": 1}, 42, false, nil)
	h.Publish(map[string]any{"type": "message.created", "n": 2}, 42, true, nil) // a note
	if got := drain(op); len(got) != 2 {
		t.Fatalf("operator should get both: %v", got)
	}
	if got := drain(priya); len(got) != 1 || got[0]["n"] != float64(1) {
		t.Fatalf("customer should get only the public event: %v", got)
	}
	if got := drain(marcus); len(got) != 0 {
		t.Fatalf("other customer got someone else's event: %v", got)
	}

	h.Unregister(op)
	if got := drain(priya); len(got) != 1 || got[0]["operators_online"] != false {
		t.Fatalf("presence off: %v", got)
	}
}

func TestSlowClientIsKilledNotBlocking(t *testing.T) {
	h := New()
	killed := false
	c := NewClient(3, true, func() { killed = true })
	h.Register(c)
	for i := 0; i < 100; i++ { // queue holds 64
		h.PublishToOperators(map[string]int{"i": i}, nil)
	}
	if !killed {
		t.Fatal("slow client should be killed")
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("done not closed")
	}
	c.Send([]byte("x")) // must not panic after kill
}
