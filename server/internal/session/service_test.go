package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/faulander/remember/server/internal/database"
	"github.com/google/uuid"
)

func TestLoginStoresOnlyHashesAndAuthenticatesOpaqueAccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login, err := f.service.Login(context.Background(), "a@example.com", "password", "  My Mac  ")
	if err != nil {
		t.Fatal(err)
	}
	if login.Principal.UserID != f.userA || !validV7(login.Principal.DeviceID) || !validV7(login.Principal.SessionID) {
		t.Fatalf("principal = %#v", login.Principal)
	}
	if len(login.Tokens.AccessToken) != 43 || len(login.Tokens.RefreshToken) != 43 {
		t.Fatalf("token lengths = %d/%d", len(login.Tokens.AccessToken), len(login.Tokens.RefreshToken))
	}
	principal, err := f.service.AuthenticateAccess(context.Background(), login.Tokens.AccessToken)
	if err != nil || principal != login.Principal {
		t.Fatalf("AuthenticateAccess = %#v, %v", principal, err)
	}
	accessRaw, _ := base64.RawURLEncoding.DecodeString(login.Tokens.AccessToken)
	refreshRaw, _ := base64.RawURLEncoding.DecodeString(login.Tokens.RefreshToken)
	var accessHash, refreshHash []byte
	if err := f.db.QueryRow("SELECT token_hash FROM access_tokens").Scan(&accessHash); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow("SELECT token_hash FROM refresh_tokens").Scan(&refreshHash); err != nil {
		t.Fatal(err)
	}
	if len(accessHash) != 32 || len(refreshHash) != 32 || bytes.Equal(accessRaw, accessHash) || bytes.Equal(refreshRaw, refreshHash) {
		t.Fatal("raw token material was stored or hashes have wrong length")
	}
	list, err := f.service.ListForUser(context.Background(), login.Tokens.AccessToken)
	if err != nil || len(list) != 1 || !list[0].Current || list[0].DeviceName != "My Mac" {
		t.Fatalf("list = %#v, %v", list, err)
	}
}

func TestLoginValidationCredentialAndRandomRollback(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, name := range []string{"", " \t", strings.Repeat("x", 101), strings.Repeat("é", 100) + "x", strings.Repeat("😀", 65), string([]byte{0xff})} {
		if _, err := f.service.Login(context.Background(), "a@example.com", "password", name); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("invalid name %q error = %v", name, err)
		}
	}
	if _, err := f.service.Login(context.Background(), "missing@example.com", "password", "Mac"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("credential error = %v", err)
	}
	failed, err := NewService(f.db, f.auth, f.clock, failingReader{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failed.Login(context.Background(), "a@example.com", "password", "Mac"); err == nil {
		t.Fatal("random failure accepted")
	}
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM devices").Scan(&count); err != nil || count != 0 {
		t.Fatalf("device count after failed login = %d, %v", count, err)
	}
}

func TestAccessRejectsMalformedExpiredAndFutureTokens(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", "abc", strings.Repeat("A", 43), "***************************************bad"} {
		f := newFixture(t)
		if _, err := f.service.AuthenticateAccess(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("malformed access %q error = %v", token, err)
		}
		if _, err := f.service.Refresh(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("malformed refresh %q error = %v", token, err)
		}
	}
	f := newFixture(t)
	login := mustLogin(t, f, "a@example.com", "Mac")
	f.clock.set(f.clock.now.Add(AccessTTL))
	if _, err := f.service.AuthenticateAccess(context.Background(), login.Tokens.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired access error = %v", err)
	}
	f.clock.set(login.Tokens.RefreshExpiresAt)
	if _, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired refresh error = %v", err)
	}

	future := newFixture(t)
	futureLogin := mustLogin(t, future, "a@example.com", "Mac")
	if _, err := future.db.Exec("UPDATE access_tokens SET issued_at_ms=issued_at_ms+60000, expires_at_ms=expires_at_ms+60000"); err != nil {
		t.Fatal(err)
	}
	if _, err := future.service.AuthenticateAccess(context.Background(), futureLogin.Tokens.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("future access error = %v", err)
	}
	if _, err := future.db.Exec("UPDATE refresh_tokens SET issued_at_ms=issued_at_ms+60000, expires_at_ms=expires_at_ms+60000"); err != nil {
		t.Fatal(err)
	}
	if _, err := future.service.Refresh(context.Background(), futureLogin.Tokens.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("future refresh error = %v", err)
	}
}

func TestRefreshRotatesOnceAndReplayRevokesFamily(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login := mustLogin(t, f, "a@example.com", "Mac")
	rotated, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.RefreshToken == login.Tokens.RefreshToken || rotated.AccessToken == login.Tokens.AccessToken {
		t.Fatal("refresh did not rotate both credentials")
	}
	if _, err := f.service.AuthenticateAccess(context.Background(), rotated.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replay error = %v", err)
	}
	if _, err := f.service.AuthenticateAccess(context.Background(), rotated.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("access survived replay = %v", err)
	}
	if _, err := f.service.Refresh(context.Background(), rotated.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("refresh family survived replay = %v", err)
	}
}

