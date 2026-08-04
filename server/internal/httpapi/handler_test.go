package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/faulander/remember/server/internal/blob"
	"github.com/faulander/remember/server/internal/database"
	"github.com/faulander/remember/server/internal/identity"
	"github.com/faulander/remember/server/internal/session"
	"github.com/google/uuid"
)

const (
	testAccess  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testRefresh = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	testAccessB = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
)

var (
	testUserID    = uuid.MustParse("018f0000-0000-7000-8000-000000000001")
	testDeviceID  = uuid.MustParse("018f0000-0000-7000-8000-000000000002")
	testSessionID = uuid.MustParse("018f0000-0000-7000-8000-000000000003")
	targetID      = uuid.MustParse("018f0000-0000-7000-8000-000000000004")
	testUserBID   = uuid.MustParse("018f0000-0000-7000-8000-000000000005")
)

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()
	handler, state, _, cleanup := newHandlerTest(t, nil)
	defer cleanup()

	assertRequest(t, handler, http.MethodGet, "/healthz", "", "", http.StatusOK)
	assertRequest(t, handler, http.MethodGet, "/readyz", "", "", http.StatusServiceUnavailable)
	state.MarkReady()
	assertRequest(t, handler, http.MethodGet, "/readyz", "", "", http.StatusOK)
	request := httptest.NewRequest(http.MethodHead, "/healthz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("HEAD probe status/body = %d/%q", response.Code, response.Body.String())
	}
}

func TestAuthAndManagementSuccessContracts(t *testing.T) {
	t.Parallel()
	service := newFakeSessions()
	handler, _, _, cleanup := newHandlerTest(t, service)
	defer cleanup()

	login := jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": "user@example.com", "password": "secret password value", "device_name": "My Mac",
	}, "", http.StatusOK)
	assertJSONPath(t, login, "principal", "user_id", testUserID.String())
	assertJSONPath(t, login, "tokens", "access_token", testAccess)
	if strings.Contains(login.Body.String(), "secret password value") {
		t.Fatal("login response echoed password")
	}

	refresh := jsonRequest(t, handler, http.MethodPost, "/v1/auth/refresh", map[string]string{"refresh_token": testRefresh}, "", http.StatusOK)
	assertJSONPath(t, refresh, "", "access_token", testAccess)

	list := assertRequest(t, handler, http.MethodGet, "/v1/sessions", "", testAccess, http.StatusOK)
	if !strings.Contains(list.Body.String(), `"device_name":"My Mac"`) {
		t.Fatalf("session list = %s", list.Body.String())
	}
	jsonRequest(t, handler, http.MethodPatch, "/v1/devices/"+targetID.String(), map[string]string{"display_name": "Renamed"}, testAccess, http.StatusOK)
	assertRequest(t, handler, http.MethodDelete, "/v1/sessions/"+targetID.String(), "", testAccess, http.StatusOK)
	assertRequest(t, handler, http.MethodDelete, "/v1/devices/"+targetID.String(), "", testAccess, http.StatusOK)
	assertRequest(t, handler, http.MethodPost, "/v1/auth/logout", "", testAccess, http.StatusOK)

	service.mu.Lock()
	defer service.mu.Unlock()
	if service.renamedID != targetID || service.renamedName != "Renamed" || service.revokedDevice != targetID {
		t.Fatalf("device calls = rename %s/%q revoke %s", service.renamedID, service.renamedName, service.revokedDevice)
	}
	if len(service.revokedSessions) != 2 || service.revokedSessions[0] != targetID || service.revokedSessions[1] != testSessionID {
		t.Fatalf("session revocations = %v", service.revokedSessions)
	}
	for _, access := range service.receivedAccess {
		if access != testAccess {
			t.Fatalf("service received unexpected access credential %q", access)
		}
	}
}

