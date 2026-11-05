// Package store is the SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // pure Go driver: CGO_ENABLED=0 builds everywhere
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrInvalid is returned for a request the rules do not allow.
var ErrInvalid = errors.New("invalid")

// Store wraps the database.
type Store struct {
	db  *sql.DB
	Now func() time.Time
}

// Open opens (creating if needed) the database at path and applies
// pending migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite has one writer anyway, and this volume (tens of
	// conversations a month) never needs more. It also rules out SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	s := &Store{db: db, Now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// SchemaVersion returns PRAGMA user_version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// migrate applies migrations/NNN_*.sql in order, tracking the level in
// PRAGMA user_version. Each file runs in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if current > len(entries) {
		return fmt.Errorf("database schema %d is newer than this build (%d); refusing to run", current, len(entries))
	}
	for i, e := range entries {
		v := i + 1
		if !strings.HasPrefix(e.Name(), fmt.Sprintf("%03d_", v)) {
			return fmt.Errorf("migration %s out of sequence (want %03d_)", e.Name(), v)
		}
		if v <= current {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Message kinds and author kinds.
const (
	KindComment = "comment"
	KindNote    = "note"
	KindEvent   = "event"

	AuthorCustomer = "customer"
	AuthorOperator = "operator"
	AuthorSystem   = "system"

	StatusOpen    = "open"
	StatusSnoozed = "snoozed"
	StatusClosed  = "closed"
)

// Conversation is one support thread.
type Conversation struct {
	ID             int64  `json:"id"`
	CustomerUserID int64  `json:"customer_user_id"`
	CustomerName   string `json:"customer_name"`
	CustomerEmail  string `json:"customer_email"`
	AccountID      *int64 `json:"account_id"`
	SiteID         *int64 `json:"site_id"`
	Subject        string `json:"subject"`
	Status         string `json:"status"`
	SnoozedUntil   *int64 `json:"snoozed_until"`
	AssigneeUserID *int64 `json:"assignee_user_id"`
	WaitingSince   *int64 `json:"waiting_since"`
	LastCustomerAt *int64 `json:"last_customer_at"`
	LastOperatorAt *int64 `json:"last_operator_at"`
	LastMessageAt  int64  `json:"last_message_at"`
	Source         string `json:"source"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`

	// Filled by List for the viewing user.
	Preview string `json:"preview,omitempty"`
	Unread  int    `json:"unread"`
}

// Message is one entry in a conversation.
type Message struct {
	ID             int64  `json:"id"`
	ConversationID int64  `json:"conversation_id"`
	Kind           string `json:"kind"`
	Internal       bool   `json:"internal"`
	AuthorKind     string `json:"author_kind"`
	AuthorUserID   *int64 `json:"author_user_id"`
	AuthorName     string `json:"author_name"`
	Body           string `json:"body"`
	CreatedAt      int64  `json:"created_at"`
	EditedAt       *int64 `json:"edited_at"`
}

const convCols = `c.id, c.customer_user_id, c.customer_name, c.customer_email, c.account_id, c.site_id, c.subject,
	c.status, c.snoozed_until, c.assignee_user_id, c.waiting_since, c.last_customer_at, c.last_operator_at,
	c.last_message_at, c.source, c.created_at, c.updated_at`

const msgCols = `id, conversation_id, kind, internal, author_kind, author_user_id, author_name, body, created_at, edited_at`

type scanner interface{ Scan(dest ...any) error }

func convDest(c *Conversation) []any {
	return []any{&c.ID, &c.CustomerUserID, &c.CustomerName, &c.CustomerEmail, &c.AccountID, &c.SiteID, &c.Subject,
		&c.Status, &c.SnoozedUntil, &c.AssigneeUserID, &c.WaitingSince, &c.LastCustomerAt, &c.LastOperatorAt,
		&c.LastMessageAt, &c.Source, &c.CreatedAt, &c.UpdatedAt}
}

func scanMessage(r scanner) (*Message, error) {
	var m Message
	err := r.Scan(&m.ID, &m.ConversationID, &m.Kind, &m.Internal, &m.AuthorKind, &m.AuthorUserID, &m.AuthorName, &m.Body, &m.CreatedAt, &m.EditedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func getConversation(ctx context.Context, q querier, id int64) (*Conversation, error) {
	var c Conversation
	err := q.QueryRowContext(ctx, "SELECT "+convCols+" FROM conversations c WHERE c.id = ?", id).Scan(convDest(&c)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetConversation returns one conversation.
func (s *Store) GetConversation(ctx context.Context, id int64) (*Conversation, error) {
	return getConversation(ctx, s.db, id)
}

// NewConversation describes a conversation to create.
type NewConversation struct {
	CustomerUserID int64
	CustomerName   string
	CustomerEmail  string
	AccountID      *int64
	SiteID         *int64
	Subject        string
	Source         string
}

// NewMessage describes a message to add.
type NewMessage struct {
	Kind         string
	Internal     bool // only meaningful for events; notes are always internal
	AuthorKind   string
	AuthorUserID int64 // 0 for system
	AuthorName   string
	Body         string
}

// CreateConversation creates a conversation with its first message.
func (s *Store) CreateConversation(ctx context.Context, nc NewConversation, first NewMessage) (*Conversation, *Message, error) {
	if nc.CustomerUserID <= 0 {
		return nil, nil, fmt.Errorf("%w: customer", ErrInvalid)
	}
	if nc.Source == "" {
		nc.Source = "chat"
	}
	now := s.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO conversations
		(customer_user_id, customer_name, customer_email, account_id, site_id, subject, source, last_message_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nc.CustomerUserID, nc.CustomerName, nc.CustomerEmail, nc.AccountID, nc.SiteID, nc.Subject, nc.Source, now, now, now)
	if err != nil {
		return nil, nil, err
	}
	id, _ := res.LastInsertId()
	m, err := addMessageTx(ctx, tx, id, first, now)
	if err != nil {
		return nil, nil, err
	}
	c, err := getConversation(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	return c, m, tx.Commit()
}

// AddMessage appends a message and applies its effect on the conversation.
func (s *Store) AddMessage(ctx context.Context, convID int64, nm NewMessage) (*Message, *Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, err := getConversation(ctx, tx, convID); err != nil {
		return nil, nil, err
	}
	m, err := addMessageTx(ctx, tx, convID, nm, s.Now().Unix())
	if err != nil {
		return nil, nil, err
	}
	c, err := getConversation(ctx, tx, convID)
	if err != nil {
		return nil, nil, err
	}
	return m, c, tx.Commit()
}

func addMessageTx(ctx context.Context, tx *sql.Tx, convID int64, nm NewMessage, now int64) (*Message, error) {
	switch nm.Kind {
	case KindComment:
		nm.Internal = false
	case KindNote:
		nm.Internal = true
		if nm.AuthorKind != AuthorOperator {
			return nil, fmt.Errorf("%w: only operators write notes", ErrInvalid)
		}
	case KindEvent:
	default:
		return nil, fmt.Errorf("%w: kind %q", ErrInvalid, nm.Kind)
	}
	if nm.AuthorKind != AuthorCustomer && nm.AuthorKind != AuthorOperator && nm.AuthorKind != AuthorSystem {
		return nil, fmt.Errorf("%w: author kind %q", ErrInvalid, nm.AuthorKind)
	}
	if strings.TrimSpace(nm.Body) == "" || !utf8.ValidString(nm.Body) {
		return nil, fmt.Errorf("%w: body", ErrInvalid)
	}
	var author any
	if nm.AuthorUserID > 0 {
		author = nm.AuthorUserID
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO messages
		(conversation_id, kind, internal, author_kind, author_user_id, author_name, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		convID, nm.Kind, nm.Internal, nm.AuthorKind, author, nm.AuthorName, nm.Body, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()

	switch {
	case nm.Kind == KindComment && nm.AuthorKind == AuthorCustomer:
		// A customer reply reopens a closed or snoozed thread and starts
		// (or keeps) the waiting clock.
		_, err = tx.ExecContext(ctx, `UPDATE conversations SET last_customer_at = ?, last_message_at = ?,
			waiting_since = COALESCE(waiting_since, ?), status = 'open', snoozed_until = NULL, updated_at = ?
			WHERE id = ?`, now, now, now, now, convID)
	case nm.Kind == KindComment && nm.AuthorKind == AuthorOperator:
		_, err = tx.ExecContext(ctx, `UPDATE conversations SET last_operator_at = ?, last_message_at = ?,
			waiting_since = NULL, updated_at = ? WHERE id = ?`, now, now, now, convID)
	default:
		_, err = tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, now, convID)
	}
	if err != nil {
		return nil, err
	}
	return scanMessage(tx.QueryRowContext(ctx, "SELECT "+msgCols+" FROM messages WHERE id = ?", id))
}

// MessageQuery selects messages for a thread view.
type MessageQuery struct {
	IncludeInternal bool  // operators only
	BeforeID        int64 // 0 = newest
	Limit           int   // default 200
}

// Messages returns up to Limit messages before BeforeID, oldest first.
func (s *Store) Messages(ctx context.Context, convID int64, q MessageQuery) ([]Message, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 200
	}
	before := q.BeforeID
	if before <= 0 {
		before = 1<<63 - 1
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+msgCols+` FROM messages
		WHERE conversation_id = ? AND id < ? AND (? OR internal = 0)
		ORDER BY id DESC LIMIT ?`, convID, before, q.IncludeInternal, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListQuery filters an inbox listing.
type ListQuery struct {
	ViewerID   int64
	Operator   bool
	CustomerID int64  // forced to the viewer for customers
	Status     string // open | snoozed | closed | "" for any
	Waiting    bool   // open and waiting on an operator
	BeforeAt   int64  // cursor: last_message_at of the last row seen
	Limit      int
}

// ListConversations returns conversations newest activity first, with a
// preview and unread count for the viewer.
func (s *Store) ListConversations(ctx context.Context, q ListQuery) ([]Conversation, error) {
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	// Unread counts the other side's comments after the viewer's read mark.
	otherSide := AuthorOperator
	if q.Operator {
		otherSide = AuthorCustomer
	}
	where := []string{"1 = 1"}
	args := []any{otherSide, q.ViewerID}
	if !q.Operator || q.CustomerID > 0 {
		where = append(where, "c.customer_user_id = ?")
		cid := q.CustomerID
		if !q.Operator {
			cid = q.ViewerID
		}
		args = append(args, cid)
	}
	if q.Waiting {
		where = append(where, "c.status = 'open' AND c.waiting_since IS NOT NULL")
	} else if q.Status != "" {
		where = append(where, "c.status = ?")
		args = append(args, q.Status)
	}
	if q.BeforeAt > 0 {
		where = append(where, "c.last_message_at < ?")
		args = append(args, q.BeforeAt)
	}
	args = append(args, q.Limit)
	rows, err := s.db.QueryContext(ctx, "SELECT "+convCols+`,
		COALESCE((SELECT substr(m.body, 1, 200) FROM messages m WHERE m.conversation_id = c.id AND m.kind = 'comment'
			ORDER BY m.id DESC LIMIT 1), ''),
		(SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id AND m.kind = 'comment' AND m.author_kind = ?
			AND m.id > COALESCE((SELECT r.last_read_message_id FROM reads r WHERE r.user_id = ? AND r.conversation_id = c.id), 0))
		FROM conversations c WHERE `+strings.Join(where, " AND ")+`
		ORDER BY c.last_message_at DESC, c.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		dest := append(convDest(&c), &c.Preview, &c.Unread)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Patch is a set of conversation changes. Nil fields are left alone; a
// zero ID clears SiteID, AccountID or AssigneeUserID.
type Patch struct {
	Status         *string `json:"status"`
	SnoozedUntil   *int64  `json:"snoozed_until"`
	SiteID         *int64  `json:"site_id"`
	AccountID      *int64  `json:"account_id"`
	AssigneeUserID *int64  `json:"assignee_user_id"`
	Subject        *string `json:"subject"`
}

// Actor is who made a change, for the event trail.
type Actor struct {
	UserID   int64
	Name     string
	Operator bool
}

// UpdateConversation applies a patch and records events for status, site
// and assignee changes. Permission checks belong to the caller.
func (s *Store) UpdateConversation(ctx context.Context, id int64, p Patch, a Actor) (*Conversation, []Message, error) {
	now := s.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	cur, err := getConversation(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	authorKind := AuthorCustomer
	if a.Operator {
		authorKind = AuthorOperator
	}
	type ev struct {
		body     string
		internal bool
	}
	var events []ev
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if p.Status != nil && *p.Status != cur.Status {
		switch *p.Status {
		case StatusOpen:
			sets = append(sets, "status = 'open'", "snoozed_until = NULL")
			events = append(events, ev{"Reopened by " + a.Name, false})
		case StatusClosed:
			sets = append(sets, "status = 'closed'", "snoozed_until = NULL", "waiting_since = NULL")
			events = append(events, ev{"Closed by " + a.Name, false})
		case StatusSnoozed:
			if p.SnoozedUntil == nil || *p.SnoozedUntil <= now || *p.SnoozedUntil > now+366*86400 {
				return nil, nil, fmt.Errorf("%w: snoozed_until must be in the next year", ErrInvalid)
			}
			sets = append(sets, "status = 'snoozed'", "snoozed_until = ?")
			args = append(args, *p.SnoozedUntil)
			events = append(events, ev{"Snoozed until " + time.Unix(*p.SnoozedUntil, 0).UTC().Format(time.RFC3339) + " by " + a.Name, true})
		default:
			return nil, nil, fmt.Errorf("%w: status %q", ErrInvalid, *p.Status)
		}
	}
	if p.SiteID != nil {
		v := nullID(*p.SiteID)
		sets = append(sets, "site_id = ?")
		args = append(args, v)
		if v == nil {
			events = append(events, ev{"Site unlinked by " + a.Name, true})
		} else {
			events = append(events, ev{fmt.Sprintf("Linked to site #%d by %s", *p.SiteID, a.Name), true})
		}
	}
	if p.AccountID != nil {
		sets = append(sets, "account_id = ?")
		args = append(args, nullID(*p.AccountID))
	}
	if p.AssigneeUserID != nil {
		v := nullID(*p.AssigneeUserID)
		sets = append(sets, "assignee_user_id = ?")
		args = append(args, v)
		if v == nil {
			events = append(events, ev{"Unassigned by " + a.Name, true})
		} else {
			events = append(events, ev{fmt.Sprintf("Assigned to user #%d by %s", *p.AssigneeUserID, a.Name), true})
		}
	}
	if p.Subject != nil {
		sets = append(sets, "subject = ?")
		args = append(args, *p.Subject)
	}
	args = append(args, id)
	if _, err := tx.ExecContext(ctx, "UPDATE conversations SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return nil, nil, err
	}
	var msgs []Message
	for _, e := range events {
		m, err := addMessageTx(ctx, tx, id, NewMessage{Kind: KindEvent, Internal: e.internal, AuthorKind: authorKind,
			AuthorUserID: a.UserID, AuthorName: a.Name, Body: e.body}, now)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, *m)
	}
	c, err := getConversation(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	return c, msgs, tx.Commit()
}

func nullID(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

// WakeSnoozed reopens snoozed conversations whose time has come.
func (s *Store) WakeSnoozed(ctx context.Context) ([]Conversation, []Message, error) {
	now := s.Now().Unix()
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM conversations WHERE status = 'snoozed' AND snoozed_until <= ?", now)
	if err != nil {
		return nil, nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var convs []Conversation
	var msgs []Message
	for _, id := range ids {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return convs, msgs, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conversations SET status = 'open', snoozed_until = NULL, updated_at = ?
			WHERE id = ? AND status = 'snoozed'`, now, id); err != nil {
			tx.Rollback()
			return convs, msgs, err
		}
		m, err := addMessageTx(ctx, tx, id, NewMessage{Kind: KindEvent, Internal: true, AuthorKind: AuthorSystem, Body: "Snooze ended"}, now)
		if err != nil {
			tx.Rollback()
			return convs, msgs, err
		}
		c, err := getConversation(ctx, tx, id)
		if err != nil {
			tx.Rollback()
			return convs, msgs, err
		}
		if err := tx.Commit(); err != nil {
			return convs, msgs, err
		}
		convs = append(convs, *c)
		msgs = append(msgs, *m)
	}
	return convs, msgs, nil
}

// MarkRead advances a user's read mark. It never moves backwards.
func (s *Store) MarkRead(ctx context.Context, userID, convID, messageID int64) error {
	var ok int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE id = ? AND conversation_id = ?", messageID, convID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO reads (user_id, conversation_id, last_read_message_id, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, conversation_id) DO UPDATE SET
			last_read_message_id = MAX(last_read_message_id, excluded.last_read_message_id),
			updated_at = excluded.updated_at`, userID, convID, messageID, s.Now().Unix())
	return err
}

// LastRead returns a user's read mark in a conversation (0 if none).
func (s *Store) LastRead(ctx context.Context, userID, convID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT last_read_message_id FROM reads WHERE user_id = ? AND conversation_id = ?", userID, convID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}
