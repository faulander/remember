package emaildelivery

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/faulander/remember/server/internal/identity"
	"github.com/faulander/remember/server/internal/verificationtoken"
	"github.com/google/uuid"
)

const dispatchInterval = 15 * time.Second

type Clock interface{ Now() time.Time }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Dispatcher drains the transactionally-created verification outbox with
// at-least-once delivery. A duplicate message carries the same one-time token.
type Dispatcher struct {
	db     *sql.DB
	sender Sender
	clock  Clock
	tokens *verificationtoken.Codec
}

func NewDispatcher(db *sql.DB, sender Sender, clock Clock, tokens *verificationtoken.Codec) (*Dispatcher, error) {
	if db == nil || sender == nil || tokens == nil {
		return nil, errors.New("email dispatcher dependency is nil")
	}
	if clock == nil {
		clock = wallClock{}
	}
	return &Dispatcher{db: db, sender: sender, clock: clock, tokens: tokens}, nil
}

// DispatchOne attempts the oldest due message. attempted is false when the
// queue has no currently deliverable message.
func (d *Dispatcher) DispatchOne(ctx context.Context) (attempted bool, err error) {
	now := d.clock.Now().UTC()
	if _, err := d.db.ExecContext(ctx, `DELETE FROM users
		WHERE status=? AND id IN (
			SELECT user_id FROM email_verifications WHERE expires_at_ms<=?
		)`, identity.StatusPendingVerification, now.UnixMilli()); err != nil {
		return false, fmt.Errorf("expire pending registrations: %w", err)
	}
	var userIDBytes, tokenNonce, tokenCiphertext []byte
	var recipient string
	var attempts int64
	err = d.db.QueryRowContext(ctx, `SELECT o.user_id,o.recipient,o.token_nonce,o.token_ciphertext,o.attempt_count
		FROM email_verification_outbox o
		JOIN email_verifications v ON v.user_id=o.user_id
		WHERE o.next_attempt_at_ms<=? AND v.expires_at_ms>?
		ORDER BY o.next_attempt_at_ms,o.created_at_ms LIMIT 1`, now.UnixMilli(), now.UnixMilli()).Scan(&userIDBytes, &recipient, &tokenNonce, &tokenCiphertext, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	userID, parseErr := uuid.FromBytes(userIDBytes)
	if err != nil || parseErr != nil {
		if err == nil {
			err = errors.New("invalid queued verification identity")
		}
		return false, fmt.Errorf("read verification outbox: %w", err)
	}
	rawToken, err := d.tokens.Open(userID, tokenNonce, tokenCiphertext)
	if err != nil || len(rawToken) != verificationTokenBytes {
		return false, errors.New("read verification outbox: invalid sealed verification")
	}
	token := base64.RawURLEncoding.EncodeToString(rawToken)
	if err := d.sender.SendVerification(ctx, recipient, token); err != nil {
		nextAttempt := now.Add(retryDelay(attempts + 1)).UnixMilli()
		if _, updateErr := d.db.ExecContext(ctx, `UPDATE email_verification_outbox
			SET attempt_count=attempt_count+1,next_attempt_at_ms=?
			WHERE user_id=? AND token_nonce=? AND token_ciphertext=?`, nextAttempt, userIDBytes, tokenNonce, tokenCiphertext); updateErr != nil {
			return true, fmt.Errorf("reschedule verification delivery: %w", updateErr)
		}
		return true, errors.New("verification delivery failed")
	}
	result, err := d.db.ExecContext(ctx, "DELETE FROM email_verification_outbox WHERE user_id=? AND token_nonce=? AND token_ciphertext=?", userIDBytes, tokenNonce, tokenCiphertext)
	if err != nil {
		return true, fmt.Errorf("complete verification delivery: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return true, errors.New("verification delivery completion mismatch")
	}
	return true, nil
}

func (d *Dispatcher) Run(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			for {
				attempted, err := d.DispatchOne(ctx)
				if err != nil && ctx.Err() == nil {
					logger.Warn("email_verification_delivery_failed", "event_code", "EMAIL_VERIFICATION_DELIVERY_FAILED", "error", err)
				}
				if !attempted || err != nil {
					break
				}
			}
			timer.Reset(dispatchInterval)
		}
	}
}

func retryDelay(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return 15 * time.Second * time.Duration(1<<(attempt-1))
}