func TestStrictJSONMediaBodyAndMethods(t *testing.T) {
	t.Parallel()
	handler, _, _, cleanup := newHandlerTest(t, nil)
	defer cleanup()

	for _, test := range []struct {
		name, contentType, body string
	}{
		{"missing media", "", `{}`},
		{"wrong media", "text/plain", `{}`},
		{"media parameters", "application/json; charset=utf-8", `{}`},
		{"unknown field", "application/json", `{"email":"a","password":"b","device_name":"c","user_id":"x"}`},
		{"trailing value", "application/json", `{"email":"a","password":"b","device_name":"c"} {}`},
		{"malformed", "application/json", `{`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertError(t, response, http.StatusBadRequest, "invalid_request")
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"email":"a","password":"b","device_name":"c"}`))
	request.Header.Add("Content-Type", "application/json")
	request.Header.Add("Content-Type", "text/plain")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")

	large := `{"email":"a","password":"` + strings.Repeat("x", int(maxJSONBodyBytes)) + `","device_name":"c"}`
	request = httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(large))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")

	response = assertRequest(t, handler, http.MethodDelete, "/v1/devices/"+targetID.String(), "unexpected", testAccess, http.StatusBadRequest)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	response = assertRequest(t, handler, http.MethodGet, "/v1/sessions", strings.Repeat("x", int(maxJSONBodyBytes)+1), testAccess, http.StatusBadRequest)
	assertError(t, response, http.StatusBadRequest, "invalid_request")

	for _, test := range []struct{ method, path, allow string }{
		{http.MethodGet, "/v1/auth/login", "POST"},
		{http.MethodPost, "/v1/sessions", "GET"},
		{http.MethodPost, "/v1/devices/" + targetID.String(), "PATCH, DELETE"},
		{http.MethodPatch, "/v1/sessions/" + targetID.String(), "DELETE"},
	} {
		response := assertRequest(t, handler, test.method, test.path, "", "", http.StatusMethodNotAllowed)
		if response.Header().Get("Allow") != test.allow {
			t.Errorf("%s %s Allow=%q", test.method, test.path, response.Header().Get("Allow"))
		}
	}
}

func TestBearerUUIDAndTenantOverrideAreRejected(t *testing.T) {
	t.Parallel()
	service := newFakeSessions()
	handler, _, _, cleanup := newHandlerTest(t, service)
	defer cleanup()

	for _, authorization := range []string{"", "bearer " + testAccess, "Bearer", "Bearer short", "Bearer " + testAccess + " extra"} {
		response := assertRequest(t, handler, http.MethodGet, "/v1/sessions", "", authorization, http.StatusUnauthorized)
		assertError(t, response, http.StatusUnauthorized, "invalid_session")
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	request.Header.Add("Authorization", "Bearer "+testAccess)
	request.Header.Add("Authorization", "Bearer "+testAccess)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusUnauthorized, "invalid_session")

	for _, raw := range []string{uuid.New().String(), strings.ToUpper(targetID.String())} {
		response := assertRequest(t, handler, http.MethodDelete, "/v1/devices/"+raw, "", testAccess, http.StatusBadRequest)
		assertError(t, response, http.StatusBadRequest, "invalid_request")
	}
	response = assertRequest(t, handler, http.MethodDelete, "/v1/devices/"+targetID.String()+"/extra", "", testAccess, http.StatusNotFound)
	assertError(t, response, http.StatusNotFound, "not_found")
	response = assertRequest(t, handler, http.MethodDelete, "/v1/devices/"+targetID.String()+"?user_id="+testUserID.String(), "", testAccess, http.StatusBadRequest)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.revokedDevice != uuid.Nil {
		t.Fatal("tenant override request reached service")
	}
}

func TestGenericErrorMappingNeverLeaksInternalErrors(t *testing.T) {
	t.Parallel()
	service := newFakeSessions()
	handler, _, _, cleanup := newHandlerTest(t, service)
	defer cleanup()

	service.loginErr = identity.ErrInvalidCredentials
	response := jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{"email": "missing@example.com", "password": "x", "device_name": "Mac"}, "", http.StatusUnauthorized)
	assertError(t, response, http.StatusUnauthorized, "invalid_credentials")
	service.loginErr = errors.New("database sensitive detail")
	response = jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{"email": "user2@example.com", "password": "x", "device_name": "Mac"}, "", http.StatusInternalServerError)
	assertError(t, response, http.StatusInternalServerError, "internal_error")
	if strings.Contains(response.Body.String(), "database") {
		t.Fatal("internal login error leaked")
	}

	service.loginErr = nil
	service.refreshErr = session.ErrUnauthenticated
	response = jsonRequest(t, handler, http.MethodPost, "/v1/auth/refresh", map[string]string{"refresh_token": testRefresh}, "", http.StatusUnauthorized)
	assertError(t, response, http.StatusUnauthorized, "invalid_session")
	service.authenticateErr = errors.New("secret storage failure")
	response = assertRequest(t, handler, http.MethodGet, "/v1/sessions", "", testAccess, http.StatusInternalServerError)
	assertError(t, response, http.StatusInternalServerError, "internal_error")
}

func TestLoginAndRefreshRateLimitsAreGeneric(t *testing.T) {
	t.Parallel()
	clock := &fakeHTTPClock{now: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
	service := newFakeSessions()
	handler, _, api, cleanup := newHandlerTestWithClock(t, service, clock)
	defer cleanup()

	for index := 0; index < loginKeyLimit; index++ {
		email := " User@Example.com "
		if index%2 == 1 {
			email = "user@example.COM"
		}
		jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{"email": email, "password": "x", "device_name": "Mac"}, "", http.StatusOK)
	}
	limited := jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{"email": "user@example.com", "password": "x", "device_name": "Mac"}, "", http.StatusTooManyRequests)
	assertError(t, limited, http.StatusTooManyRequests, "rate_limited")
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("login rate limit omitted Retry-After")
	}
	if len(api.limits.loginKey.entries) != 1 {
		t.Fatalf("normalized login keys=%d", len(api.limits.loginKey.entries))
	}
	for key := range api.limits.loginKey.entries {
		if key != limitKey(loginKeyDomain, "user@example.com") {
			t.Fatal("login limiter retained a non-hashed or unexpected key")
		}
	}

	for index := 0; index < loginKeyLimit; index++ {
		email := "person@bücher.example"
		if index%2 == 1 {
			email = "person@xn--bcher-kva.example"
		}
		jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{"email": email, "password": "x", "device_name": "Mac"}, "", http.StatusOK)
	}
	limited = jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": "person@xn--bcher-kva.example", "password": "x", "device_name": "Mac",
	}, "", http.StatusTooManyRequests)
	assertError(t, limited, http.StatusTooManyRequests, "rate_limited")

	for index := 0; index < refreshKeyLimit; index++ {
		jsonRequest(t, handler, http.MethodPost, "/v1/auth/refresh", map[string]string{"refresh_token": "unknown-secret-token"}, "", http.StatusOK)
	}
	limited = jsonRequest(t, handler, http.MethodPost, "/v1/auth/refresh", map[string]string{"refresh_token": "unknown-secret-token"}, "", http.StatusTooManyRequests)
	assertError(t, limited, http.StatusTooManyRequests, "rate_limited")
	if len(api.limits.refreshKey.entries) != 1 {
		t.Fatalf("refresh keys=%d", len(api.limits.refreshKey.entries))
	}
	clock.advance(loginWindow)
	jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{"email": "USER@example.com", "password": "x", "device_name": "Mac"}, "", http.StatusOK)

	unknownClock := &fakeHTTPClock{now: clock.Now()}
	unknownService := newFakeSessions()
	unknownService.loginErr = identity.ErrInvalidCredentials
	unknownHandler, _, _, unknownCleanup := newHandlerTestWithClock(t, unknownService, unknownClock)
	defer unknownCleanup()
	for index := 0; index < loginKeyLimit; index++ {
		response := jsonRequest(t, unknownHandler, http.MethodPost, "/v1/auth/login", map[string]string{
			"email": "unknown@example.com", "password": "x", "device_name": "Mac",
		}, "", http.StatusUnauthorized)
		assertError(t, response, http.StatusUnauthorized, "invalid_credentials")
	}
	limited = jsonRequest(t, unknownHandler, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": "unknown@example.com", "password": "x", "device_name": "Mac",
	}, "", http.StatusTooManyRequests)
	assertError(t, limited, http.StatusTooManyRequests, "rate_limited")
}

func TestConcurrentLoginWorkIsBounded(t *testing.T) {
	service := newFakeSessions()
	service.loginBlock = make(chan struct{})
	service.loginStarted = make(chan struct{}, maxConcurrentLogin)
	handler, _, api, cleanup := newHandlerTest(t, service)
	defer cleanup()

	var group sync.WaitGroup
	for index := 0; index < maxConcurrentLogin; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{
				"email": fmt.Sprintf("person-%d@example.com", index), "password": "x", "device_name": "Mac",
			}, "", http.StatusOK)
		}(index)
	}
	for range maxConcurrentLogin {
		<-service.loginStarted
	}
	limited := jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": "overflow@example.com", "password": "x", "device_name": "Mac",
	}, "", http.StatusTooManyRequests)
	assertError(t, limited, http.StatusTooManyRequests, "rate_limited")
	if len(api.limits.loginKey.entries) != maxConcurrentLogin {
		t.Fatalf("slot overflow consumed limiter key: keys=%d", len(api.limits.loginKey.entries))
	}
	close(service.loginBlock)
	group.Wait()
}

func TestLogsContainNoCredentialsDynamicIDsOrQuery(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler, _, _, cleanup := newHandlerTestWithLogger(t, newFakeSessions(), nil, logger)
	defer cleanup()

	email, password := "private-person@example.com", "extremely-secret-password"
	jsonRequest(t, handler, http.MethodPost, "/v1/auth/login", map[string]string{"email": email, "password": password, "device_name": "Secret Device"}, "", http.StatusOK)
	assertRequest(t, handler, http.MethodDelete, "/v1/devices/"+targetID.String(), "", testAccess, http.StatusOK)
	assertRequest(t, handler, http.MethodGet, "/unknown-private-path?refresh_token="+testRefresh, "", "", http.StatusBadRequest)

	output := logs.String()
	for _, secret := range []string{email, password, testAccess, testRefresh, targetID.String(), "unknown-private-path", "Secret Device"} {
		if strings.Contains(output, secret) {
			t.Errorf("log leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, `"route":"/v1/devices/{id}"`) || !strings.Contains(output, `"route":"unknown"`) {
		t.Fatalf("logs missing bounded routes: %s", output)
	}
}

func TestBlobPutGetAndStrictTransport(t *testing.T) {
	t.Parallel()
	handler, _, api, cleanup := newHandlerTest(t, newFakeSessions())
	defer cleanup()
	blobs := newFakeBlobUser()
	api.blobForUser = func(userID uuid.UUID) (BlobUserService, error) {
		if userID != testUserID {
			t.Fatalf("blob binder user=%s", userID)
		}
		return blobs, nil
	}
	content := []byte("# authenticated blob\n")
	hash := sha256.Sum256(content)
	path := "/v1/blobs/" + hex.EncodeToString(hash[:])
	request := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(content))
	request.Header.Set("Authorization", "Bearer "+testAccess)
	request.Header.Set("Content-Type", "application/octet-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"size":21`) {
		t.Fatalf("blob put=%d %s", response.Code, response.Body.String())
	}
	response = assertRequest(t, handler, http.MethodGet, path, "", testAccess, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), content) || response.Header().Get("Content-Type") != "application/octet-stream" ||
		response.Header().Get("Content-Length") != fmt.Sprint(len(content)) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("blob get headers/body=%v %q", response.Header(), response.Body.Bytes())
	}

	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Content-Type", "application/octet-stream; charset=utf-8") },
		func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") },
		func(r *http.Request) { r.Header.Set("Range", "bytes=0-1") },
		func(r *http.Request) { r.ContentLength = -1 },
	} {
		request = httptest.NewRequest(http.MethodPut, path, bytes.NewReader(content))
		request.Header.Set("Authorization", "Bearer "+testAccess)
		request.Header.Set("Content-Type", "application/octet-stream")
		mutate(request)
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assertError(t, response, http.StatusBadRequest, "invalid_request")
	}
	request = httptest.NewRequest(http.MethodPut, path, bytes.NewReader(content))
	request.Header.Set("Authorization", "Bearer "+testAccess)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.ContentLength = int64(len(content) - 1)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	request = httptest.NewRequest(http.MethodPut, path, bytes.NewReader(content))
	request.Header.Set("Authorization", "Bearer "+testAccess)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.ContentLength = int64(len(content) + 1)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	request = httptest.NewRequest(http.MethodPut, path, http.NoBody)
	request.Header.Set("Authorization", "Bearer "+testAccess)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.ContentLength = blob.MaxBlobBytes + 1
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusRequestEntityTooLarge, "blob_too_large")
}

