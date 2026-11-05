package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/CaptainCore/captaincore-chat/internal/auth"
	"github.com/CaptainCore/captaincore-chat/internal/store"
)

const (
	maxBodyRunes    = 10000
	maxSubjectRunes = 200
)

// event is every frame the hub pushes.
type event struct {
	Type           string              `json:"type"`
	ConversationID int64               `json:"conversation_id,omitempty"`
	Conversation   *store.Conversation `json:"conversation,omitempty"`
	Message        *store.Message      `json:"message,omitempty"`
	User           *typingUser         `json:"user,omitempty"`
}

type typingUser struct {
	Name     string `json:"name"`
	Operator bool   `json:"operator"`
}

func actor(c *auth.Claims) store.Actor {
	return store.Actor{UserID: c.Subject, Name: displayName(c), Operator: c.IsOperator()}
}

func displayName(c *auth.Claims) string {
	if n, err := cleanLine(c.Name, 100); err == nil {
		return n
	}
	return "User #" + strconv.FormatInt(c.Subject, 10)
}

// loadConversation returns the conversation if the caller may see it. A
// customer asking for someone else's conversation gets the same 404 as a
// missing one, so IDs reveal nothing.
func (s *Server) loadConversation(w http.ResponseWriter, r *http.Request, c *auth.Claims) (*store.Conversation, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "Conversation not found.")
		return nil, false
	}
	conv, err := s.store.GetConversation(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !c.IsOperator() && conv.CustomerUserID != c.Subject) {
		writeError(w, http.StatusNotFound, "not_found", "Conversation not found.")
		return nil, false
	}
	if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	return conv, true
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "Something went wrong.")
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user":             map[string]any{"id": c.Subject, "name": displayName(c), "role": c.Role},
		"operators_online": s.hub.OperatorsOnline(),
		"expires":          c.Expires,
	})
}

// GET /v1/conversations?status=open|snoozed|closed|waiting&before=<last_message_at>&limit=N
func (s *Server) handleList(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	q := store.ListQuery{ViewerID: c.Subject, Operator: c.IsOperator()}
	switch st := r.URL.Query().Get("status"); st {
	case "", store.StatusOpen, store.StatusSnoozed, store.StatusClosed:
		q.Status = st
	case "waiting":
		q.Waiting = true
	default:
		writeError(w, http.StatusBadRequest, "bad_status", "status must be open, snoozed, closed or waiting.")
		return
	}
	q.BeforeAt, _ = strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	q.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if c.IsOperator() {
		q.CustomerID, _ = strconv.ParseInt(r.URL.Query().Get("customer"), 10, 64)
	}
	list, err := s.store.ListConversations(r.Context(), q)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if list == nil {
		list = []store.Conversation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": list})
}

type createRequest struct {
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	SiteID    *int64 `json:"site_id"`
	AccountID *int64 `json:"account_id"`
}

// POST /v1/conversations (customers)
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	if c.IsOperator() {
		writeError(w, http.StatusForbidden, "customers_only", "Operators can't start conversations yet.")
		return
	}
	var req createRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Request body is not valid.")
		return
	}
	body, err := cleanBody(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = firstLine(body, 80)
	}
	if subject, err = cleanLine(subject, maxSubjectRunes); err != nil {
		writeError(w, http.StatusBadRequest, "bad_subject", err.Error())
		return
	}
	// Site and account links must come from the token, never trusted input.
	if req.SiteID != nil && !c.HasSite(*req.SiteID) {
		writeError(w, http.StatusForbidden, "site", "That site isn't on your account.")
		return
	}
	if req.AccountID != nil && !c.HasAccount(*req.AccountID) {
		writeError(w, http.StatusForbidden, "account", "That account isn't yours.")
		return
	}
	if req.AccountID == nil && len(c.Accounts) == 1 {
		req.AccountID = &c.Accounts[0]
	}
	if !s.createLimit.Allow("u" + strconv.FormatInt(c.Subject, 10)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Slow down a little and try again.")
		return
	}
	conv, msg, err := s.store.CreateConversation(r.Context(),
		store.NewConversation{CustomerUserID: c.Subject, CustomerName: displayName(c), CustomerEmail: c.Email,
			AccountID: req.AccountID, SiteID: req.SiteID, Subject: subject},
		store.NewMessage{Kind: store.KindComment, AuthorKind: store.AuthorCustomer, AuthorUserID: c.Subject, AuthorName: displayName(c), Body: body})
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.hub.Publish(event{Type: "conversation.created", ConversationID: conv.ID, Conversation: conv, Message: msg}, conv.CustomerUserID, false, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"conversation": conv, "messages": []*store.Message{msg}})
}

// GET /v1/conversations/{id}?before=<message id>&limit=N
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	conv, ok := s.loadConversation(w, r, c)
	if !ok {
		return
	}
	q := store.MessageQuery{IncludeInternal: c.IsOperator()}
	q.BeforeID, _ = strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	q.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	msgs, err := s.store.Messages(r.Context(), conv.ID, q)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}
	out := map[string]any{"conversation": conv, "messages": msgs}
	if c.IsOperator() {
		// Powers the "Seen by Priya" receipt in the operator view.
		seen, err := s.store.LastRead(r.Context(), conv.CustomerUserID, conv.ID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		out["customer_last_read_message_id"] = seen
	}
	writeJSON(w, http.StatusOK, out)
}

