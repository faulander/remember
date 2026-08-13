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
	resp, err := sessionRequest(ctx, client, base, "/v1/auth/login", body, "")
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
	if s.accessToken != "" && time.Until(s.accessExpiresAt) > accessRefreshMargin {
		return s.accessToken, nil
	}
	body, err := json.Marshal(map[string]string{"refresh_token": s.refreshToken})
	if err != nil {
		return "", errors.New("encode refresh request")
	}
	resp, err := sessionRequest(ctx, s.http, s.base, "/v1/auth/refresh", body, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := classifySessionError(resp, "invalid_session")
		if errors.Is(err, ErrReauthRequired) {
			s.clearLocked()
		}
		return "", err
	}
	var out sessionTokens
	if err := decodeJSON(resp, &out, "access_token", "refresh_token", "access_expires_at", "refresh_expires_at"); err != nil {
		return "", err
	}
	access, refresh, accessExpiry, refreshExpiry, err := validateSessionTokens(out, time.Now())
	if err != nil {
		return "", err
	}
	s.accessToken, s.refreshToken = access, refresh
	s.accessExpiresAt, s.refreshExpiresAt = accessExpiry, refreshExpiry
	return s.accessToken, nil
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
	resp, err := sessionRequest(ctx, s.http, s.base, "/v1/auth/logout", nil, token)
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

func sessionRequest(ctx context.Context, client *http.Client, base *url.URL, path string, body []byte, bearer string) (*http.Response, error) {
	u := *base
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
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
}