func TestBlobAuthOrderingErrorsAndLogSecrecy(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	handler, _, api, cleanup := newHandlerTestWithLogger(t, newFakeSessions(), nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer cleanup()
	request := httptest.NewRequest(http.MethodGet, "/v1/blobs/NOT-A-HASH", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusUnauthorized, "invalid_session")
	request = httptest.NewRequest(http.MethodGet, "/v1/blobs/NOT-A-HASH?probe=true", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusUnauthorized, "invalid_session")

	content := []byte("private blob")
	hash := sha256.Sum256(content)
	rawHash := hex.EncodeToString(hash[:])
	blobs := newFakeBlobUser()
	blobs.getErr = blob.ErrUnavailable
	api.blobForUser = func(uuid.UUID) (BlobUserService, error) { return blobs, nil }
	response = assertRequest(t, handler, http.MethodGet, "/v1/blobs/"+rawHash, "", testAccess, http.StatusNotFound)
	assertError(t, response, http.StatusNotFound, "blob_not_found")
	blobs.putErr = blob.ErrQuotaExceeded
	request = httptest.NewRequest(http.MethodPut, "/v1/blobs/"+rawHash, bytes.NewReader(content))
	request.Header.Set("Authorization", "Bearer "+testAccess)
	request.Header.Set("Content-Type", "application/octet-stream")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusRequestEntityTooLarge, "quota_exceeded")
	if strings.Contains(logs.String(), rawHash) || !strings.Contains(logs.String(), `"route":"/v1/blobs/{hash}"`) {
		t.Fatalf("blob log leaked hash or route missing: %s", logs.String())
	}
}

