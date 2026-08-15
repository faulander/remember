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
	synccore "github.com/faulander/remember/server/internal/sync"
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

func TestRegistrationAndVerificationContracts(t *testing.T) {
	t.Parallel()
	handler, _, api, cleanup := newHandlerTest(t, nil)
	defer cleanup()
	identities := &fakeIdentity{}
	api.identity, api.registrationEnabled = identities, true
	password := "correct horse battery staple"
	response := jsonRequest(t, handler, http.MethodPost, "/v1/auth/register", map[string]string{
		"email": "Person@example.com", "password": password,
	}, "", http.StatusAccepted)
	if response.Body.String() != "{\"status\":\"verification_required\"}\n" || strings.Contains(response.Body.String(), password) || strings.Contains(response.Body.String(), identities.registration.VerificationToken) {
		t.Fatalf("registration response = %s", response.Body.String())
	}
	for range registrationKeyLimit - 1 {
		jsonRequest(t, handler, http.MethodPost, "/v1/auth/register", map[string]string{
			"email": "person@example.com", "password": password,
		}, "", http.StatusAccepted)
	}
	rateLimited := jsonRequest(t, handler, http.MethodPost, "/v1/auth/register", map[string]string{
		"email": "PERSON@example.com", "password": password,
	}, "", http.StatusTooManyRequests)
	assertError(t, rateLimited, http.StatusTooManyRequests, "rate_limited")
	if identities.email != "person@example.com" || identities.password != password || identities.registerCalls != registrationKeyLimit {
		t.Fatalf("registration calls=%d email=%q password_match=%v", identities.registerCalls, identities.email, identities.password == password)
	}
	token := strings.Repeat("A", 43)
	verified := jsonRequest(t, handler, http.MethodPost, "/v1/auth/verify-email", map[string]string{"token": token}, "", http.StatusOK)
	if verified.Body.String() != "{\"status\":\"verified\"}\n" || identities.verifiedToken != token {
		t.Fatalf("verification response=%s token=%q", verified.Body.String(), identities.verifiedToken)
	}
	identities.verifyErr = identity.ErrInvalidVerificationToken
	invalid := jsonRequest(t, handler, http.MethodPost, "/v1/auth/verify-email", map[string]string{"token": strings.Repeat("B", 43)}, "", http.StatusBadRequest)
	assertError(t, invalid, http.StatusBadRequest, "invalid_verification")
	api.registrationEnabled = false
	unavailable := jsonRequest(t, handler, http.MethodPost, "/v1/auth/register", map[string]string{"email": "new@example.com", "password": password}, "", http.StatusServiceUnavailable)
	assertError(t, unavailable, http.StatusServiceUnavailable, "registration_unavailable")
	api.registrationEnabled = true
	identities.registerErr = identity.ErrRegistrationUnavailable
	unavailable = jsonRequest(t, handler, http.MethodPost, "/v1/auth/register", map[string]string{"email": "other@example.com", "password": password}, "", http.StatusServiceUnavailable)
	assertError(t, unavailable, http.StatusServiceUnavailable, "registration_unavailable")
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
		{"duplicate key", "application/json", `{"email":"a","email":"b","password":"c","device_name":"d"}`},
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

func TestSyncSubmitPullDTOAndPrincipalBinding(t *testing.T) {
	t.Parallel()
	handler, _, api, cleanup := newHandlerTest(t, newFakeSessions())
	defer cleanup()
	actor := newFakeSyncActor()
	blobHash := sha256.Sum256([]byte("sync blob"))
	parent := targetID
	actor.submitResult = synccore.SubmitResult{Accepted: true, Revision: 3, Cursor: 7}
	actor.pullResult = synccore.PullResult{
		Changes: []synccore.VersionState{
			{Cursor: 7, Mutation: synccore.MutationCreate, OperationID: testSessionID, ObjectID: targetID, ObjectType: synccore.ObjectNote, Revision: 3, Name: "Note.md", BlobHash: blobHash[:]},
			{Cursor: 8, Mutation: synccore.MutationMove, OperationID: testDeviceID, ObjectID: testUserID, ObjectType: synccore.ObjectFolder, Revision: 2, ParentID: &parent, Name: "Folder", Deleted: false},
		}, HasMore: true, NextCursor: 8,
	}
	var boundUser, boundDevice uuid.UUID
	api.syncForActor = func(userID, deviceID uuid.UUID) (SyncActorService, error) {
		boundUser, boundDevice = userID, deviceID
		return actor, nil
	}
	body := validSyncMutationBody(blobHash)
	response := jsonRequest(t, handler, http.MethodPost, "/v1/sync/operations", body, testAccess, http.StatusOK)
	var submitted syncSubmitResponse
	if err := json.Unmarshal(response.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	if !submitted.Accepted || submitted.Revision == nil || *submitted.Revision != 3 || submitted.Cursor == nil || *submitted.Cursor != 7 || submitted.Conflict != nil {
		t.Fatalf("submit response=%#v", submitted)
	}
	actor.mu.Lock()
	mutation := actor.lastMutation
	actor.mu.Unlock()
	if boundUser != testUserID || boundDevice != testDeviceID || mutation.OperationID != testSessionID || mutation.ObjectID != targetID ||
		mutation.Kind != synccore.MutationCreate || mutation.ObjectType != synccore.ObjectNote || mutation.BaseRevision != 0 || mutation.ParentID != nil ||
		mutation.Name != "Note.md" || !bytes.Equal(mutation.BlobHash, blobHash[:]) {
		t.Fatalf("bound=%s/%s mutation=%#v", boundUser, boundDevice, mutation)
	}

	response = assertRequest(t, handler, http.MethodGet, "/v1/sync/changes?after=6&limit=2", "", testAccess, http.StatusOK)
	var pulled struct {
		Changes    []syncChangeResponse `json:"changes"`
		HasMore    bool                 `json:"has_more"`
		NextCursor uint64               `json:"next_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &pulled); err != nil {
		t.Fatal(err)
	}
	if len(pulled.Changes) != 2 || !pulled.HasMore || pulled.NextCursor != 8 || pulled.Changes[0].ParentID != nil ||
		pulled.Changes[0].BlobHash == nil || *pulled.Changes[0].BlobHash != hex.EncodeToString(blobHash[:]) ||
		pulled.Changes[1].ParentID == nil || *pulled.Changes[1].ParentID != parent.String() || pulled.Changes[1].BlobHash != nil {
		t.Fatalf("pull response=%#v", pulled)
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	if actor.lastAfter != 6 || actor.lastLimit != 2 {
		t.Fatalf("pull arguments=%d/%d", actor.lastAfter, actor.lastLimit)
	}
}

func TestPreserveDeleteFolderHTTPBinding(t *testing.T) {
	handler, _, api, cleanup := newHandlerTest(t, newFakeSessions())
	defer cleanup()
	actor := newFakeSyncActor()
	recovered, cloneID, note, source := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	hash := sha256.Sum256([]byte("note"))
	actor.preserveResult = synccore.PreserveDeleteFolderResult{RecoveredFolderID: recovered, RecoveredFolderName: "Recovered", RecoveredCursor: 10, DeletedCursor: 14, FirstCursor: 10, LastCursor: 14, Clones: []synccore.PreserveDeleteFolderClone{{OriginalFolderID: source, RecoveredFolderID: cloneID, SourceParentID: source, TargetParentID: recovered, CreateCursor: 11, DeleteCursor: 13, SourceRevision: 2, Depth: 1, Name: "Child"}}, NoteMoves: []synccore.PreserveDeleteNoteMove{{NoteID: note, SourceParentID: source, TargetParentID: cloneID, MoveCursor: 12, SourceRevision: 2, TargetRevision: 3, Name: "N.md", BlobHash: hash[:]}}}
	api.syncForActor = func(user, device uuid.UUID) (SyncActorService, error) {
		if user != testUserID || device != testDeviceID {
			t.Fatalf("actor=%s/%s", user, device)
		}
		return actor, nil
	}
	operation, conflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folder := actor.preserveResult.NoteMoves[0].SourceParentID
	body := map[string]any{"operation_id": operation.String(), "conflict_operation_id": conflict.String(), "folder_id": folder.String(), "expected_revision": 2, "request_version": 4, "known_cursor": 9}
	response := jsonRequest(t, handler, http.MethodPost, "/v1/sync/folder-preserve-delete", body, testAccess, http.StatusOK)
	if actor.lastPreserve.OperationID != operation || actor.lastPreserve.ConflictOperationID != conflict || actor.lastPreserve.FolderID != folder || actor.lastPreserve.ExpectedRevision != 2 || actor.lastPreserve.Version != 4 || actor.lastPreserve.KnownCursor != 9 {
		t.Fatalf("request=%#v", actor.lastPreserve)
	}
	if !strings.Contains(response.Body.String(), recovered.String()) || !strings.Contains(response.Body.String(), `"recovered_folder_name":"Recovered"`) || !strings.Contains(response.Body.String(), note.String()) || !strings.Contains(response.Body.String(), hex.EncodeToString(hash[:])) || !strings.Contains(response.Body.String(), `"source_parent_id"`) || !strings.Contains(response.Body.String(), `"target_parent_id"`) || !strings.Contains(response.Body.String(), `"depth":1`) {
		t.Fatalf("body=%s", response.Body.String())
	}
}

func TestPreserveDeleteFolderLegacyHTTPShapesRemainExact(t *testing.T) {
	handler, _, api, cleanup := newHandlerTest(t, newFakeSessions())
	defer cleanup()
	actor := newFakeSyncActor()
	root, original, recovered := uuid.New(), uuid.New(), uuid.New()
	hash := sha256.Sum256([]byte("note"))
	actor.preserveResult = synccore.PreserveDeleteFolderResult{
		RecoveredFolderID: recovered, RecoveredFolderName: "Recovered", RecoveredCursor: 10, DeletedCursor: 13, FirstCursor: 10, LastCursor: 13,
		Clones:    []synccore.PreserveDeleteFolderClone{{OriginalFolderID: original, RecoveredFolderID: uuid.New(), SourceParentID: root, TargetParentID: recovered, CreateCursor: 11, DeleteCursor: 12, SourceRevision: 2, Depth: 1, Name: "Child"}},
		NoteMoves: []synccore.PreserveDeleteNoteMove{{NoteID: uuid.New(), SourceParentID: root, TargetParentID: recovered, MoveCursor: 12, SourceRevision: 1, TargetRevision: 2, Name: "N.md", BlobHash: hash[:]}},
	}
	api.syncForActor = func(uuid.UUID, uuid.UUID) (SyncActorService, error) { return actor, nil }
	assertKeys := func(raw json.RawMessage, expected ...string) {
		t.Helper()
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if len(value) != len(expected) {
			t.Fatalf("keys=%v want=%v", value, expected)
		}
		for _, key := range expected {
			if _, ok := value[key]; !ok {
				t.Fatalf("missing key %q in %v", key, value)
			}
		}
	}
	for _, tc := range []struct {
		version      uint64
		responseKeys []string
		cloneKeys    []string
	}{
		{1, []string{"recovered_folder_id", "recovered_cursor", "deleted_cursor"}, nil},
		{2, []string{"recovered_folder_id", "recovered_cursor", "deleted_cursor", "first_cursor", "last_cursor", "clones"}, []string{"original_folder_id", "recovered_folder_id", "create_cursor", "delete_cursor"}},
		{3, []string{"recovered_folder_id", "recovered_folder_name", "recovered_cursor", "deleted_cursor", "first_cursor", "last_cursor", "clones", "note_moves"}, []string{"original_folder_id", "recovered_folder_id", "create_cursor", "delete_cursor", "source_revision", "name"}},
	} {
		body := map[string]any{"operation_id": uuid.Must(uuid.NewV7()).String(), "conflict_operation_id": uuid.Must(uuid.NewV7()).String(), "folder_id": root.String(), "expected_revision": 2, "request_version": tc.version}
		if tc.version >= 2 {
			body["known_cursor"] = 9
		}
		response := jsonRequest(t, handler, http.MethodPost, "/v1/sync/folder-preserve-delete", body, testAccess, http.StatusOK)
		assertKeys(response.Body.Bytes(), tc.responseKeys...)
		if tc.cloneKeys != nil {
			var decoded struct {
				Clones []json.RawMessage `json:"clones"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || len(decoded.Clones) != 1 {
				t.Fatalf("clones=%s err=%v", response.Body.String(), err)
			}
			assertKeys(decoded.Clones[0], tc.cloneKeys...)
		}
	}
}

func TestSyncConflictReplayAndErrorMapping(t *testing.T) {
	t.Parallel()
	handler, _, api, cleanup := newHandlerTest(t, newFakeSessions())
	defer cleanup()
	actor := newFakeSyncActor()
	api.syncForActor = func(uuid.UUID, uuid.UUID) (SyncActorService, error) { return actor, nil }
	hash := sha256.Sum256([]byte("blob"))
	body := validSyncMutationBody(hash)
	actor.submitResult = synccore.SubmitResult{Conflict: synccore.ConflictPathCollision, Canonical: &synccore.CanonicalState{ObjectType: synccore.ObjectNote, Revision: 3, Name: "Canonical.md", BlobHash: hash[:]}}
	for range 2 {
		response := jsonRequest(t, handler, http.MethodPost, "/v1/sync/operations", body, testAccess, http.StatusOK)
		var result syncSubmitResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Accepted || result.Conflict == nil || *result.Conflict != string(synccore.ConflictPathCollision) || result.Revision != nil || result.Cursor != nil || result.Canonical == nil || result.Canonical.Revision != 3 || result.Canonical.BlobHash == nil || *result.Canonical.BlobHash != hex.EncodeToString(hash[:]) {
			t.Fatalf("conflict response=%#v", result)
		}
	}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{synccore.ErrInvalidInput, http.StatusBadRequest, "invalid_request"},
		{synccore.ErrInactiveActor, http.StatusUnauthorized, "invalid_session"},
		{synccore.ErrBlobUnavailable, http.StatusConflict, "blob_unavailable"},
		{synccore.ErrOperationReplayMismatch, http.StatusConflict, "operation_replay_mismatch"},
		{errors.New("sensitive database detail"), http.StatusInternalServerError, "internal_error"},
	} {
		actor.submitErr = test.err
		response := jsonRequest(t, handler, http.MethodPost, "/v1/sync/operations", body, testAccess, test.status)
		assertError(t, response, test.status, test.code)
		if strings.Contains(response.Body.String(), "database") {
			t.Fatal("sync error leaked detail")
		}
	}
	actor.submitErr = nil
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{synccore.ErrInvalidInput, http.StatusBadRequest, "invalid_request"},
		{synccore.ErrInactiveActor, http.StatusUnauthorized, "invalid_session"},
		{errors.New("sensitive pull detail"), http.StatusInternalServerError, "internal_error"},
	} {
		actor.pullErr = test.err
		response := assertRequest(t, handler, http.MethodGet, "/v1/sync/changes", "", testAccess, test.status)
		assertError(t, response, test.status, test.code)
	}
}

