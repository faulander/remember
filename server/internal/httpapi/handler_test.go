package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faulander/remember/server/internal/database"
)

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state := &State{}
	handler := New(db, state, nil)

	assertProbe(t, handler, http.MethodGet, "/healthz", http.StatusOK, true)
	assertProbe(t, handler, http.MethodGet, "/readyz", http.StatusServiceUnavailable, true)
	state.MarkReady()
	assertProbe(t, handler, http.MethodGet, "/readyz", http.StatusOK, true)
	assertProbe(t, handler, http.MethodHead, "/healthz", http.StatusOK, false)
	assertProbe(t, handler, http.MethodHead, "/readyz", http.StatusOK, false)
	state.MarkDraining()
	assertProbe(t, handler, http.MethodGet, "/healthz", http.StatusOK, true)
	assertProbe(t, handler, http.MethodGet, "/readyz", http.StatusServiceUnavailable, true)
}

func TestReadinessHidesDatabaseFailure(t *testing.T) {
	t.Parallel()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "sensitive-name.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state := &State{}
	state.MarkReady()
	handler := New(db, state, nil)
	db.Close()

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if strings.Contains(response.Body.String(), "sensitive-name") || strings.Contains(response.Body.String(), "closed") {
		t.Errorf("response leaked database details: %s", response.Body.String())
	}
}

func TestProbeMethodAndHeaders(t *testing.T) {
	t.Parallel()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := New(db, &State{}, nil)

	request := httptest.NewRequest(http.MethodPost, "/healthz", strings.NewReader("ignored"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("method response = %d, Allow=%q", response.Code, response.Header().Get("Allow"))
	}
	for name, want := range map[string]string{
		"Cache-Control":          "no-store",
		"Content-Type":           "application/json; charset=utf-8",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Error("missing request ID")
	}
}

func TestRequestLogDoesNotContainUnknownPathOrQuery(t *testing.T) {
	t.Parallel()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := New(db, &State{}, logger)

	request := httptest.NewRequest(http.MethodGet, "/private-note-name?token=secret-value", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	for _, secret := range []string{"private-note-name", "secret-value", "token"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("log leaked %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `"route":"unknown"`) {
		t.Errorf("log missing bounded route: %s", logs.String())
	}
}

func assertProbe(t *testing.T, handler http.Handler, method, path string, status int, wantBody bool) {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status {
		t.Errorf("%s %s status = %d, want %d", method, path, response.Code, status)
	}
	if wantBody && response.Body.Len() == 0 {
		t.Errorf("%s %s has empty body", method, path)
	}
	if !wantBody && response.Body.Len() != 0 {
		t.Errorf("%s %s body = %q, want empty", method, path, response.Body.String())
	}
}
