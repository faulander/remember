// Package httpapi contains the server's bounded HTTP surface.
package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/faulander/remember/server/internal/database"
	"github.com/faulander/remember/server/internal/identity"
	"github.com/faulander/remember/server/internal/session"
	"github.com/google/uuid"
)

const maxJSONBodyBytes int64 = 16 << 10

// State controls whether the service may receive new work.
type State struct{ ready atomic.Bool }

func (s *State) MarkReady()    { s.ready.Store(true) }
func (s *State) MarkDraining() { s.ready.Store(false) }
func (s *State) Ready() bool   { return s.ready.Load() }

// SessionService is the only authority available to auth HTTP handlers. The
// raw database remains scoped to readiness checks.
type SessionService interface {
	Login(context.Context, string, string, string) (session.LoginResult, error)
	AuthenticateAccess(context.Context, string) (session.Principal, error)
	Refresh(context.Context, string) (session.Tokens, error)
	ListForUser(context.Context, string) ([]session.SessionInfo, error)
	RenameDevice(context.Context, string, uuid.UUID, string) error
	RevokeSession(context.Context, string, uuid.UUID) error
	RevokeDevice(context.Context, string, uuid.UUID) error
}

// Dependencies are explicit and injectable for bounded transport tests.
type Dependencies struct {
	Sessions SessionService
	Clock    Clock
}

// New returns the bounded public API.
func New(db *sql.DB, state *State, logger *slog.Logger, dependencies Dependencies) (http.Handler, error) {
	if db == nil || state == nil || dependencies.Sessions == nil {
		return nil, errors.New("http API dependency is nil")
	}
	if dependencies.Clock == nil {
		dependencies.Clock = wallClock{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	api := &handler{
		db: db, state: state, sessions: dependencies.Sessions,
		limits: newAbuseLimiters(dependencies.Clock), loginSlots: make(chan struct{}, maxConcurrentLogin),
	}
	return requestLog(logger, securityHeaders(api)), nil
}

type handler struct {
	db         *sql.DB
	state      *State
	sessions   SessionService
	limits     *abuseLimiters
	loginSlots chan struct{}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	switch {
	case r.URL.Path == "/healthz":
		h.probe(w, r, false)
	case r.URL.Path == "/readyz":
		h.probe(w, r, true)
	case r.URL.Path == "/v1/auth/login":
		h.login(w, r)
	case r.URL.Path == "/v1/auth/refresh":
		h.refresh(w, r)
	case r.URL.Path == "/v1/auth/logout":
		h.logout(w, r)
	case r.URL.Path == "/v1/sessions":
		h.listSessions(w, r)
	case onePathSegment(r.URL.Path, "/v1/devices/"):
		h.device(w, r, strings.TrimPrefix(r.URL.Path, "/v1/devices/"))
	case onePathSegment(r.URL.Path, "/v1/sessions/"):
		h.session(w, r, strings.TrimPrefix(r.URL.Path, "/v1/sessions/"))
	default:
		writeAPIError(w, r, http.StatusNotFound, "not_found")
	}
}

func (h *handler) probe(w http.ResponseWriter, r *http.Request, readiness bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, "GET, HEAD")
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

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	var request struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
	}
	if err := decodeStrictJSON(w, r, &request); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	select {
	case h.loginSlots <- struct{}{}:
		defer func() { <-h.loginSlots }()
	default:
		writeRateLimited(w, r, time.Second)
		return
	}
	if allowed, retry := h.limits.allowLogin(request.Email); !allowed {
		writeRateLimited(w, r, retry)
		return
	}
	result, err := h.sessions.Login(r.Context(), request.Email, request.Password, request.DeviceName)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidCredentials), errors.Is(err, session.ErrUnauthenticated):
			writeAPIError(w, r, http.StatusUnauthorized, "invalid_credentials")
		case errors.Is(err, session.ErrInvalidInput):
			writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		default:
			writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		}
		return
	}
	writeJSON(w, r, http.StatusOK, loginResponse(result))
}

func (h *handler) refresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	var request struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeStrictJSON(w, r, &request); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if allowed, retry := h.limits.allowRefresh(request.RefreshToken); !allowed {
		writeRateLimited(w, r, retry)
		return
	}
	tokens, err := h.sessions.Refresh(r.Context(), request.RefreshToken)
	if err != nil {
		if errors.Is(err, session.ErrUnauthenticated) {
			writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
		} else {
			writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		}
		return
	}
	writeJSON(w, r, http.StatusOK, tokenResponse(tokens))
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	access, principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if err := requireEmptyBody(w, r); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.sessions.RevokeSession(r.Context(), access, principal.SessionID); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "revoked"})
}

func (h *handler) listSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	access, _, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if err := requireEmptyBody(w, r); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	items, err := h.sessions.ListForUser(r.Context(), access)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	response := make([]sessionInfoResponse, 0, len(items))
	for _, item := range items {
		response = append(response, mapSessionInfo(item))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"sessions": response})
}