func TestBlobCrossTenantAndUnknownAreIndistinguishable(t *testing.T) {
	t.Parallel()
	service := newFakeSessions()
	service.principals = map[string]session.Principal{
		testAccess:  service.principal,
		testAccessB: {UserID: testUserBID, DeviceID: testDeviceID, SessionID: testSessionID},
	}
	handler, _, api, cleanup := newHandlerTest(t, service)
	defer cleanup()
	owner, foreign := newFakeBlobUser(), newFakeBlobUser()
	content := []byte("tenant A only")
	hash := sha256.Sum256(content)
	owner.content[hash] = content
	api.blobForUser = func(userID uuid.UUID) (BlobUserService, error) {
		if userID == testUserID {
			return owner, nil
		}
		return foreign, nil
	}
	foreignResponse := assertRequest(t, handler, http.MethodGet, "/v1/blobs/"+hex.EncodeToString(hash[:]), "", testAccessB, http.StatusNotFound)
	unknown := sha256.Sum256([]byte("unknown"))
	unknownResponse := assertRequest(t, handler, http.MethodGet, "/v1/blobs/"+hex.EncodeToString(unknown[:]), "", testAccessB, http.StatusNotFound)
	if foreignResponse.Body.String() != unknownResponse.Body.String() {
		t.Fatalf("foreign=%q unknown=%q", foreignResponse.Body.String(), unknownResponse.Body.String())
	}
}

