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
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/CaptainCore/captaincore-chat/internal/auth"
	"github.com/CaptainCore/captaincore-chat/internal/config"
	"github.com/CaptainCore/captaincore-chat/internal/hub"
	"github.com/CaptainCore/captaincore-chat/version"
)

// Server wires config, verifier and hub to HTTP. It stores nothing: the
// Manager owns every conversation and calls /internal/publish after each
// change.
type Server struct {
	cfg      *config.Config
	verifier *auth.Verifier
	hub      *hub.Hub
	log      *slog.Logger
	replay   *replayCache

	typingLimit *limiter // typing frames per user+conversation
	draftLimit  *limiter // customer draft previews per user+conversation
}

// New builds a Server.
func New(cfg *config.Config, log *slog.Logger) *Server {
	v := auth.NewVerifier(cfg.Keys(), cfg.Issuer, cfg.Audience)
	return &Server{
		cfg:         cfg,
		verifier:    v,
		hub:         hub.New(),
		log:         log,
		replay:      newReplayCache(2 * v.Skew),
		typingLimit: newLimiter(1, 2*time.Second),
		draftLimit:  newLimiter(4, 2*time.Second),
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
	mux.HandleFunc("POST /internal/publish", s.handlePublish)
	return s.recoverer(s.logRequests(s.securityHeaders(mux)))
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
		// No WriteTimeout: websockets are long-lived.
	}
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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version.Version})
}

// displayName is the token's name, cleaned for display.
func displayName(c *auth.Claims) string {
	n := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, c.Name))
	if n == "" {
		return "User #" + strconv.FormatInt(c.Subject, 10)
	}
	if r := []rune(n); len(r) > 100 {
		n = string(r[:100])
	}
	return n
}

// ── middleware ──

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
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