func TestRefreshReplayRevokesAfterClockRollback(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login := mustLogin(t, f, "a@example.com", "Mac")
	f.clock.set(f.clock.now.Add(time.Minute))
	rotated, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.set(f.clock.now.Add(-30 * time.Second))
	if _, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("rollback replay error = %v", err)
	}
	f.clock.set(f.clock.now.Add(time.Minute))
	if _, err := f.service.AuthenticateAccess(context.Background(), rotated.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("rotated access survived rollback replay = %v", err)
	}
}

func TestConcurrentRefreshAcrossServicesHasOneWinnerAndRevokesOnReplay(t *testing.T) {
	f := newFixture(t)
	login := mustLogin(t, f, "a@example.com", "Mac")
	otherDB, err := database.Open(context.Background(), f.path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	other, err := NewService(otherDB, f.auth, f.clock, &lockedReader{})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan struct {
		tokens Tokens
		err    error
	}, 2)
	for _, service := range []*Service{f.service, other} {
		go func(service *Service) {
			<-start
			tokens, err := service.Refresh(context.Background(), login.Tokens.RefreshToken)
			results <- struct {
				tokens Tokens
				err    error
			}{tokens, err}
		}(service)
	}
	close(start)
	var success int
	var winner Tokens
	for range 2 {
		result := <-results
		if result.err == nil {
			success++
			winner = result.tokens
		} else if !errors.Is(result.err, ErrUnauthenticated) {
			t.Fatalf("unexpected cross-service loser error = %v", result.err)
		}
	}
	if success != 1 {
		t.Fatalf("cross-service refresh successes = %d", success)
	}
	if _, err := f.service.AuthenticateAccess(context.Background(), winner.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("cross-service replay did not revoke winner: %v", err)
	}
}

func TestConcurrentRefreshHasOneWinnerAndRevokesOnReplay(t *testing.T) {
	f := newFixture(t)
	login := mustLogin(t, f, "a@example.com", "Mac")
	start := make(chan struct{})
	results := make(chan struct {
		tokens Tokens
		err    error
	}, 2)
	for range 2 {
		go func() {
			<-start
			tokens, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken)
			results <- struct {
				tokens Tokens
				err    error
			}{tokens, err}
		}()
	}
	close(start)
	var success int
	var winner Tokens
	for range 2 {
		result := <-results
		if result.err == nil {
			success++
			winner = result.tokens
		} else if !errors.Is(result.err, ErrUnauthenticated) {
			t.Fatalf("unexpected loser error = %v", result.err)
		}
	}
	if success != 1 {
		t.Fatalf("refresh successes = %d", success)
	}
	if _, err := f.service.AuthenticateAccess(context.Background(), winner.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replay did not revoke winner family: %v", err)
	}
}

func TestRefreshRandomFailureDoesNotConsumeToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login := mustLogin(t, f, "a@example.com", "Mac")
	failed, err := NewService(f.db, f.auth, f.clock, failingReader{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failed.Refresh(context.Background(), login.Tokens.RefreshToken); err == nil {
		t.Fatal("random failure accepted")
	}
	if _, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken); err != nil {
		t.Fatalf("random failure consumed refresh token: %v", err)
	}
}

