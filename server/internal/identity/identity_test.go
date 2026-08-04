package identity

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/faulander/remember/server/internal/database"
	"github.com/google/uuid"
)

func TestCanonicalizeEmail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input, delivery, canonical string
	}{
		{" User.Name+tag@Example.COM ", "User.Name+tag@example.com", "user.name+tag@example.com"},
		{"person@bücher.example", "person@xn--bcher-kva.example", "person@xn--bcher-kva.example"},
	}
	for _, tt := range tests {
		got, err := CanonicalizeEmail(tt.input)
		if err != nil {
			t.Fatalf("CanonicalizeEmail(%q) error = %v", tt.input, err)
		}
		if got.Delivery != tt.delivery || got.Canonical != tt.canonical {
			t.Errorf("CanonicalizeEmail(%q) = %#v", tt.input, got)
		}
	}

	invalid := []string{
		"", "name", "a@@example.com", ".a@example.com", "a..b@example.com",
		"a.@example.com", "\"quoted\"@example.com", "ü@example.com",
		"a@[127.0.0.1]", "a@example.com.", "a@-example.com", "a@example..com",
		"a\n@example.com", string([]byte{0xff}) + "@example.com",
	}
	for _, input := range invalid {
		if _, err := CanonicalizeEmail(input); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("CanonicalizeEmail(%q) error = %v, want invalid", input, err)
		}
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	t.Parallel()

	params := testArgonParams()
	hasher, err := newPasswordHasher(params, bytes.NewReader(bytes.Repeat([]byte{7}, 256)))
	if err != nil {
		t.Fatal(err)
	}
	password := "correct horse battery staple"
	encoded, err := hasher.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, password) || !strings.HasPrefix(encoded, "$argon2id$v=19$") {
		t.Errorf("unexpected PHC string %q", encoded)
	}
	matches, needsRehash, err := hasher.Verify(password, encoded, PasswordPolicyVersion)
	if err != nil || !matches || needsRehash {
		t.Errorf("Verify() = %t, %t, %v", matches, needsRehash, err)
	}
	matches, _, err = hasher.Verify("incorrect password value", encoded, PasswordPolicyVersion)
	if err != nil || matches {
		t.Errorf("wrong password Verify() = %t, %v", matches, err)
	}
	_, needsRehash, err = hasher.Verify(password, encoded, PasswordPolicyVersion+1)
	if err != nil || !needsRehash {
		t.Errorf("old policy needsRehash = %t, error %v", needsRehash, err)
	}
}

func TestPasswordHasherRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()

	for _, params := range []Argon2Params{
		{Memory: 32, Iterations: 1, Parallelism: 1, SaltLength: 16, HashLength: 32},
		{Memory: maxArgonMemory + 1, Iterations: 1, Parallelism: 1, SaltLength: 16, HashLength: 32},
		{Memory: 64, Iterations: maxArgonIterations + 1, Parallelism: 1, SaltLength: 16, HashLength: 32},
		{Memory: 64, Iterations: 1, Parallelism: maxArgonParallelism + 1, SaltLength: 16, HashLength: 32},
	} {
		if _, err := newPasswordHasher(params, bytes.NewReader(make([]byte, 128))); err == nil {
			t.Errorf("unsafe params accepted: %#v", params)
		}
	}
	production := ProductionArgon2Params()
	production.Memory = 64
	if ProductionArgon2Params().Memory != productionMemory {
		t.Error("production parameters are externally mutable")
	}
}