func TestBlobUploadConcurrencyIsBounded(t *testing.T) {
	handler, _, api, cleanup := newHandlerTest(t, newFakeSessions())
	defer cleanup()
	blobs := newFakeBlobUser()
	blobs.block = make(chan struct{})
	blobs.started = make(chan struct{}, maxConcurrentBlobs)
	api.blobForUser = func(uuid.UUID) (BlobUserService, error) { return blobs, nil }
	content := []byte("bounded blob")
	hash := sha256.Sum256(content)
	path := "/v1/blobs/" + hex.EncodeToString(hash[:])
	var group sync.WaitGroup
	for range maxConcurrentBlobs {
		group.Add(1)
		go func() {
			defer group.Done()
			request := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(content))
			request.Header.Set("Authorization", "Bearer "+testAccess)
			request.Header.Set("Content-Type", "application/octet-stream")
			handler.ServeHTTP(httptest.NewRecorder(), request)
		}()
	}
	for range maxConcurrentBlobs {
		<-blobs.started
	}
	request := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(content))
	request.Header.Set("Authorization", "Bearer "+testAccess)
	request.Header.Set("Content-Type", "application/octet-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusTooManyRequests, "rate_limited")
	close(blobs.block)
	group.Wait()
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := New(db, &State{}, nil, Dependencies{}); err == nil {
		t.Fatal("New accepted missing session dependency")
	}
}