func TestSyncStrictInputAuthOrderingMethodsAndQuery(t *testing.T) {
	t.Parallel()
	handler, _, api, cleanup := newHandlerTest(t, newFakeSessions())
	defer cleanup()
	actor := newFakeSyncActor()
	api.syncForActor = func(uuid.UUID, uuid.UUID) (SyncActorService, error) { return actor, nil }
	hash := sha256.Sum256([]byte("blob"))
	valid := validSyncMutationBody(hash)
	invalidBodies := []string{
		`{"operation_id":"` + testSessionID.String() + `","mutation":"create","object_id":"` + targetID.String() + `","object_type":"note","base_revision":0,"parent_id":null,"name":"Note.md","blob_hash":"` + hex.EncodeToString(hash[:]) + `","user_id":"` + testUserID.String() + `"}`,
		`{"operation_id":"` + strings.ToUpper(testSessionID.String()) + `","mutation":"create","object_id":"` + targetID.String() + `","object_type":"note","base_revision":0,"parent_id":null,"name":"Note.md","blob_hash":"` + hex.EncodeToString(hash[:]) + `"}`,
		`{"operation_id":"` + testSessionID.String() + `","mutation":"create","object_id":"` + targetID.String() + `","object_type":"note","base_revision":0,"parent_id":null,"name":"Note.md","blob_hash":"` + strings.ToUpper(hex.EncodeToString(hash[:])) + `"}`,
		`{"operation_id":"` + testSessionID.String() + `","mutation":"unknown","object_id":"` + targetID.String() + `","object_type":"note","base_revision":0,"parent_id":null,"name":"Note.md","blob_hash":"` + hex.EncodeToString(hash[:]) + `"}`,
		`{"operation_id":"` + testSessionID.String() + `","mutation":"create","object_id":"` + targetID.String() + `","object_type":"folder","name":"Folder"}`,
		`{"operation_id":"` + testSessionID.String() + `","mutation":"create","mutation":"delete","object_id":"` + targetID.String() + `","object_type":"folder","base_revision":0,"parent_id":null,"name":"Folder","blob_hash":null}`,
		`{"operation_id":"` + testSessionID.String() + `","mutation":"update","object_id":"` + targetID.String() + `","object_type":"note","base_revision":9223372036854775808,"parent_id":null,"name":"","blob_hash":"` + hex.EncodeToString(hash[:]) + `"}`,
	}
	for _, body := range invalidBodies {
		request := httptest.NewRequest(http.MethodPost, "/v1/sync/operations", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testAccess)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assertError(t, response, http.StatusBadRequest, "invalid_request")
	}
	legacy := validSyncMutationBody(hash)
	legacy["object_id"] = uuid.New().String()
	jsonRequest(t, handler, http.MethodPost, "/v1/sync/operations", legacy, testAccess, http.StatusOK)
	request := httptest.NewRequest(http.MethodPost, "/v1/sync/operations", strings.NewReader(`{`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusUnauthorized, "invalid_session")
	request = httptest.NewRequest(http.MethodPost, "/v1/sync/operations", strings.NewReader(string(mustJSON(t, valid))+` {}`))
	request.Header.Set("Authorization", "Bearer "+testAccess)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	request = httptest.NewRequest(http.MethodPost, "/v1/sync/operations", bytes.NewReader(mustJSON(t, valid)))
	request.Header.Set("Authorization", "Bearer "+testAccess)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertError(t, response, http.StatusBadRequest, "invalid_request")

	for _, query := range []string{
		"after=", "after=00", "after=%30", "after=%2B1", "after=-1", "after=9223372036854775808", "after=18446744073709551616",
		"after=0&", "after=1&after=2", "limit=01", "limit=-1", "limit=501", "limit=18446744073709551616", "limit=1&limit=2", "unknown=1",
	} {
		response := assertRequest(t, handler, http.MethodGet, "/v1/sync/changes?"+query, "", testAccess, http.StatusBadRequest)
		assertError(t, response, http.StatusBadRequest, "invalid_request")
	}
	response = assertRequest(t, handler, http.MethodGet, "/v1/sync/changes?after=bad", "invalid body", "", http.StatusUnauthorized)
	assertError(t, response, http.StatusUnauthorized, "invalid_session")
	response = assertRequest(t, handler, http.MethodGet, "/v1/sync/changes", "x", testAccess, http.StatusBadRequest)
	assertError(t, response, http.StatusBadRequest, "invalid_request")
	response = assertRequest(t, handler, http.MethodGet, "/v1/sync/changes", "", testAccess, http.StatusOK)
	actor.mu.Lock()
	if actor.lastAfter != 0 || actor.lastLimit != 0 {
		t.Fatalf("default pull arguments=%d/%d", actor.lastAfter, actor.lastLimit)
	}
	actor.mu.Unlock()
	for _, test := range []struct{ method, path, allow string }{
		{http.MethodGet, "/v1/sync/operations", "POST"},
		{http.MethodPost, "/v1/sync/changes", "GET"},
	} {
		response = assertRequest(t, handler, test.method, test.path, "", "", http.StatusMethodNotAllowed)
		if response.Header().Get("Allow") != test.allow {
			t.Fatalf("Allow=%q want=%q", response.Header().Get("Allow"), test.allow)
		}
	}
}

func TestSyncConcurrencyAndLogSecrecy(t *testing.T) {
	var logs bytes.Buffer
	handler, _, api, cleanup := newHandlerTestWithLogger(t, newFakeSessions(), nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	defer cleanup()
	actor := newFakeSyncActor()
	actor.block = make(chan struct{})
	actor.started = make(chan struct{}, maxConcurrentSync)
	api.syncForActor = func(uuid.UUID, uuid.UUID) (SyncActorService, error) { return actor, nil }
	hash := sha256.Sum256([]byte("highly private sync blob"))
	body := validSyncMutationBody(hash)
	encoded := mustJSON(t, body)
	var group sync.WaitGroup
	for range maxConcurrentSync {
		group.Add(1)
		go func() {
			defer group.Done()
			request := httptest.NewRequest(http.MethodPost, "/v1/sync/operations", bytes.NewReader(encoded))
			request.Header.Set("Authorization", "Bearer "+testAccess)
			request.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(httptest.NewRecorder(), request)
		}()
	}
	for range maxConcurrentSync {
		<-actor.started
	}
	response := jsonRequest(t, handler, http.MethodPost, "/v1/sync/operations", body, testAccess, http.StatusTooManyRequests)
	assertError(t, response, http.StatusTooManyRequests, "rate_limited")
	close(actor.block)
	group.Wait()
	querySecret := "987654321"
	assertRequest(t, handler, http.MethodGet, "/v1/sync/changes?after="+querySecret+"&limit=1", "", testAccess, http.StatusOK)
	output := logs.String()
	for _, secret := range []string{testSessionID.String(), targetID.String(), hex.EncodeToString(hash[:]), querySecret, "Note.md"} {
		if strings.Contains(output, secret) {
			t.Fatalf("sync log leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, `"route":"/v1/sync/operations"`) || !strings.Contains(output, `"route":"/v1/sync/changes"`) {
		t.Fatalf("sync log routes missing: %s", output)
	}
}

func validSyncMutationBody(hash [sha256.Size]byte) map[string]any {
	return map[string]any{
		"operation_id": testSessionID.String(), "mutation": "create", "object_id": targetID.String(),
		"object_type": "note", "base_revision": uint64(0), "parent_id": nil, "name": "Note.md",
		"blob_hash": hex.EncodeToString(hash[:]),
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
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
	fakeSync := newFakeSyncActor()
	api := &handler{
		db: db, state: state, sessions: service, limits: newAbuseLimiters(clock),
		loginSlots: make(chan struct{}, maxConcurrentLogin), blobSlots: make(chan struct{}, maxConcurrentBlobs),
		syncSlots:    make(chan struct{}, maxConcurrentSync),
		blobForUser:  func(uuid.UUID) (BlobUserService, error) { return fakeBlobs, nil },
		syncForActor: func(uuid.UUID, uuid.UUID) (SyncActorService, error) { return fakeSync, nil },
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

type fakeIdentity struct {
	registration                   identity.Registration
	email, password, verifiedToken string
	registerCalls                  int
	registerErr, verifyErr         error
}

func (f *fakeIdentity) Register(_ context.Context, email, password string) (identity.Registration, error) {
	f.email, f.password = email, password
	f.registerCalls++
	if f.registration == (identity.Registration{}) {
		f.registration = identity.Registration{Created: true, UserID: testUserID, VerificationToken: strings.Repeat("S", 43)}
	}
	return f.registration, f.registerErr
}

func (f *fakeIdentity) VerifyEmail(_ context.Context, token string) error {
	f.verifiedToken = token
	return f.verifyErr
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

type fakeSyncActor struct {
	mu             sync.Mutex
	submitResult   synccore.SubmitResult
	pullResult     synccore.PullResult
	submitErr      error
	pullErr        error
	preserveResult synccore.PreserveDeleteFolderResult
	preserveErr    error
	lastPreserve   synccore.PreserveDeleteFolderRequest
	lastMutation   synccore.Mutation
	lastAfter      uint64
	lastLimit      int
	block          chan struct{}
	started        chan struct{}
}

func newFakeSyncActor() *fakeSyncActor {
	return &fakeSyncActor{submitResult: synccore.SubmitResult{Accepted: true, Revision: 1, Cursor: 1}}
}

func (f *fakeSyncActor) Submit(_ context.Context, mutation synccore.Mutation) (synccore.SubmitResult, error) {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMutation = mutation
	return f.submitResult, f.submitErr
}

func (f *fakeSyncActor) PreserveAndDeleteEmptyFolder(_ context.Context, request synccore.PreserveDeleteFolderRequest) (synccore.PreserveDeleteFolderResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPreserve = request
	return f.preserveResult, f.preserveErr
}

func (f *fakeSyncActor) Pull(_ context.Context, after uint64, limit int) (synccore.PullResult, error) {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAfter, f.lastLimit = after, limit
	return f.pullResult, f.pullErr
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
