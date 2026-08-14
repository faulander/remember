package remotehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const accessRefreshMargin = 30 * time.Second

type Principal struct {
	UserID, DeviceID, SessionID uuid.UUID
}

type RefreshTokenSink interface {
	SaveRefreshToken(context.Context, string, time.Time) error
}

type RefreshTokenSinkFunc func(context.Context, string, time.Time) error

func (f RefreshTokenSinkFunc) SaveRefreshToken(ctx context.Context, token string, expiresAt time.Time) error {
	return f(ctx, token, expiresAt)
}

// Session owns one login's credentials in memory. Persistence belongs to a
// platform keychain adapter; credentials are never written to the local index.
type Session struct {
	mu sync.Mutex

	base *url.URL
	http *http.Client

	principal        Principal
	accessToken      string
	refreshToken     string
	accessExpiresAt  time.Time
	refreshExpiresAt time.Time
	refreshSink      RefreshTokenSink
	refreshDirty     bool
}

// Login authenticates one device and returns an in-memory session suitable as
// a Client AccessTokenSource.
func Login(ctx context.Context, rawBase string, transport *http.Client, email, password, deviceName string) (*Session, error) {
	base, client, err := remoteEndpoint(rawBase, transport)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"email": email, "password": password, "device_name": deviceName})
	if err != nil {
		return nil, errors.New("encode login request")
	}
	resp, err := sessionRequest(ctx, client, base, http.MethodPost, "/v1/auth/login", body, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifySessionError(resp, "invalid_credentials")
	}
	var out struct {
		Principal struct {
			UserID    string `json:"user_id"`
			DeviceID  string `json:"device_id"`
			SessionID string `json:"session_id"`
		} `json:"principal"`
		Tokens sessionTokens `json:"tokens"`
	}
	if err := decodeJSON(resp, &out, "principal", "tokens"); err != nil {
		return nil, err
	}
	principal, err := parsePrincipal(out.Principal.UserID, out.Principal.DeviceID, out.Principal.SessionID)
	if err != nil {
		return nil, err
	}
	access, refresh, accessExpiry, refreshExpiry, err := validateSessionTokens(out.Tokens, time.Now())
	if err != nil {
		return nil, err
	}
	return &Session{
		base: base, http: client, principal: principal,
		accessToken: access, refreshToken: refresh,
		accessExpiresAt: accessExpiry, refreshExpiresAt: refreshExpiry,
	}, nil
}

// Resume rotates one persisted refresh token before making the restored
// session available.
func Resume(ctx context.Context, rawBase string, transport *http.Client, principal Principal, refreshToken string) (*Session, error) {
	if !validUUIDv7(principal.UserID) || !validUUIDv7(principal.DeviceID) || !validUUIDv7(principal.SessionID) || !validSessionToken(refreshToken) {
		return nil, ErrInvalidResponse
	}
	base, client, err := remoteEndpoint(rawBase, transport)
	if err != nil {
		return nil, err
	}
	session := &Session{base: base, http: client, principal: principal, refreshToken: refreshToken}
	session.mu.Lock()
	err = session.refreshLocked(ctx, true)
	session.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return session, nil
}

// BindRefreshTokenSink persists the current rotating refresh credential and
// makes every later rotation durable before an access token is returned.
func (s *Session) BindRefreshTokenSink(ctx context.Context, sink RefreshTokenSink) error {
	if s == nil || sink == nil {
		return errors.New("nil refresh token sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshToken == "" {
		return ErrReauthRequired
	}
	previousSink, previousDirty := s.refreshSink, s.refreshDirty
	s.refreshSink, s.refreshDirty = sink, true
	if err := s.persistRefreshLocked(ctx); err != nil {
		s.refreshSink, s.refreshDirty = previousSink, previousDirty
		return err
	}
	return nil
}

func (s *Session) Principal() Principal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.principal
}