type messageRequest struct {
	Body string `json:"body"`
	Kind string `json:"kind"` // comment (default) | note (operators)
}

// POST /v1/conversations/{id}/messages
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	conv, ok := s.loadConversation(w, r, c)
	if !ok {
		return
	}
	var req messageRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Request body is not valid.")
		return
	}
	kind := req.Kind
	switch kind {
	case "", store.KindComment:
		kind = store.KindComment
	case store.KindNote:
		if !c.IsOperator() {
			writeError(w, http.StatusForbidden, "operators_only", "Only operators can write internal notes.")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad_kind", "kind must be comment or note.")
		return
	}
	body, err := cleanBody(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	if !s.msgLimit.Allow("u" + strconv.FormatInt(c.Subject, 10)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Slow down a little and try again.")
		return
	}
	authorKind := store.AuthorCustomer
	if c.IsOperator() {
		authorKind = store.AuthorOperator
	}
	msg, conv, err := s.store.AddMessage(r.Context(), conv.ID, store.NewMessage{Kind: kind, AuthorKind: authorKind,
		AuthorUserID: c.Subject, AuthorName: displayName(c), Body: body})
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.hub.Publish(event{Type: "message.created", ConversationID: conv.ID, Message: msg}, conv.CustomerUserID, msg.Internal, nil)
	s.hub.Publish(event{Type: "conversation.updated", ConversationID: conv.ID, Conversation: conv}, conv.CustomerUserID, false, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"message": msg, "conversation": conv})
}

// PATCH /v1/conversations/{id}
// Customers may only resolve (status closed) or reopen (status open) their
// own conversation. Operators may change everything.
func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	conv, ok := s.loadConversation(w, r, c)
	if !ok {
		return
	}
	var p store.Patch
	if err := decode(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Request body is not valid.")
		return
	}
	if !c.IsOperator() {
		statusOK := p.Status != nil && (*p.Status == store.StatusClosed || *p.Status == store.StatusOpen)
		if !statusOK || p.SnoozedUntil != nil || p.SiteID != nil || p.AccountID != nil || p.AssigneeUserID != nil || p.Subject != nil {
			writeError(w, http.StatusForbidden, "operators_only", "You can resolve or reopen a conversation; operators handle the rest.")
			return
		}
	}
	if p.Subject != nil {
		v, err := cleanLine(*p.Subject, maxSubjectRunes)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_subject", err.Error())
			return
		}
		p.Subject = &v
	}
	conv, evs, err := s.store.UpdateConversation(r.Context(), conv.ID, p, actor(c))
	if errors.Is(err, store.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), "invalid: "))
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	for i := range evs {
		m := evs[i]
		s.hub.Publish(event{Type: "message.created", ConversationID: conv.ID, Message: &m}, conv.CustomerUserID, m.Internal, nil)
	}
	s.hub.Publish(event{Type: "conversation.updated", ConversationID: conv.ID, Conversation: conv}, conv.CustomerUserID, false, nil)
	writeJSON(w, http.StatusOK, map[string]any{"conversation": conv, "events": evs})
}

type readRequest struct {
	MessageID int64 `json:"message_id"`
}

// POST /v1/conversations/{id}/read
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request, c *auth.Claims) {
	conv, ok := s.loadConversation(w, r, c)
	if !ok {
		return
	}
	var req readRequest
	if err := decode(r, &req); err != nil || req.MessageID <= 0 {
		writeError(w, http.StatusBadRequest, "bad_json", "message_id is required.")
		return
	}
	if err := s.store.MarkRead(r.Context(), c.Subject, conv.ID, req.MessageID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Message not found in this conversation.")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if !c.IsOperator() {
		// Live "Seen" receipt for the operator view.
		s.hub.PublishToOperators(map[string]any{"type": "read", "conversation_id": conv.ID, "message_id": req.MessageID}, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── text hygiene ──
// Bodies are stored and served as plain text. Clients must escape on
// render; the server additionally strips control characters so nothing
// odd reaches a terminal, a log or an email.

func cleanBody(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("Message must be valid UTF-8.")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' || (r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩') {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("Message is empty.")
	}
	if utf8.RuneCountInString(s) > maxBodyRunes {
		return "", errors.New("Message is too long (10,000 characters max).")
	}
	return s, nil
}

func cleanLine(s string, max int) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("Text must be valid UTF-8.")
	}
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	s = strings.Map(func(r rune) rune {
		if (r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩') {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return "", errors.New("Text is empty.")
	}
	if utf8.RuneCountInString(s) > max {
		return "", errors.New("Text is too long.")
	}
	return s, nil
}

func firstLine(s string, max int) string {
	line, _, _ := strings.Cut(s, "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > max {
		r := []rune(line)
		line = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return line
}