func TestPasswordPolicyBoundariesAndPHCLimits(t *testing.T) {
	t.Parallel()

	hasher, err := newPasswordHasher(testArgonParams(), bytes.NewReader(bytes.Repeat([]byte{9}, 256)))
	if err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"short password", string([]byte{0xff}), strings.Repeat("a", 1025)} {
		if _, err := hasher.Hash(password); !errors.Is(err, ErrInvalidPassword) {
			t.Errorf("Hash() error = %v for invalid password length %d/%d", err, len(password), utf8.RuneCountInString(password))
		}
	}
	if _, err := hasher.Hash("fifteen-chars!!"); err != nil {
		t.Errorf("minimum password rejected: %v", err)
	}
	malicious := []string{
		"$argon2id$v=19$m=999999,t=1,p=1$MTIzNDU2Nzg$MTIzNDU2Nzg5MDEyMzQ1Ng",
		strings.Repeat("A", maxPHCLength+1),
		"$argon2id$v=19$m=64,t=1,p=1$MTIzNDU2\nNzg$MTIzNDU2Nzg5MDEyMzQ1Ng",
		"$argon2id$v=19$m=064,t=1,p=1$MTIzNDU2Nzg$MTIzNDU2Nzg5MDEyMzQ1Ng",
	}
	for _, encoded := range malicious {
		if _, _, err := hasher.Verify("fifteen-chars!!", encoded, 1); !errors.Is(err, ErrInvalidPasswordHash) {
			t.Errorf("Verify() malicious PHC error = %v", err)
		}
	}
}

func TestRegisterDuplicateAndVerifySingleUse(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)}
	service, db := testService(t, clock)
	ctx := context.Background()
	created, err := service.Register(ctx, "User@Example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.UserID.Version() != 7 || len(created.VerificationToken) != 43 {
		t.Fatalf("registration = %#v", created)
	}
	if uuidV7Milliseconds(created.UserID) != clock.now.UnixMilli() || clock.callCount() != 1 {
		t.Errorf("registration UUID/clock mismatch: id_ms=%d clock_ms=%d calls=%d", uuidV7Milliseconds(created.UserID), clock.now.UnixMilli(), clock.callCount())
	}
	var storedToken []byte
	var originalPasswordHash string
	if err := db.QueryRow("SELECT token_hash FROM email_verifications WHERE user_id = ?", created.UserID[:]).Scan(&storedToken); err != nil {
		t.Fatal(err)
	}
	if len(storedToken) != 32 || bytes.Contains(storedToken, []byte(created.VerificationToken)) {
		t.Error("verification token was not stored as a fixed hash")
	}
	if err := db.QueryRow("SELECT password_hash FROM users WHERE id = ?", created.UserID[:]).Scan(&originalPasswordHash); err != nil {
		t.Fatal(err)
	}

	duplicate, err := service.Register(ctx, "user@example.COM", "different secure password value")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Created || duplicate.UserID != uuid.Nil || duplicate.VerificationToken != "" {
		t.Errorf("duplicate leaked registration data: %#v", duplicate)
	}
	var userCount, tokenCount int
	var passwordHashAfter string
	if err := db.QueryRow("SELECT COUNT(*), MIN(password_hash) FROM users").Scan(&userCount, &passwordHashAfter); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM email_verifications").Scan(&tokenCount); err != nil {
		t.Fatal(err)
	}
	if userCount != 1 || tokenCount != 1 || passwordHashAfter != originalPasswordHash {
		t.Errorf("duplicate changed account: users=%d tokens=%d", userCount, tokenCount)
	}

	if err := service.VerifyEmail(ctx, created.VerificationToken); err != nil {
		t.Fatal(err)
	}
	var status string
	var verifiedAt int64
	if err := db.QueryRow("SELECT status, verified_at_ms FROM users WHERE id = ?", created.UserID[:]).Scan(&status, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if status != StatusActive || verifiedAt != clock.now.UnixMilli() {
		t.Errorf("verified account status=%q at=%d", status, verifiedAt)
	}
	if err := service.VerifyEmail(ctx, created.VerificationToken); !errors.Is(err, ErrInvalidVerificationToken) {
		t.Errorf("token reuse error = %v", err)
	}
}