func (s *Session) AccessToken(ctx context.Context) (string, error) {
	if s == nil {
		return "", ErrReauthRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshToken == "" || !time.Now().Before(s.refreshExpiresAt) {
		return "", ErrReauthRequired
	}
	if err := s.persistRefreshLocked(ctx); err != nil {
		return "", err
	}
	if s.accessToken != "" && time.Until(s.accessExpiresAt) > accessRefreshMargin {
		return s.accessToken, nil
	}
	if err := s.refreshLocked(ctx, false); err != nil {
		return "", err
	}
	return s.accessToken, nil
}

func (s *Session) refreshLocked(ctx context.Context, allowUnknownExpiry bool) error {
	if s.refreshToken == "" || (!allowUnknownExpiry && !time.Now().Before(s.refreshExpiresAt)) {
		return ErrReauthRequired
	}
	body, err := json.Marshal(map[string]string{"refresh_token": s.refreshToken})
	if err != nil {
		return errors.New("encode refresh request")
	}
	resp, err := sessionRequest(ctx, s.http, s.base, http.MethodPost, "/v1/auth/refresh", body, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := classifySessionError(resp, "invalid_session")
		if errors.Is(err, ErrReauthRequired) {
			s.clearLocked()
		}
		return err
	}
	var out sessionTokens
	if err := decodeJSON(resp, &out, "access_token", "refresh_token", "access_expires_at", "refresh_expires_at"); err != nil {
		return err
	}
	access, refresh, accessExpiry, refreshExpiry, err := validateSessionTokens(out, time.Now())
	if err != nil {
		return err
	}
	s.accessToken, s.refreshToken = access, refresh
	s.accessExpiresAt, s.refreshExpiresAt = accessExpiry, refreshExpiry
	s.refreshDirty = s.refreshSink != nil
	return s.persistRefreshLocked(ctx)
}

func (s *Session) persistRefreshLocked(ctx context.Context) error {
	if !s.refreshDirty {
		return nil
	}
	if s.refreshSink == nil || s.refreshSink.SaveRefreshToken(ctx, s.refreshToken, s.refreshExpiresAt) != nil {
		return errors.New("secure session persistence unavailable")
	}
	s.refreshDirty = false
	return nil
}

// Logout revokes the server session. Retryable failures leave the credentials
// available so the caller can retry rather than silently abandoning a live session.
func (s *Session) Logout(ctx context.Context) error {
	if s == nil {
		return nil
	}
	token, err := s.AccessToken(ctx)
	if errors.Is(err, ErrReauthRequired) {
		return nil
	}
	if err != nil {
		return err
	}
	resp, err := sessionRequest(ctx, s.http, s.base, http.MethodPost, "/v1/auth/logout", nil, token)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := classifySessionError(resp, "invalid_session")
		if errors.Is(err, ErrReauthRequired) {
			s.mu.Lock()
			s.clearLocked()
			s.mu.Unlock()
			return nil
		}
		return err
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(resp, &out, "status"); err != nil || out.Status != "revoked" {
		return ErrInvalidResponse
	}
	s.mu.Lock()
	s.clearLocked()
	s.mu.Unlock()
	return nil
}

type SessionInfo struct {
	SessionID  uuid.UUID
	DeviceID   uuid.UUID
	DeviceName string
	Status     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	Current    bool
}

func (s *Session) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	resp, err := s.authorizedRequest(ctx, http.MethodGet, "/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifySessionError(resp, "invalid_session")
	}
	var out struct {
		Sessions []struct {
			SessionID  string  `json:"session_id"`
			DeviceID   string  `json:"device_id"`
			DeviceName string  `json:"device_name"`
			Status     string  `json:"status"`
			CreatedAt  string  `json:"created_at"`
			ExpiresAt  string  `json:"expires_at"`
			RevokedAt  *string `json:"revoked_at"`
			Current    *bool   `json:"current"`
		} `json:"sessions"`
	}
	if err := decodeJSON(resp, &out, "sessions"); err != nil || out.Sessions == nil {
		return nil, ErrInvalidResponse
	}
	result := make([]SessionInfo, 0, len(out.Sessions))
	seen, current := make(map[uuid.UUID]bool, len(out.Sessions)), 0
	principal := s.Principal()
	for _, raw := range out.Sessions {
		sessionID, sessionErr := uuid.Parse(raw.SessionID)
		deviceID, deviceErr := uuid.Parse(raw.DeviceID)
		createdAt, createdErr := time.Parse(time.RFC3339Nano, raw.CreatedAt)
		expiresAt, expiresErr := time.Parse(time.RFC3339Nano, raw.ExpiresAt)
		if sessionErr != nil || deviceErr != nil || createdErr != nil || expiresErr != nil || !validUUIDv7(sessionID) || !validUUIDv7(deviceID) || seen[sessionID] || strings.TrimSpace(raw.DeviceName) == "" || len(raw.DeviceName) > 400 || raw.Current == nil || !createdAt.Before(expiresAt) || (raw.Status != "active" && raw.Status != "revoked") {
			return nil, ErrInvalidResponse
		}
		item := SessionInfo{SessionID: sessionID, DeviceID: deviceID, DeviceName: raw.DeviceName, Status: raw.Status, CreatedAt: createdAt, ExpiresAt: expiresAt, Current: *raw.Current}
		if raw.RevokedAt != nil {
			value, err := time.Parse(time.RFC3339Nano, *raw.RevokedAt)
			if err != nil {
				return nil, ErrInvalidResponse
			}
			item.RevokedAt = &value
		}
		if (item.Status == "revoked") != (item.RevokedAt != nil) {
			return nil, ErrInvalidResponse
		}
		if item.Current {
			if item.SessionID != principal.SessionID || item.DeviceID != principal.DeviceID || item.Status != "active" {
				return nil, ErrInvalidResponse
			}
			current++
		}
		seen[item.SessionID] = true
		result = append(result, item)
	}
	if current != 1 {
		return nil, ErrInvalidResponse
	}
	return result, nil
}

