package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var ctx = context.Background()

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func open(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	s.Now = c.now
	return s, c
}

func cust(body string) NewMessage {
	return NewMessage{Kind: KindComment, AuthorKind: AuthorCustomer, AuthorUserID: 42, AuthorName: "Priya", Body: body}
}
func op(kind, body string) NewMessage {
	return NewMessage{Kind: kind, AuthorKind: AuthorOperator, AuthorUserID: 3, AuthorName: "Austin", Body: body}
}

func TestMigrateIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat.db")
	for i := 0; i < 2; i++ {
		s, err := Open(p)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		v, _ := s.SchemaVersion(ctx)
		if v != 1 {
			t.Fatalf("schema version %d", v)
		}
		s.Close()
	}
}

func TestWaitingLifecycle(t *testing.T) {
	s, clk := open(t)
	site := int64(11)
	c, m, err := s.CreateConversation(ctx, NewConversation{CustomerUserID: 42, CustomerName: "Priya", SiteID: &site, Subject: "Checkout broken"}, cust("Checkout shows an error"))
	if err != nil {
		t.Fatal(err)
	}
	if c.WaitingSince == nil || *c.WaitingSince != clk.t.Unix() || c.Status != StatusOpen || m.Kind != KindComment {
		t.Fatalf("new conversation should be waiting: %+v", c)
	}
	start := *c.WaitingSince

	clk.add(time.Minute)
	if _, c, _ = s.AddMessage(ctx, c.ID, cust("Also on mobile")); *c.WaitingSince != start {
		t.Fatal("second customer message must keep the original waiting time")
	}

	clk.add(time.Minute)
	beforeNote := c.LastMessageAt
	if _, c, _ = s.AddMessage(ctx, c.ID, op(KindNote, "rolled back plugin")); c.WaitingSince == nil || c.LastMessageAt != beforeNote {
		t.Fatal("a note must not answer the customer or bump last_message_at")
	}

	clk.add(time.Minute)
	if _, c, _ = s.AddMessage(ctx, c.ID, op(KindComment, "Fixed")); c.WaitingSince != nil {
		t.Fatal("operator reply should clear waiting")
	}

	st := StatusClosed
	c, evs, err := s.UpdateConversation(ctx, c.ID, Patch{Status: &st}, Actor{UserID: 3, Name: "Austin", Operator: true})
	if err != nil || c.Status != StatusClosed || len(evs) != 1 || evs[0].Internal {
		t.Fatalf("close: %v %+v %+v", err, c, evs)
	}

	clk.add(time.Hour)
	if _, c, _ = s.AddMessage(ctx, c.ID, cust("It broke again")); c.Status != StatusOpen || c.WaitingSince == nil {
		t.Fatal("customer reply should reopen a closed conversation and start waiting")
	}
}

func TestCustomersNeverGetInternalMessages(t *testing.T) {
	s, _ := open(t)
	c, _, _ := s.CreateConversation(ctx, NewConversation{CustomerUserID: 42}, cust("hi"))
	s.AddMessage(ctx, c.ID, op(KindNote, "secret"))
	until := s.Now().Add(time.Hour).Unix()
	st := StatusSnoozed
	if _, _, err := s.UpdateConversation(ctx, c.ID, Patch{Status: &st, SnoozedUntil: &until}, Actor{UserID: 3, Name: "Austin", Operator: true}); err != nil {
		t.Fatal(err)
	}
	pub, _ := s.Messages(ctx, c.ID, MessageQuery{})
	all, _ := s.Messages(ctx, c.ID, MessageQuery{IncludeInternal: true})
	if len(pub) != 1 || len(all) != 3 {
		t.Fatalf("customer view %d (want 1), operator view %d (want 3)", len(pub), len(all))
	}
	for _, m := range pub {
		if m.Internal {
			t.Fatal("internal message leaked")
		}
	}
}

func TestCustomerCannotWriteNotesEvenViaStore(t *testing.T) {
	s, _ := open(t)
	c, _, _ := s.CreateConversation(ctx, NewConversation{CustomerUserID: 42}, cust("hi"))
	bad := cust("x")
	bad.Kind = KindNote
	if _, _, err := s.AddMessage(ctx, c.ID, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestSnoozeAndWake(t *testing.T) {
	s, clk := open(t)
	c, _, _ := s.CreateConversation(ctx, NewConversation{CustomerUserID: 42}, cust("hi"))
	a := Actor{UserID: 3, Name: "Austin", Operator: true}
	st := StatusSnoozed
	past := clk.t.Add(-time.Minute).Unix()
	if _, _, err := s.UpdateConversation(ctx, c.ID, Patch{Status: &st, SnoozedUntil: &past}, a); !errors.Is(err, ErrInvalid) {
		t.Fatal("snooze into the past should be rejected")
	}
	until := clk.t.Add(time.Hour).Unix()
	if _, _, err := s.UpdateConversation(ctx, c.ID, Patch{Status: &st, SnoozedUntil: &until}, a); err != nil {
		t.Fatal(err)
	}
	if woke, _, _ := s.WakeSnoozed(ctx); len(woke) != 0 {
		t.Fatal("woke too early")
	}
	clk.add(2 * time.Hour)
	woke, msgs, err := s.WakeSnoozed(ctx)
	if err != nil || len(woke) != 1 || woke[0].Status != StatusOpen || woke[0].SnoozedUntil != nil || !msgs[0].Internal {
		t.Fatalf("wake: %v %+v %+v", err, woke, msgs)
	}
}

func TestListScopesAndUnread(t *testing.T) {
	s, clk := open(t)
	a, _, _ := s.CreateConversation(ctx, NewConversation{CustomerUserID: 42, Subject: "A"}, cust("one"))
	clk.add(time.Second)
	s.AddMessage(ctx, a.ID, cust("two"))
	clk.add(time.Second)
	other := cust("not yours")
	other.AuthorUserID = 99
	b, _, _ := s.CreateConversation(ctx, NewConversation{CustomerUserID: 99, Subject: "B"}, other)

	mine, _ := s.ListConversations(ctx, ListQuery{ViewerID: 42, CustomerID: 99}) // customers can't widen scope
	if len(mine) != 1 || mine[0].ID != a.ID {
		t.Fatalf("customer list leaked: %+v", mine)
	}
	waiting, _ := s.ListConversations(ctx, ListQuery{ViewerID: 3, Operator: true, Waiting: true})
	if len(waiting) != 2 || waiting[0].ID != b.ID {
		t.Fatalf("operator waiting list: %+v", waiting)
	}
	if waiting[1].Unread != 2 || waiting[1].Preview != "two" {
		t.Fatalf("unread/preview: %+v", waiting[1])
	}
	msgs, _ := s.Messages(ctx, a.ID, MessageQuery{IncludeInternal: true})
	if err := s.MarkRead(ctx, 3, a.ID, msgs[len(msgs)-1].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRead(ctx, 3, a.ID, msgs[0].ID); err != nil { // older mark must not move it back
		t.Fatal(err)
	}
	waiting, _ = s.ListConversations(ctx, ListQuery{ViewerID: 3, Operator: true, Waiting: true})
	if waiting[1].Unread != 0 {
		t.Fatalf("unread after read: %d", waiting[1].Unread)
	}
	if err := s.MarkRead(ctx, 3, a.ID, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatal("read mark for a message outside the conversation must fail")
	}
}
