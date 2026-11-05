// Package api is the HTTP + websocket surface.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/CaptainCore/captaincore-chat/internal/auth"
	"github.com/CaptainCore/captaincore-chat/internal/config"
	"github.com/CaptainCore/captaincore-chat/internal/hub"
	"github.com/CaptainCore/captaincore-chat/internal/store"
	"github.com/CaptainCore/captaincore-chat/version"
)

const maxBodyBytes = 64 << 10

// Server wires config, store, verifier and hub to HTTP.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	verifier *auth.Verifier
	hub      *hub.Hub
	log      *slog.Logger

	msgLimit    *limiter // messages per user
	createLimit *limiter // new conversations per user
	typingLimit *limiter // typing frames per user+conversation
}

// New builds a Server.
func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Server {
	return &Server{
		cfg:         cfg,
		store:       st,
		verifier:    auth.NewVerifier(cfg.Keys(), cfg.Issuer, cfg.Audience),
		hub:         hub.New(),
		log:         log,
		msgLimit:    newLimiter(20, time.Minute),
		createLimit: newLimiter(5, time.Minute),
		typingLimit: newLimiter(1, 2*time.Second),
	}
}

// Verifier exposes the token verifier (tests swap its clock).
func (s *Server) Verifier() *auth.Verifier { return s.verifier }

// Hub exposes the hub (tests and presence checks).
func (s *Server) Hub() *hub.Hub { return s.hub }

// Handler returns the full HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/ws", s.handleWS)

	authed := func(h func(http.ResponseWriter, *http.Request, *auth.Claims)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, ok := s.authenticate(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized", "A valid token is required.")
				return
			}
			h(w, r, c)
		}
	}
	mux.HandleFunc("GET /v1/me", authed(s.handleMe))
	mux.HandleFunc("GET /v1/conversations", authed(s.handleList))
	mux.HandleFunc("POST /v1/conversations", authed(s.handleCreate))
	mux.HandleFunc("GET /v1/conversations/{id}", authed(s.handleGet))
	mux.HandleFunc("PATCH /v1/conversations/{id}", authed(s.handlePatch))
	mux.HandleFunc("POST /v1/conversations/{id}/messages", authed(s.handlePostMessage))
	mux.HandleFunc("POST /v1/conversations/{id}/read", authed(s.handleRead))

	return s.recoverer(s.logRequests(s.securityHeaders(s.cors(mux))))
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		// No WriteTimeout: websockets are long-lived. REST handlers are all
		// quick SQLite calls.
	}
	go s.wakeLoop(ctx)

	errc := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.cfg.Listen, "version", version.Version)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.log.Info("shutting down")
	s.hub.CloseAll()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// wakeLoop reopens snoozed conversations when their time comes.
func (s *Server) wakeLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.WakeSnoozed(ctx)
		}
	}
}

// WakeSnoozed runs one snooze sweep and publishes the results.
func (s *Server) WakeSnoozed(ctx context.Context) {
	convs, msgs, err := s.store.WakeSnoozed(ctx)
	if err != nil {
		s.log.Error("wake snoozed", "err", err)
	}
	for i := range convs {
		c, m := convs[i], msgs[i]
		s.hub.PublishToOperators(event{Type: "message.created", ConversationID: c.ID, Message: &m}, nil)
		s.hub.Publish(event{Type: "conversation.updated", ConversationID: c.ID, Conversation: &c}, c.CustomerUserID, false, nil)
	}
}

// authenticate reads "Authorization: Bearer <token>". Tokens never travel
// in query strings, which end up in access logs.
func (s *Server) authenticate(r *http.Request) (*auth.Claims, bool) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return nil, false
	}
	c, err := s.verifier.Verify(strings.TrimSpace(tok))
	if err != nil {
		s.log.Info("token rejected", "reason", err.Error(), "path", r.URL.Path)
		return nil, false
	}
	return c, true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database", "Database unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version.Version})
}

// ── middleware ──

// cors allows the configured Manager origins. Auth is a bearer header, not
// a cookie, so there is no Allow-Credentials and no CSRF surface.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && s.cfg.OriginAllowed(origin)
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			if allowed && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack passes the websocket upgrade through the logging wrapper.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.status = http.StatusSwitchingProtocols
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// Path only: never the query string or headers.
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "value", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "Something went wrong.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ── JSON helpers ──

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// decode reads exactly one JSON object with no unknown fields.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON object")
	}
	return nil
}
