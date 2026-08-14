package emaildelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/faulander/remember/server/internal/database"
	"github.com/faulander/remember/server/internal/verificationtoken"
	"github.com/google/uuid"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

type recordingSender struct {
	recipient string
	token     string
	err       error
}

func (s *recordingSender) SendVerification(_ context.Context, recipient, token string) error {
	s.recipient, s.token = recipient, token
	return s.err
}

func TestDispatcherDeliversRetriesAndExpiresRegistrations(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "delivery.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	tokens, err := verificationtoken.NewCodec(make([]byte, verificationtoken.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, verificationTokenBytes))
	user := insertQueuedVerification(t, db, tokens, clock.now, token, "Person@example.com", time.Hour)
	sender := &recordingSender{}
	dispatcher, err := NewDispatcher(db, sender, clock, tokens)
	if err != nil {
		t.Fatal(err)
	}
	attempted, err := dispatcher.DispatchOne(ctx)
	if err != nil || !attempted || sender.recipient != "Person@example.com" || sender.token != token {
		t.Fatalf("DispatchOne() attempted=%v recipient=%q token_match=%v err=%v", attempted, sender.recipient, sender.token == token, err)
	}
	assertCount(t, db, "email_verification_outbox", 0)
	assertCount(t, db, "email_verifications", 1)

	retryToken := base64.RawURLEncoding.EncodeToString(bytesOf(1))
	insertQueuedVerification(t, db, tokens, clock.now, retryToken, "retry@example.com", time.Hour)
	sender.err = errors.New("backend detail")
	attempted, err = dispatcher.DispatchOne(ctx)
	if !attempted || err == nil || err.Error() != "verification delivery failed" {
		t.Fatalf("failed DispatchOne() attempted=%v err=%v", attempted, err)
	}
	var attempts, next int64
	if err := db.QueryRow("SELECT attempt_count,next_attempt_at_ms FROM email_verification_outbox WHERE recipient=?", "retry@example.com").Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || next != clock.now.Add(15*time.Second).UnixMilli() {
		t.Errorf("retry attempts=%d next=%d", attempts, next)
	}
	if attempted, err = dispatcher.DispatchOne(ctx); err != nil || attempted {
		t.Fatalf("early retry attempted=%v err=%v", attempted, err)
	}

	expiredToken := base64.RawURLEncoding.EncodeToString(bytesOf(2))
	expiredUser := insertQueuedVerification(t, db, tokens, clock.now.Add(-2*time.Hour), expiredToken, "expired@example.com", time.Hour)
	if attempted, err = dispatcher.DispatchOne(ctx); err != nil || attempted {
		t.Fatalf("expiry dispatch attempted=%v err=%v", attempted, err)
	}
	assertMissingUser(t, db, expiredUser)
	assertExistingUser(t, db, user)
}

func insertQueuedVerification(t *testing.T, db *sql.DB, tokens *verificationtoken.Codec, issued time.Time, token, recipient string, ttl time.Duration) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := db.Exec("INSERT INTO users(id,email_delivery,email_canonical,password_hash,password_policy,status,created_at_ms) VALUES(?,?,?,'hash',1,'pending_verification',?)", id[:], recipient, recipient, issued.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(token))
	if _, err := db.Exec("INSERT INTO email_verifications(user_id,token_hash,issued_at_ms,expires_at_ms) VALUES(?,?,?,?)", id[:], hash[:], issued.UnixMilli(), issued.Add(ttl).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := tokens.Seal(bytes.NewReader(bytes.Repeat([]byte{7}, 32)), id, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO email_verification_outbox(user_id,recipient,token_nonce,token_ciphertext,created_at_ms,next_attempt_at_ms) VALUES(?,?,?,?,?,?)", id[:], recipient, nonce, ciphertext, issued.UnixMilli(), issued.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return id
}

func bytesOf(value byte) []byte {
	result := make([]byte, verificationTokenBytes)
	for index := range result {
		result[index] = value
	}
	return result
}

func assertCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
		t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
	}
}

func assertMissingUser(t *testing.T, db *sql.DB, id uuid.UUID) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM users WHERE id=?", id[:]).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired user count=%d err=%v", count, err)
	}
}

func assertExistingUser(t *testing.T, db *sql.DB, id uuid.UUID) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM users WHERE id=?", id[:]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("existing user count=%d err=%v", count, err)
	}
}