func TestRefreshDatabaseFailureRollsBackRotation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	login := mustLogin(t, f, "a@example.com", "Mac")
	if _, err := f.db.Exec(`CREATE TRIGGER fail_rotated_access BEFORE INSERT ON access_tokens
		WHEN (SELECT COUNT(*) FROM access_tokens) > 0 BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken); err == nil {
		t.Fatal("injected database failure accepted")
	}
	if _, err := f.db.Exec("DROP TRIGGER fail_rotated_access"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken); err != nil {
		t.Fatalf("database failure partially consumed token: %v", err)
	}
}

func TestTenantBoundRenameListAndRevocation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a1 := mustLogin(t, f, "a@example.com", "A one")
	a2 := mustLogin(t, f, "a@example.com", "A two")
	b := mustLogin(t, f, "b@example.com", "B")
	list, err := f.service.ListForUser(context.Background(), a1.Tokens.AccessToken)
	if err != nil || len(list) != 2 {
		t.Fatalf("tenant A list length = %d, %v", len(list), err)
	}
	for _, item := range list {
		if item.DeviceID == b.Principal.DeviceID {
			t.Fatal("cross-tenant device leaked into list")
		}
	}
	if err := f.service.RenameDevice(context.Background(), a1.Tokens.AccessToken, b.Principal.DeviceID, "stolen"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant rename = %v", err)
	}
	if err := f.service.RevokeSession(context.Background(), a1.Tokens.AccessToken, b.Principal.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant session revoke = %v", err)
	}
	if err := f.service.RevokeDevice(context.Background(), a1.Tokens.AccessToken, b.Principal.DeviceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant device revoke = %v", err)
	}
	if err := f.service.RenameDevice(context.Background(), a1.Tokens.AccessToken, a2.Principal.DeviceID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RevokeSession(context.Background(), a1.Tokens.AccessToken, a2.Principal.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RevokeDevice(context.Background(), a1.Tokens.AccessToken, a2.Principal.DeviceID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RenameDevice(context.Background(), a1.Tokens.AccessToken, a2.Principal.DeviceID, "revived"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked target rename = %v", err)
	}
	if _, err := f.service.AuthenticateAccess(context.Background(), a2.Tokens.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked session access = %v", err)
	}
	if _, err := f.service.AuthenticateAccess(context.Background(), a1.Tokens.AccessToken); err != nil {
		t.Fatalf("actor session unexpectedly revoked: %v", err)
	}
	if err := f.service.RevokeDevice(context.Background(), a1.Tokens.AccessToken, a1.Principal.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.AuthenticateAccess(context.Background(), a1.Tokens.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked device access = %v", err)
	}
}

func TestRevocationHandlesClockRollback(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	actor := mustLogin(t, f, "a@example.com", "Actor")
	f.clock.set(f.clock.now.Add(time.Minute))
	target := mustLogin(t, f, "a@example.com", "Target")
	f.clock.set(f.clock.now.Add(-30 * time.Second))
	if err := f.service.RevokeDevice(context.Background(), actor.Tokens.AccessToken, target.Principal.DeviceID); err != nil {
		t.Fatalf("rollback device revocation = %v", err)
	}
	f.clock.set(f.clock.now.Add(time.Minute))
	if _, err := f.service.AuthenticateAccess(context.Background(), target.Tokens.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("target survived rollback revocation = %v", err)
	}
}

func TestInactiveAccountAndDeviceFailGenerically(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"account", "device"} {
		f := newFixture(t)
		login := mustLogin(t, f, "a@example.com", "Mac")
		var err error
		if target == "account" {
			_, err = f.db.Exec("UPDATE users SET status='deletion_pending' WHERE id=?", f.userA[:])
		} else {
			_, err = f.db.Exec("UPDATE devices SET status='revoked',revoked_at_ms=? WHERE user_id=? AND id=?", f.clock.now.UnixMilli(), f.userA[:], login.Principal.DeviceID[:])
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.AuthenticateAccess(context.Background(), login.Tokens.AccessToken); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s access error = %v", target, err)
		}
		if _, err := f.service.Refresh(context.Background(), login.Tokens.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s refresh error = %v", target, err)
		}
	}
}

func TestAccountCascadeRemovesSessionTokenGraph(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_ = mustLogin(t, f, "a@example.com", "Mac")
	if _, err := f.db.Exec("DELETE FROM users WHERE id=?", f.userA[:]); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"devices", "sessions", "access_tokens", "refresh_tokens"} {
		var count int
		if err := f.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s count = %d, %v", table, count, err)
		}
	}
}

type fixture struct {
	db      *sql.DB
	service *Service
	auth    *fakeAuthenticator
	clock   *fakeClock
	userA   uuid.UUID
	userB   uuid.UUID
	path    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.db")
	db, err := database.Open(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	userA := uuid.MustParse("018f0000-0000-7000-8000-000000000001")
	userB := uuid.MustParse("018f0000-0000-7000-8000-000000000002")
	for _, user := range []struct {
		id    uuid.UUID
		email string
	}{{userA, "a@example.com"}, {userB, "b@example.com"}} {
		if _, err := db.Exec(`INSERT INTO users(id,email_delivery,email_canonical,password_hash,password_policy,status,created_at_ms,verified_at_ms)
			VALUES(?,?,?,'test hash',1,'active',1,1)`, user.id[:], user.email, user.email); err != nil {
			t.Fatal(err)
		}
	}
	auth := &fakeAuthenticator{users: map[string]uuid.UUID{"a@example.com": userA, "b@example.com": userB}}
	clock := &fakeClock{now: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	service, err := NewService(db, auth, clock, &lockedReader{})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db: db, service: service, auth: auth, clock: clock, userA: userA, userB: userB, path: path}
}

func mustLogin(t *testing.T, f *fixture, email, name string) LoginResult {
	t.Helper()
	login, err := f.service.Login(context.Background(), email, "password", name)
	if err != nil {
		t.Fatal(err)
	}
	return login
}

type fakeAuthenticator struct{ users map[string]uuid.UUID }

func (a *fakeAuthenticator) AuthenticateCredential(_ context.Context, email, _ string) (uuid.UUID, error) {
	id, ok := a.users[email]
	if !ok {
		return uuid.Nil, ErrUnauthenticated
	}
	return id, nil
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

type lockedReader struct {
	mu   sync.Mutex
	next byte
}

func (r *lockedReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range buffer {
		r.next++
		buffer[i] = r.next
	}
	return len(buffer), nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