func (s *Session) RenameDevice(ctx context.Context, deviceID uuid.UUID, name string) error {
	if !validUUIDv7(deviceID) {
		return errors.New("invalid device id")
	}
	body, err := json.Marshal(map[string]string{"display_name": name})
	if err != nil {
		return errors.New("encode device request")
	}
	return s.sessionMutation(ctx, http.MethodPatch, "/v1/devices/"+deviceID.String(), body)
}

func (s *Session) RevokeSession(ctx context.Context, sessionID uuid.UUID) error {
	if !validUUIDv7(sessionID) {
		return errors.New("invalid session id")
	}
	return s.sessionMutation(ctx, http.MethodDelete, "/v1/sessions/"+sessionID.String(), nil)
}

func (s *Session) sessionMutation(ctx context.Context, method, path string, body []byte) error {
	resp, err := s.authorizedRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifySessionError(resp, "invalid_session")
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(resp, &out, "status"); err != nil || out.Status != "ok" {
		return ErrInvalidResponse
	}
	return nil
}

func (s *Session) authorizedRequest(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	token, err := s.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	return sessionRequest(ctx, s.http, s.base, method, path, body, token)
}

type sessionTokens struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	AccessExpiresAt  string `json:"access_expires_at"`
	RefreshExpiresAt string `json:"refresh_expires_at"`
}

func validateSessionTokens(tokens sessionTokens, now time.Time) (string, string, time.Time, time.Time, error) {
	if !validSessionToken(tokens.AccessToken) || !validSessionToken(tokens.RefreshToken) {
		return "", "", time.Time{}, time.Time{}, ErrInvalidResponse
	}
	accessExpiry, accessErr := time.Parse(time.RFC3339Nano, tokens.AccessExpiresAt)
	refreshExpiry, refreshErr := time.Parse(time.RFC3339Nano, tokens.RefreshExpiresAt)
	if accessErr != nil || refreshErr != nil || accessExpiry.After(refreshExpiry) || !now.Before(refreshExpiry) {
		return "", "", time.Time{}, time.Time{}, ErrInvalidResponse
	}
	return tokens.AccessToken, tokens.RefreshToken, accessExpiry, refreshExpiry, nil
}

func validSessionToken(token string) bool {
	return token != "" && len(token) <= 4096 && !strings.ContainsAny(token, "\r\n\x00")
}

func parsePrincipal(userRaw, deviceRaw, sessionRaw string) (Principal, error) {
	user, userErr := uuid.Parse(userRaw)
	device, deviceErr := uuid.Parse(deviceRaw)
	session, sessionErr := uuid.Parse(sessionRaw)
	if userErr != nil || deviceErr != nil || sessionErr != nil || !validUUIDv7(user) || !validUUIDv7(device) || !validUUIDv7(session) {
		return Principal{}, ErrInvalidResponse
	}
	return Principal{UserID: user, DeviceID: device, SessionID: session}, nil
}

func sessionRequest(ctx context.Context, client *http.Client, base *url.URL, method, path string, body []byte, bearer string) (*http.Response, error) {
	u := *base
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("build authentication request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrRetryable
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, ErrInvalidResponse
	}
	return resp, nil
}

func classifySessionError(resp *http.Response, unauthorizedCode string) error {
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil || len(content) > maxJSONBytes || rejectDuplicates(content) != nil || !validJSONContentType(resp.Header.Values("Content-Type")) {
		return ErrInvalidResponse
	}
	var out struct {
		Error string `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil {
		return ErrInvalidResponse
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized && out.Error == unauthorizedCode:
		return ErrReauthRequired
	case resp.StatusCode == http.StatusBadRequest && out.Error == "invalid_request":
		return &RejectedError{Status: resp.StatusCode, Code: out.Error}
	case resp.StatusCode == http.StatusNotFound && out.Error == "not_found":
		return &RejectedError{Status: resp.StatusCode, Code: out.Error}
	case resp.StatusCode == http.StatusTooManyRequests && out.Error == "rate_limited":
		return ErrRetryable
	case resp.StatusCode == http.StatusInternalServerError && out.Error == "internal_error":
		return ErrRetryable
	default:
		return ErrInvalidResponse
	}
}

func (s *Session) clearLocked() {
	s.accessToken = ""
	s.refreshToken = ""
	s.accessExpiresAt = time.Time{}
	s.refreshExpiresAt = time.Time{}
	s.refreshDirty = false
}
