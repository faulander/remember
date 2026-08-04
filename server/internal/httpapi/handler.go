// Package httpapi contains the server's bounded HTTP surface.
package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/faulander/remember/server/internal/database"
)

// State controls whether the service may receive new work.
type State struct{ ready atomic.Bool }

func (s *State) MarkReady()    { s.ready.Store(true) }
func (s *State) MarkDraining() { s.ready.Store(false) }
func (s *State) Ready() bool   { return s.ready.Load() }

// New returns the minimal foundation API.
func New(db *sql.DB, state *State, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	api := &handler{db: db, state: state}
	return requestLog(logger, securityHeaders(api))
}

type handler struct {
	db    *sql.DB
	state *State
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		h.probe(w, r, false)
	case "/readyz":
		h.probe(w, r, true)
	default:
		writeJSON(w, r, http.StatusNotFound, map[string]string{"status": "not_found"})
	}
}

func (h *handler) probe(w http.ResponseWriter, r *http.Request, readiness bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, r, http.StatusMethodNotAllowed, map[string]string{"status": "method_not_allowed"})
		return
	}
	if !readiness {
		writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !h.state.Ready() {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 750*time.Millisecond)
	defer cancel()
	if err := database.Ready(ctx, h.db); err != nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := newRequestID()
		w.Header().Set("X-Request-ID", requestID)
		capture := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(capture, r)
		logger.Info("http_request",
			"event_code", "HTTP_REQUEST",
			"request_id", requestID,
			"method", r.Method,
			"route", knownRoute(r.URL.Path),
			"status", capture.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func newRequestID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(value[:])
}

func knownRoute(path string) string {
	switch path {
	case "/healthz", "/readyz":
		return path
	default:
		return "unknown"
	}
}
