package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

const (
	StatusPendingVerification = "pending_verification"
	StatusActive              = "active"
	StatusDeletionPending     = "deletion_pending"
	VerificationTTL           = 24 * time.Hour
	tokenBytes                = 32
)

var ErrInvalidVerificationToken = errors.New("invalid verification token")

var verificationDomain = []byte("remember:email-verification:v1\x00")

// Clock is called once per identity operation.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Service contains no HTTP, delivery, session, or sync behavior.
type Service struct {
	db       *sql.DB
	password *PasswordHasher
	clock    Clock
	random   io.Reader
}

func NewService(db *sql.DB, password *PasswordHasher, clock Clock, random io.Reader) (*Service, error) {
	if db == nil || password == nil || random == nil {
		return nil, errors.New("identity service dependency is nil")
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Service{db: db, password: password, clock: clock, random: random}, nil
}

// Registration is secret-bearing and may only be passed to a delivery port.
type Registration struct {
	Created           bool
	UserID            uuid.UUID
	VerificationToken string
}

// Register creates a pending account and one verification token. Existing
// addresses return an empty, non-error result and are never modified.
func (s *Service) Register(ctx context.Context, emailInput, passwordInput string) (Registration, error) {
	email, err := CanonicalizeEmail(emailInput)
	if err != nil {
		return Registration{}, err
	}
	passwordHash, err := s.password.Hash(passwordInput)
	if err != nil {
		return Registration{}, err
	}
	now := s.clock.Now().UTC()
	userID, err := newUUIDv7(now, s.random)
	if err != nil {
		return Registration{}, fmt.Errorf("generate user id: %w", err)
	}
	rawToken := make([]byte, tokenBytes)
	if _, err := io.ReadFull(s.random, rawToken); err != nil {
		return Registration{}, fmt.Errorf("generate verification token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(rawToken)
	tokenHash := hashVerificationToken(rawToken)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Registration{}, fmt.Errorf("begin registration: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO users(
			id, email_delivery, email_canonical, password_hash,
			password_policy, status, created_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID[:], email.Delivery, email.Canonical, passwordHash,
		PasswordPolicyVersion, StatusPendingVerification, now.UnixMilli(),
	)
	if err != nil {
		var exists int
		lookupErr := tx.QueryRowContext(ctx,
			"SELECT 1 FROM users WHERE email_canonical = ?", email.Canonical).Scan(&exists)
		if lookupErr == nil {
			return Registration{}, nil
		}
		return Registration{}, fmt.Errorf("insert pending user: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO email_verifications(user_id, token_hash, issued_at_ms, expires_at_ms)
		VALUES (?, ?, ?, ?)`,
		userID[:], tokenHash[:], now.UnixMilli(), now.Add(VerificationTTL).UnixMilli(),
	)
	if err != nil {
		return Registration{}, fmt.Errorf("insert verification token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Registration{}, fmt.Errorf("commit registration: %w", err)
	}
	return Registration{Created: true, UserID: userID, VerificationToken: token}, nil
}

// VerifyEmail consumes a token and activates its pending user atomically.
func (s *Service) VerifyEmail(ctx context.Context, encodedToken string) error {
	now := s.clock.Now().UTC().UnixMilli()
	rawToken, err := base64.RawURLEncoding.Strict().DecodeString(encodedToken)
	if err != nil || len(rawToken) != tokenBytes || len(encodedToken) != 43 {
		return ErrInvalidVerificationToken
	}
	tokenHash := hashVerificationToken(rawToken)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin email verification: %w", err)
	}
	defer tx.Rollback()
	var userID []byte
	var status string
	var issuedAt, expiresAt int64
	err = tx.QueryRowContext(ctx, `
		SELECT u.id, u.status, v.issued_at_ms, v.expires_at_ms
		FROM email_verifications v
		JOIN users u ON u.id = v.user_id
		WHERE v.token_hash = ?`, tokenHash[:],
	).Scan(&userID, &status, &issuedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidVerificationToken
	}
	if err != nil {
		return fmt.Errorf("read email verification: %w", err)
	}
	if len(userID) != 16 || status != StatusPendingVerification || now < issuedAt || now >= expiresAt {
		return ErrInvalidVerificationToken
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE users SET status = ?, verified_at_ms = ?
		WHERE id = ? AND status = ?`,
		StatusActive, now, userID, StatusPendingVerification,
	)
	if err != nil {
		return fmt.Errorf("activate user: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil || updated != 1 {
		return ErrInvalidVerificationToken
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM email_verifications WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("consume email verification: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit email verification: %w", err)
	}
	return nil
}

func newUUIDv7(now time.Time, random io.Reader) (uuid.UUID, error) {
	var id uuid.UUID
	if _, err := io.ReadFull(random, id[:]); err != nil {
		return uuid.Nil, err
	}
	milliseconds := uint64(now.UTC().UnixMilli())
	id[0] = byte(milliseconds >> 40)
	id[1] = byte(milliseconds >> 32)
	id[2] = byte(milliseconds >> 24)
	id[3] = byte(milliseconds >> 16)
	id[4] = byte(milliseconds >> 8)
	id[5] = byte(milliseconds)
	id[6] = (id[6] & 0x0f) | 0x70
	id[8] = (id[8] & 0x3f) | 0x80
	return id, nil
}

func hashVerificationToken(raw []byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write(verificationDomain)
	_, _ = hash.Write(raw)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

// NewProductionService constructs the internal core with production crypto.
func NewProductionService(db *sql.DB) (*Service, error) {
	hasher, err := NewPasswordHasher(rand.Reader)
	if err != nil {
		return nil, err
	}
	return NewService(db, hasher, systemClock{}, rand.Reader)
}
