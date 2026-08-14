package remotehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPublicRegistrationAndEmailVerification(t *testing.T) {
	t.Parallel()
	var registrations, verifications int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/register":
			registrations++
			var body map[string]string
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || body["email"] != "person@example.com" || body["password"] != "correct horse battery staple" {
				t.Errorf("registration request = %s auth=%q body=%#v", r.Method, r.Header.Get("Authorization"), body)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"verification_required"}`))
		case "/v1/auth/verify-email":
			verifications++
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["token"] != strings.Repeat("A", 43) {
				t.Errorf("verification request = %#v", body)
			}
			_, _ = w.Write([]byte(`{"status":"verified"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := Register(context.Background(), server.URL, nil, "person@example.com", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEmail(context.Background(), server.URL, nil, strings.Repeat("A", 43)); err != nil {
		t.Fatal(err)
	}
	if registrations != 1 || verifications != 1 {
		t.Fatalf("registrations=%d verifications=%d", registrations, verifications)
	}
}

func TestPublicIdentityRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, path, code string
		status           int
		retryable        bool
	}{
		{"registration disabled", "/v1/auth/register", "registration_unavailable", http.StatusServiceUnavailable, false},
		{"invalid verification", "/v1/auth/verify-email", "invalid_verification", http.StatusBadRequest, false},
		{"registration limited", "/v1/auth/register", "rate_limited", http.StatusTooManyRequests, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": test.code})
			}))
			defer server.Close()
			var err error
			if test.path == "/v1/auth/register" {
				err = Register(context.Background(), server.URL, nil, "person@example.com", "correct horse battery staple")
			} else {
				err = VerifyEmail(context.Background(), server.URL, nil, strings.Repeat("A", 43))
			}
			if test.retryable {
				if !errors.Is(err, ErrRetryable) {
					t.Fatalf("error = %v, want retryable", err)
				}
				return
			}
			var rejected *RejectedError
			if !errors.As(err, &rejected) || rejected.Status != test.status || rejected.Code != test.code {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestSessionLoginRefreshAndLogout(t *testing.T) {
	t.Parallel()
	user, device, sessionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/login":
			var body map[string]string
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || body["email"] != "person@example.com" || body["password"] != "secret" || body["device_name"] != "Mac" {
				t.Errorf("invalid login request: %#v", body)
			}
			writeSessionLoginResponse(t, w, user, device, sessionID, "access-1", "refresh-1", time.Now().Add(time.Second), time.Now().Add(time.Hour))
		case "/v1/auth/refresh":
			refreshes.Add(1)
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["refresh_token"] != "refresh-1" {
				t.Errorf("invalid refresh request: %#v", body)
			}
			writeSessionTokens(t, w, "access-2", "refresh-2", time.Now().Add(15*time.Minute), time.Now().Add(time.Hour))
		case "/v1/auth/logout":
			if r.Header.Get("Authorization") != "Bearer access-2" {
				t.Errorf("logout authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"status":"revoked"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	session, err := Login(context.Background(), server.URL, nil, "person@example.com", "secret", "Mac")
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Principal(); got != (Principal{UserID: user, DeviceID: device, SessionID: sessionID}) {
		t.Fatalf("principal = %#v", got)
	}
	token, err := session.AccessToken(context.Background())
	if err != nil || token != "access-2" || refreshes.Load() != 1 {
		t.Fatalf("AccessToken() = %q, %v; refreshes=%d", token, err, refreshes.Load())
	}
	if err := session.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.AccessToken(context.Background()); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("post-logout AccessToken() error = %v", err)
	}
}

func TestSessionListsRenamesAndRevokesSessions(t *testing.T) {
	t.Parallel()
	user, device, currentSession := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	otherDevice, otherSession := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	created := time.Now().Add(-time.Hour).UTC()
	expires := time.Now().Add(time.Hour).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/login":
			writeSessionLoginResponse(t, w, user, device, currentSession, "access", "refresh", time.Now().Add(time.Minute), expires)
		case "/v1/sessions":
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer access" {
				t.Errorf("list request = %s auth=%q", r.Method, r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{
				{"session_id": currentSession.String(), "device_id": device.String(), "device_name": "Dieser Mac", "status": "active", "created_at": created.Format(time.RFC3339Nano), "expires_at": expires.Format(time.RFC3339Nano), "revoked_at": nil, "current": true},
				{"session_id": otherSession.String(), "device_id": otherDevice.String(), "device_name": "MacBook", "status": "active", "created_at": created.Format(time.RFC3339Nano), "expires_at": expires.Format(time.RFC3339Nano), "revoked_at": nil, "current": false},
			}})
		case "/v1/devices/" + device.String():
			var body map[string]string
			if r.Method != http.MethodPatch || r.Header.Get("Authorization") != "Bearer access" || json.NewDecoder(r.Body).Decode(&body) != nil || body["display_name"] != "Arbeitsplatz" {
				t.Errorf("rename request = %s %#v", r.Method, body)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/devices/" + otherDevice.String():
			if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer access" {
				t.Errorf("device revoke request = %s auth=%q", r.Method, r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/sessions/" + otherSession.String():
			if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer access" {
				t.Errorf("revoke request = %s auth=%q", r.Method, r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	session, err := Login(context.Background(), server.URL, nil, "person@example.com", "secret", "Dieser Mac")
	if err != nil {
		t.Fatal(err)
	}
	items, err := session.ListSessions(context.Background())
	if err != nil || len(items) != 2 || !items[0].Current || items[1].SessionID != otherSession {
		t.Fatalf("ListSessions() = %#v, %v", items, err)
	}
	if err := session.RenameDevice(context.Background(), device, "Arbeitsplatz"); err != nil {
		t.Fatal(err)
	}
	if err := session.RevokeDevice(context.Background(), otherDevice); err != nil {
		t.Fatal(err)
	}
	if err := session.RevokeSession(context.Background(), otherSession); err != nil {
		t.Fatal(err)
	}
}

func TestSessionResumeRotatesAndPersistsRefreshCredential(t *testing.T) {
	t.Parallel()
	principal := Principal{UserID: uuid.Must(uuid.NewV7()), DeviceID: uuid.Must(uuid.NewV7()), SessionID: uuid.Must(uuid.NewV7())}
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]string
		if r.URL.Path != "/v1/auth/refresh" || json.NewDecoder(r.Body).Decode(&body) != nil || body["refresh_token"] != "persisted-refresh" {
			t.Errorf("resume request = %s %#v", r.URL.Path, body)
		}
		refreshes.Add(1)
		writeSessionTokens(t, w, "access-new", "refresh-new", time.Now().Add(15*time.Minute), time.Now().Add(time.Hour))
	}))
	defer server.Close()

	session, err := Resume(context.Background(), server.URL, nil, principal, "persisted-refresh")
	if err != nil {
		t.Fatal(err)
	}
	var savedToken string
	if err := session.BindRefreshTokenSink(context.Background(), RefreshTokenSinkFunc(func(_ context.Context, token string, _ time.Time) error {
		savedToken = token
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	token, err := session.AccessToken(context.Background())
	if err != nil || token != "access-new" || savedToken != "refresh-new" || refreshes.Load() != 1 {
		t.Fatalf("restored token=%q saved=%q refreshes=%d err=%v", token, savedToken, refreshes.Load(), err)
	}
}

func TestSessionDoesNotExposeRefreshPersistenceErrors(t *testing.T) {
	t.Parallel()
	var logouts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/auth/logout" {
			logouts.Add(1)
			_, _ = w.Write([]byte(`{"status":"revoked"}`))
			return
		}
		writeSessionLoginResponse(t, w, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), "access", "refresh", time.Now().Add(time.Minute), time.Now().Add(time.Hour))
	}))
	defer server.Close()
	session, err := Login(context.Background(), server.URL, nil, "person@example.com", "secret", "Mac")
	if err != nil {
		t.Fatal(err)
	}
	err = session.BindRefreshTokenSink(context.Background(), RefreshTokenSinkFunc(func(context.Context, string, time.Time) error {
		return errors.New("keychain backend detail")
	}))
	if err == nil || strings.Contains(err.Error(), "backend detail") {
		t.Fatalf("persistence error = %v", err)
	}
	if err := session.Logout(context.Background()); err != nil || logouts.Load() != 1 {
		t.Fatalf("Logout() after persistence failure = %v; calls=%d", err, logouts.Load())
	}
}