func (h *handler) device(w http.ResponseWriter, r *http.Request, rawID string) {
	if r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		methodNotAllowed(w, r, "PATCH, DELETE")
		return
	}
	access, _, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	id, err := parseUUIDv7(rawID)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.Method == http.MethodPatch {
		var request struct {
			DisplayName string `json:"display_name"`
		}
		if err := decodeStrictJSON(w, r, &request); err != nil {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		err = h.sessions.RenameDevice(r.Context(), access, id, request.DisplayName)
	} else {
		if err := requireEmptyBody(w, r); err != nil {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		err = h.sessions.RevokeDevice(r.Context(), access, id)
	}
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) session(w http.ResponseWriter, r *http.Request, rawID string) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, r, http.MethodDelete)
		return
	}
	access, _, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	id, err := parseUUIDv7(rawID)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := requireEmptyBody(w, r); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.sessions.RevokeSession(r.Context(), access, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) authenticate(w http.ResponseWriter, r *http.Request) (string, session.Principal, bool) {
	access, err := bearerToken(r)
	if err != nil {
		writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
		return "", session.Principal{}, false
	}
	principal, err := h.sessions.AuthenticateAccess(r.Context(), access)
	if err != nil {
		if errors.Is(err, session.ErrUnauthenticated) {
			writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
		} else {
			writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		}
		return "", session.Principal{}, false
	}
	return access, principal, true
}

func (h *handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, session.ErrUnauthenticated):
		writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
	case errors.Is(err, session.ErrInvalidInput):
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, session.ErrNotFound):
		writeAPIError(w, r, http.StatusNotFound, "not_found")
	default:
		writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
	}
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any) error {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return errors.New("invalid JSON media type")
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		return errors.New("invalid JSON media type")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func requireEmptyBody(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	content, err := io.ReadAll(r.Body)
	if err != nil || len(content) != 0 {
		return session.ErrInvalidInput
	}
	return nil
}

func bearerToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > 128 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", session.ErrUnauthenticated
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if len(token) != 43 || strings.ContainsAny(token, " \t\r\n,") {
		return "", session.ErrUnauthenticated
	}
	return token, nil
}

func onePathSegment(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	remainder := strings.TrimPrefix(path, prefix)
	return remainder != "" && !strings.Contains(remainder, "/")
}

func parseUUIDv7(raw string) (uuid.UUID, error) {
	if len(raw) != 36 || strings.Contains(raw, "/") {
		return uuid.Nil, session.ErrInvalidInput
	}
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return uuid.Nil, session.ErrInvalidInput
	}
	return id, nil
}

type tokenPairResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	AccessExpiresAt  string `json:"access_expires_at"`
	RefreshExpiresAt string `json:"refresh_expires_at"`
}

type loginResultResponse struct {
	Principal principalJSON     `json:"principal"`
	Tokens    tokenPairResponse `json:"tokens"`
}

type principalJSON struct {
	UserID    string `json:"user_id"`
	DeviceID  string `json:"device_id"`
	SessionID string `json:"session_id"`
}

type sessionInfoResponse struct {
	SessionID  string  `json:"session_id"`
	DeviceID   string  `json:"device_id"`
	DeviceName string  `json:"device_name"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  string  `json:"expires_at"`
	RevokedAt  *string `json:"revoked_at"`
	Current    bool    `json:"current"`
}

func loginResponse(result session.LoginResult) loginResultResponse {
	return loginResultResponse{
		Principal: principalJSON{
			UserID: result.Principal.UserID.String(), DeviceID: result.Principal.DeviceID.String(), SessionID: result.Principal.SessionID.String(),
		},
		Tokens: tokenResponse(result.Tokens),
	}
}

func tokenResponse(tokens session.Tokens) tokenPairResponse {
	return tokenPairResponse{
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		AccessExpiresAt:  tokens.AccessExpiresAt.UTC().Format(time.RFC3339Nano),
		RefreshExpiresAt: tokens.RefreshExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func mapSessionInfo(item session.SessionInfo) sessionInfoResponse {
	result := sessionInfoResponse{
		SessionID: item.SessionID.String(), DeviceID: item.DeviceID.String(), DeviceName: item.DeviceName,
		Status: item.Status, CreatedAt: item.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: item.ExpiresAt.UTC().Format(time.RFC3339Nano), Current: item.Current,
	}
	if item.RevokedAt != nil {
		value := item.RevokedAt.UTC().Format(time.RFC3339Nano)
		result.RevokedAt = &value
	}
	return result
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeAPIError(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
}

func writeRateLimited(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	seconds := int64((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited")
}

func writeAPIError(w http.ResponseWriter, r *http.Request, status int, code string) {
	writeJSON(w, r, status, map[string]string{"error": code})
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
	case "/healthz", "/readyz", "/v1/auth/login", "/v1/auth/refresh", "/v1/auth/logout", "/v1/sessions":
		return path
	default:
		switch {
		case onePathSegment(path, "/v1/devices/"):
			return "/v1/devices/{id}"
		case onePathSegment(path, "/v1/sessions/"):
			return "/v1/sessions/{id}"
		default:
			return "unknown"
		}
	}
}
