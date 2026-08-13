package remotehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

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