func TestVerificationExpiryBoundaries(t *testing.T) {
	t.Parallel()

	issued := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: issued}
	service, _ := testService(t, clock)
	registration, err := service.Register(context.Background(), "person@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	clock.set(issued.Add(VerificationTTL - time.Millisecond))
	if err := service.VerifyEmail(context.Background(), registration.VerificationToken); err != nil {
		t.Errorf("token rejected before expiry: %v", err)
	}

	clock.set(issued)
	second, err := service.Register(context.Background(), "second@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	clock.set(issued.Add(VerificationTTL))
	if err := service.VerifyEmail(context.Background(), second.VerificationToken); !errors.Is(err, ErrInvalidVerificationToken) {
		t.Errorf("token accepted at exact expiry: %v", err)
	}
	clock.set(issued.Add(-time.Millisecond))
	if err := service.VerifyEmail(context.Background(), second.VerificationToken); !errors.Is(err, ErrInvalidVerificationToken) {
		t.Errorf("token accepted before issuance: %v", err)
	}
}

func TestMalformedVerificationReadsClockExactlyOnce(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Now().UTC()}
	service, _ := testService(t, clock)
	if err := service.VerifyEmail(context.Background(), "not-a-token"); !errors.Is(err, ErrInvalidVerificationToken) {
		t.Fatalf("malformed token error = %v", err)
	}
	if clock.callCount() != 1 {
		t.Errorf("clock calls = %d, want 1", clock.callCount())
	}
}

func TestRandomFailureLeavesNoPartialRegistration(t *testing.T) {
	t.Parallel()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "identity.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	hasher, err := newPasswordHasher(testArgonParams(), bytes.NewReader(bytes.Repeat([]byte{1}, 128)))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, hasher, &fakeClock{now: time.Now().UTC()}, failingReader{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(context.Background(), "person@example.com", "correct horse battery staple"); err == nil {
		t.Fatal("registration accepted failed randomness")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("random failure persisted %d users", count)
	}
}

func TestConcurrentDuplicateRegistrationCreatesOneAccount(t *testing.T) {
	t.Parallel()

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "identity.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	random := &lockedReader{reader: bytes.NewReader(deterministicBytes(64 * 1024))}
	hasher, err := newPasswordHasher(testArgonParams(), random)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, hasher, &fakeClock{now: time.Now().UTC()}, random)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan Registration, 2)
	errorsFound := make(chan error, 2)
	var wg sync.WaitGroup
	for _, email := range []string{"Race@Example.com", "race@example.COM"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := service.Register(context.Background(), email, "correct horse battery staple")
			results <- result
			errorsFound <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent registration error = %v", err)
		}
	}
	var created int
	for result := range results {
		if result.Created {
			created++
		}
	}
	if created != 1 {
		t.Errorf("created registrations = %d, want 1", created)
	}
}

func TestConcurrentVerificationConsumesTokenOnce(t *testing.T) {
	t.Parallel()

	clock := &fakeClock{now: time.Now().UTC()}
	service, _ := testService(t, clock)
	registration, err := service.Register(context.Background(), "race@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- service.VerifyEmail(context.Background(), registration.VerificationToken)
		}()
	}
	wg.Wait()
	close(results)
	var success, invalid int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrInvalidVerificationToken):
			invalid++
		default:
			t.Fatalf("unexpected verification result: %v", err)
		}
	}
	if success != 1 || invalid != 1 {
		t.Errorf("verification results success=%d invalid=%d", success, invalid)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

type lockedReader struct {
	mu     sync.Mutex
	reader *bytes.Reader
}

func (r *lockedReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reader.Read(buffer)
}

func uuidV7Milliseconds(id uuid.UUID) int64 {
	return int64(id[0])<<40 | int64(id[1])<<32 | int64(id[2])<<24 |
		int64(id[3])<<16 | int64(id[4])<<8 | int64(id[5])
}

func deterministicBytes(length int) []byte {
	result := make([]byte, length)
	for i := range result {
		result[i] = byte((i * 37) % 251)
	}
	return result
}

func testArgonParams() Argon2Params {
	return Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, HashLength: 32}
}

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	calls int
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.now
}

func (c *fakeClock) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *fakeClock) set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func testService(t *testing.T, clock Clock) (*Service, *sql.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "identity.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	random := bytes.NewReader(deterministicBytes(64 * 1024))
	hasher, err := newPasswordHasher(testArgonParams(), random)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(db, hasher, clock, random)
	if err != nil {
		t.Fatal(err)
	}
	return service, db
}