func newHandlerTest(t *testing.T, service *fakeSessions) (http.Handler, *State, *handler, func()) {
	t.Helper()
	return newHandlerTestWithLogger(t, service, nil, nil)
}

func newHandlerTestWithClock(t *testing.T, service *fakeSessions, clock Clock) (http.Handler, *State, *handler, func()) {
	t.Helper()
	return newHandlerTestWithLogger(t, service, clock, nil)
}

func newHandlerTestWithLogger(t *testing.T, service *fakeSessions, clock Clock, logger *slog.Logger) (http.Handler, *State, *handler, func()) {
	t.Helper()
	if service == nil {
		service = newFakeSessions()
	}
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state := &State{}
	if clock == nil {
		clock = &fakeHTTPClock{now: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
	}
	fakeBlobs := newFakeBlobUser()
	api := &handler{
		db: db, state: state, sessions: service, limits: newAbuseLimiters(clock),
		loginSlots: make(chan struct{}, maxConcurrentLogin), blobSlots: make(chan struct{}, maxConcurrentBlobs),
		blobForUser: func(uuid.UUID) (BlobUserService, error) { return fakeBlobs, nil },
	}
	wrapped := requestLog(loggerOrDiscard(logger), securityHeaders(api))
	return wrapped, state, api, func() { db.Close() }
}

func loggerOrDiscard(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return logger
}

func jsonRequest(t *testing.T, handler http.Handler, method, path string, body any, access string, status int) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if access != "" {
		request.Header.Set("Authorization", "Bearer "+access)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status {
		t.Fatalf("%s %s status=%d body=%s want=%d", method, path, response.Code, response.Body.String(), status)
	}
	return response
}

func assertRequest(t *testing.T, handler http.Handler, method, path, body, authorization string, status int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if authorization != "" {
		if strings.HasPrefix(authorization, "Bearer") || strings.HasPrefix(authorization, "bearer") {
			request.Header.Set("Authorization", authorization)
		} else {
			request.Header.Set("Authorization", "Bearer "+authorization)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != status {
		t.Fatalf("%s %s status=%d body=%s want=%d", method, path, response.Code, response.Body.String(), status)
	}
	return response
}

func assertError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || !strings.Contains(response.Body.String(), `"error":"`+code+`"`) {
		t.Fatalf("error response status/body=%d/%s want=%d/%s", response.Code, response.Body.String(), status, code)
	}
	for name, want := range map[string]string{
		"Cache-Control": "no-store", "Content-Type": "application/json; charset=utf-8",
		"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("header %s=%q want=%q", name, got, want)
		}
	}
}

func assertJSONPath(t *testing.T, response *httptest.ResponseRecorder, object, key, want string) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	container := decoded
	if object != "" {
		value, ok := decoded[object].(map[string]any)
		if !ok {
			t.Fatalf("missing object %q in %#v", object, decoded)
		}
		container = value
	}
	if got := fmt.Sprint(container[key]); got != want {
		t.Fatalf("%s.%s=%q want=%q", object, key, got, want)
	}
}