func TestSessionRejectsAuthenticationErrorsAndMalformedTokens(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		response func(http.ResponseWriter)
		want     error
	}{
		{name: "credentials", response: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_credentials"}`))
		}, want: ErrReauthRequired},
		{name: "expired refresh", response: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			writeSessionLoginResponse(t, w, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), "access", "refresh", time.Now().Add(time.Minute), time.Now().Add(-time.Minute))
		}, want: ErrInvalidResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { test.response(w) }))
			defer server.Close()
			_, err := Login(context.Background(), server.URL, nil, "person@example.com", "secret", "Mac")
			if !errors.Is(err, test.want) {
				t.Fatalf("Login() error = %v, want %v", err, test.want)
			}
		})
	}
}

func writeSessionLoginResponse(t *testing.T, w http.ResponseWriter, user, device, session uuid.UUID, access, refresh string, accessExpiry, refreshExpiry time.Time) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{
		"principal": map[string]string{"user_id": user.String(), "device_id": device.String(), "session_id": session.String()},
		"tokens":    sessionTokenResponse(access, refresh, accessExpiry, refreshExpiry),
	}); err != nil {
		t.Error(err)
	}
}

func writeSessionTokens(t *testing.T, w http.ResponseWriter, access, refresh string, accessExpiry, refreshExpiry time.Time) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(sessionTokenResponse(access, refresh, accessExpiry, refreshExpiry)); err != nil {
		t.Error(err)
	}
}

func sessionTokenResponse(access, refresh string, accessExpiry, refreshExpiry time.Time) map[string]string {
	return map[string]string{
		"access_token": access, "refresh_token": refresh,
		"access_expires_at":  accessExpiry.UTC().Format(time.RFC3339Nano),
		"refresh_expires_at": refreshExpiry.UTC().Format(time.RFC3339Nano),
	}
}