type fakeSessions struct {
	mu                                    sync.Mutex
	principal                             session.Principal
	principals                            map[string]session.Principal
	loginResult                           session.LoginResult
	refreshTokens                         session.Tokens
	items                                 []session.SessionInfo
	loginErr, refreshErr, authenticateErr error
	loginBlock                            chan struct{}
	loginStarted                          chan struct{}
	renamedID, revokedDevice              uuid.UUID
	renamedName                           string
	revokedSessions                       []uuid.UUID
	receivedAccess                        []string
}

func newFakeSessions() *fakeSessions {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	principal := session.Principal{UserID: testUserID, DeviceID: testDeviceID, SessionID: testSessionID}
	tokens := session.Tokens{AccessToken: testAccess, RefreshToken: testRefresh, AccessExpiresAt: now.Add(15 * time.Minute), RefreshExpiresAt: now.Add(30 * 24 * time.Hour)}
	return &fakeSessions{
		principal: principal, loginResult: session.LoginResult{Principal: principal, Tokens: tokens}, refreshTokens: tokens,
		items: []session.SessionInfo{{SessionID: testSessionID, DeviceID: testDeviceID, DeviceName: "My Mac", Status: "active", CreatedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour), Current: true}},
	}
}

func (f *fakeSessions) Login(_ context.Context, email, _, _ string) (session.LoginResult, error) {
	f.mu.Lock()
	err, result := f.loginErr, f.loginResult
	block, started := f.loginBlock, f.loginStarted
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return session.LoginResult{}, err
	}
	return result, nil
}
func (f *fakeSessions) AuthenticateAccess(_ context.Context, access string) (session.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receivedAccess = append(f.receivedAccess, access)
	if f.authenticateErr != nil {
		return session.Principal{}, f.authenticateErr
	}
	if f.principals != nil {
		principal, ok := f.principals[access]
		if !ok {
			return session.Principal{}, session.ErrUnauthenticated
		}
		return principal, nil
	}
	if access != testAccess {
		return session.Principal{}, session.ErrUnauthenticated
	}
	return f.principal, nil
}
func (f *fakeSessions) Refresh(_ context.Context, _ string) (session.Tokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshErr != nil {
		return session.Tokens{}, f.refreshErr
	}
	return f.refreshTokens, nil
}
func (f *fakeSessions) ListForUser(_ context.Context, access string) ([]session.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receivedAccess = append(f.receivedAccess, access)
	return append([]session.SessionInfo(nil), f.items...), nil
}
func (f *fakeSessions) RenameDevice(_ context.Context, access string, id uuid.UUID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receivedAccess = append(f.receivedAccess, access)
	f.renamedID, f.renamedName = id, name
	return nil
}
func (f *fakeSessions) RevokeSession(_ context.Context, access string, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receivedAccess = append(f.receivedAccess, access)
	f.revokedSessions = append(f.revokedSessions, id)
	return nil
}
func (f *fakeSessions) RevokeDevice(_ context.Context, access string, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receivedAccess = append(f.receivedAccess, access)
	f.revokedDevice = id
	return nil
}

type fakeBlobUser struct {
	mu             sync.Mutex
	content        map[[sha256.Size]byte][]byte
	putErr, getErr error
	block          chan struct{}
	started        chan struct{}
}

func newFakeBlobUser() *fakeBlobUser {
	return &fakeBlobUser{content: make(map[[sha256.Size]byte][]byte)}
}

func (f *fakeBlobUser) Put(_ context.Context, expected [sha256.Size]byte, source io.Reader) (blob.PutResult, error) {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	content, err := io.ReadAll(source)
	if err != nil {
		return blob.PutResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return blob.PutResult{}, f.putErr
	}
	actual := sha256.Sum256(content)
	if actual != expected {
		return blob.PutResult{}, blob.ErrHashMismatch
	}
	f.content[expected] = append([]byte(nil), content...)
	return blob.PutResult{Hash: expected, Size: int64(len(content))}, nil
}

func (f *fakeBlobUser) Get(_ context.Context, hash [sha256.Size]byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	content, ok := f.content[hash]
	if !ok {
		return nil, blob.ErrUnavailable
	}
	return append([]byte(nil), content...), nil
}

type fakeHTTPClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeHTTPClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeHTTPClock) advance(value time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(value)
	c.mu.Unlock()
}
