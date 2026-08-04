// Package session implements the internal device and session core. It exposes
// no HTTP transport and never treats caller-supplied tenant identifiers as
// authorization.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 30 * 24 * time.Hour
	tokenBytes = 32
)

var (
	ErrUnauthenticated = errors.New("invalid or expired session credential")
	ErrInvalidInput    = errors.New("invalid session input")
	ErrNotFound        = errors.New("session resource not found")
)

var (
	accessDomain  = []byte("remember:access-token:v1\x00")
	refreshDomain = []byte("remember:refresh-token:v1\x00")
)

type Clock interface{ Now() time.Time }
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type Authenticator interface {
	AuthenticateCredential(context.Context, string, string) (uuid.UUID, error)
}

type Service struct {
	db            *sql.DB
	authenticator Authenticator
	clock         Clock
	random        io.Reader
	randomMu      sync.Mutex
	refreshMu     sync.Mutex
}

type Principal struct {
	UserID, DeviceID, SessionID uuid.UUID
}

type Tokens struct {
	AccessToken, RefreshToken         string
	AccessExpiresAt, RefreshExpiresAt time.Time
}

type LoginResult struct {
	Principal Principal
	Tokens    Tokens
}

type SessionInfo struct {
	SessionID, DeviceID  uuid.UUID
	DeviceName, Status   string
	CreatedAt, ExpiresAt time.Time
	RevokedAt            *time.Time
	Current              bool
}

func NewService(db *sql.DB, authenticator Authenticator, clock Clock, random io.Reader) (*Service, error) {
	if db == nil || authenticator == nil || random == nil {
		return nil, errors.New("session service dependency is nil")
	}
	if clock == nil {
		clock = systemClock{}
	}
	return &Service{db: db, authenticator: authenticator, clock: clock, random: random}, nil
}

func NewProductionService(db *sql.DB, authenticator Authenticator) (*Service, error) {
	return NewService(db, authenticator, systemClock{}, rand.Reader)
}

func (s *Service) Login(ctx context.Context, email, password, deviceName string) (LoginResult, error) {
	name, err := validateDeviceName(deviceName)
	if err != nil {
		return LoginResult{}, err
	}
	userID, err := s.authenticator.AuthenticateCredential(ctx, email, password)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.clock.Now().UTC()
	s.randomMu.Lock()
	deviceID, err := newUUIDv7(now, s.random)
	if err != nil {
		s.randomMu.Unlock()
		return LoginResult{}, fmt.Errorf("generate device id: %w", err)
	}
	sessionID, err := newUUIDv7(now, s.random)
	if err != nil {
		s.randomMu.Unlock()
		return LoginResult{}, fmt.Errorf("generate session id: %w", err)
	}
	issued, err := issueTokenPair(s.random, now, now.Add(RefreshTTL))
	s.randomMu.Unlock()
	if err != nil {
		return LoginResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LoginResult{}, fmt.Errorf("begin login: %w", err)
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id=? AND status='active'", userID[:]).Scan(&active); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LoginResult{}, ErrUnauthenticated
		}
		return LoginResult{}, fmt.Errorf("validate login account: %w", err)
	}
	nowMS, absoluteMS := now.UnixMilli(), now.Add(RefreshTTL).UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO devices(user_id,id,display_name,status,created_at_ms,updated_at_ms)
		VALUES(?,?,?,'active',?,?)`, userID[:], deviceID[:], name, nowMS, nowMS); err != nil {
		return LoginResult{}, fmt.Errorf("create login device: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(user_id,id,device_id,status,created_at_ms,expires_at_ms)
		VALUES(?,?,?,'active',?,?)`, userID[:], sessionID[:], deviceID[:], nowMS, absoluteMS); err != nil {
		return LoginResult{}, fmt.Errorf("create login session: %w", err)
	}
	if err := insertAccess(ctx, tx, userID, deviceID, sessionID, issued, now); err != nil {
		return LoginResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO refresh_tokens(
		token_hash,user_id,session_id,device_id,issued_at_ms,expires_at_ms)
		VALUES(?,?,?,?,?,?)`, issued.refreshHash[:], userID[:], sessionID[:], deviceID[:], nowMS, absoluteMS); err != nil {
		return LoginResult{}, fmt.Errorf("store refresh token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LoginResult{}, fmt.Errorf("commit login: %w", err)
	}
	return LoginResult{Principal: Principal{userID, deviceID, sessionID}, Tokens: issued.tokens}, nil
}

func (s *Service) AuthenticateAccess(ctx context.Context, encoded string) (Principal, error) {
	hash, err := parseAndHash(encoded, accessDomain)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	now := s.clock.Now().UTC().UnixMilli()
	var userBytes, deviceBytes, sessionBytes []byte
	var issuedAt, tokenExpires, sessionCreated, sessionExpires int64
	var tokenRevoked sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT a.user_id,a.device_id,a.session_id,a.issued_at_ms,a.expires_at_ms,
		a.revoked_at_ms,s.created_at_ms,s.expires_at_ms
		FROM access_tokens a
		JOIN sessions s ON s.user_id=a.user_id AND s.id=a.session_id AND s.device_id=a.device_id
		JOIN devices d ON d.user_id=a.user_id AND d.id=a.device_id
		JOIN users u ON u.id=a.user_id
		WHERE a.token_hash=? AND s.status='active' AND d.status='active' AND u.status='active'`, hash[:]).
		Scan(&userBytes, &deviceBytes, &sessionBytes, &issuedAt, &tokenExpires, &tokenRevoked, &sessionCreated, &sessionExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("read access credential: %w", err)
	}
	if tokenRevoked.Valid || now < issuedAt || now < sessionCreated || now >= tokenExpires || now >= sessionExpires {
		return Principal{}, ErrUnauthenticated
	}
	principal, err := decodePrincipal(userBytes, deviceBytes, sessionBytes)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	return principal, nil
}

func (s *Service) Refresh(ctx context.Context, encoded string) (Tokens, error) {
	hash, err := parseAndHash(encoded, refreshDomain)
	if err != nil {
		return Tokens{}, ErrUnauthenticated
	}
	now := s.clock.Now().UTC()
	s.randomMu.Lock()
	issued, err := issueTokenPair(s.random, now, time.Time{})
	s.randomMu.Unlock()
	if err != nil {
		return Tokens{}, err
	}
	// The deployment contract permits one server process. Serializing refreshes
	// here makes the conditional rotation/replay transition deterministic while
	// SQLite remains the durable source of truth.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Tokens{}, fmt.Errorf("begin refresh: %w", err)
	}
	defer tx.Rollback()
	nowMS := now.UnixMilli()
	// Acquire SQLite's write lock before reading the predecessor. The no-op
	// write preserves all constraints while serializing independent Service
	// instances on durable state rather than relying on the process mutex.
	lockedResult, err := tx.ExecContext(ctx, `UPDATE refresh_tokens SET token_hash=token_hash WHERE token_hash=?`, hash[:])
	if err != nil {
		return Tokens{}, fmt.Errorf("lock refresh credential: %w", err)
	}
	locked, err := lockedResult.RowsAffected()
	if err != nil {
		return Tokens{}, fmt.Errorf("count refresh lock: %w", err)
	}
	var userBytes, sessionBytes, deviceBytes []byte
	var issuedAt, expiresAt, sessionCreated, sessionExpires int64
	var consumed sql.NullInt64
	var userStatus, sessionStatus, deviceStatus string
	err = tx.QueryRowContext(ctx, `SELECT r.user_id,r.session_id,r.device_id,r.issued_at_ms,r.expires_at_ms,r.consumed_at_ms,
		u.status,s.status,d.status,s.created_at_ms,s.expires_at_ms
		FROM refresh_tokens r
		JOIN users u ON u.id=r.user_id
		JOIN sessions s ON s.user_id=r.user_id AND s.id=r.session_id AND s.device_id=r.device_id
		JOIN devices d ON d.user_id=r.user_id AND d.id=r.device_id
		WHERE r.token_hash=?`, hash[:]).Scan(&userBytes, &sessionBytes, &deviceBytes, &issuedAt, &expiresAt, &consumed,
		&userStatus, &sessionStatus, &deviceStatus, &sessionCreated, &sessionExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return Tokens{}, ErrUnauthenticated
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("read refresh credential: %w", err)
	}
	principal, err := decodePrincipal(userBytes, deviceBytes, sessionBytes)
	if err != nil {
		return Tokens{}, ErrUnauthenticated
	}
	if locked != 1 {
		return Tokens{}, ErrUnauthenticated
	}
	if consumed.Valid {
		if err := revokeSessionTx(ctx, tx, principal.UserID, principal.SessionID, nowMS); err != nil {
			return Tokens{}, err
		}
		if err := tx.Commit(); err != nil {
			return Tokens{}, fmt.Errorf("commit refresh replay revocation: %w", err)
		}
		return Tokens{}, ErrUnauthenticated
	}
	if nowMS < issuedAt || nowMS < sessionCreated || userStatus != "active" || sessionStatus != "active" ||
		deviceStatus != "active" || nowMS >= expiresAt || nowMS >= sessionExpires {
		return Tokens{}, ErrUnauthenticated
	}
	absolute := time.UnixMilli(sessionExpires).UTC()
	issued.tokens.RefreshExpiresAt = absolute
	accessExpiry := now.Add(AccessTTL)
	if accessExpiry.After(absolute) {
		accessExpiry = absolute
	}
	issued.tokens.AccessExpiresAt = accessExpiry
	if _, err := tx.ExecContext(ctx, `INSERT INTO refresh_tokens(
		token_hash,user_id,session_id,device_id,issued_at_ms,expires_at_ms)
		VALUES(?,?,?,?,?,?)`, issued.refreshHash[:], principal.UserID[:], principal.SessionID[:], principal.DeviceID[:], nowMS, sessionExpires); err != nil {
		return Tokens{}, fmt.Errorf("store rotated refresh token: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE refresh_tokens SET consumed_at_ms=?,replaced_by_hash=?
		WHERE token_hash=? AND consumed_at_ms IS NULL AND issued_at_ms<=? AND ?<expires_at_ms`,
		nowMS, issued.refreshHash[:], hash[:], nowMS, nowMS)
	if err != nil {
		return Tokens{}, fmt.Errorf("link rotated refresh token: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return Tokens{}, ErrUnauthenticated
	}
	if err := insertAccess(ctx, tx, principal.UserID, principal.DeviceID, principal.SessionID, issued, now); err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tokens{}, fmt.Errorf("commit refresh: %w", err)
	}
	return issued.tokens, nil
}

func (s *Service) ListForUser(ctx context.Context, accessToken string) ([]SessionInfo, error) {
	principal, err := s.AuthenticateAccess(ctx, accessToken)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.device_id,d.display_name,s.status,s.created_at_ms,s.expires_at_ms,s.revoked_at_ms
		FROM sessions s JOIN devices d ON d.user_id=s.user_id AND d.id=s.device_id
		WHERE s.user_id=? ORDER BY s.created_at_ms DESC,s.id`, principal.UserID[:])
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var result []SessionInfo
	for rows.Next() {
		var sessionBytes, deviceBytes []byte
		var item SessionInfo
		var created, expires int64
		var revoked sql.NullInt64
		if err := rows.Scan(&sessionBytes, &deviceBytes, &item.DeviceName, &item.Status, &created, &expires, &revoked); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		item.SessionID, err = uuid.FromBytes(sessionBytes)
		if err != nil {
			return nil, ErrUnauthenticated
		}
		item.DeviceID, err = uuid.FromBytes(deviceBytes)
		if err != nil {
			return nil, ErrUnauthenticated
		}
		item.CreatedAt, item.ExpiresAt = time.UnixMilli(created).UTC(), time.UnixMilli(expires).UTC()
		if revoked.Valid {
			value := time.UnixMilli(revoked.Int64).UTC()
			item.RevokedAt = &value
		}
		item.Current = item.SessionID == principal.SessionID
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return result, nil
}

func (s *Service) RenameDevice(ctx context.Context, accessToken string, deviceID uuid.UUID, displayName string) error {
	name, err := validateDeviceName(displayName)
	if err != nil || !validV7(deviceID) {
		return ErrInvalidInput
	}
	principal, err := s.AuthenticateAccess(ctx, accessToken)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC().UnixMilli()
	result, err := s.db.ExecContext(ctx, `UPDATE devices SET display_name=?,
		updated_at_ms=CASE WHEN ?<created_at_ms THEN created_at_ms ELSE ? END
		WHERE user_id=? AND id=? AND status='active'`, name, now, now, principal.UserID[:], deviceID[:])
	if err != nil {
		return fmt.Errorf("rename device: %w", err)
	}
	return requireOne(result)
}

func (s *Service) RevokeSession(ctx context.Context, accessToken string, sessionID uuid.UUID) error {
	if !validV7(sessionID) {
		return ErrInvalidInput
	}
	principal, err := s.AuthenticateAccess(ctx, accessToken)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session revocation: %w", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE user_id=? AND id=?", principal.UserID[:], sessionID[:]).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := revokeSessionTx(ctx, tx, principal.UserID, sessionID, s.clock.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session revocation: %w", err)
	}
	return nil
}

func (s *Service) RevokeDevice(ctx context.Context, accessToken string, deviceID uuid.UUID) error {
	if !validV7(deviceID) {
		return ErrInvalidInput
	}
	principal, err := s.AuthenticateAccess(ctx, accessToken)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin device revocation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE devices SET status='revoked',
		revoked_at_ms=COALESCE(revoked_at_ms,CASE WHEN ?<created_at_ms THEN created_at_ms ELSE ? END),
		updated_at_ms=CASE WHEN ?<created_at_ms THEN created_at_ms ELSE ? END
		WHERE user_id=? AND id=?`, now, now, now, now, principal.UserID[:], deviceID[:])
	if err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	if err := requireOne(result); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status='revoked',
		revoked_at_ms=COALESCE(revoked_at_ms,CASE WHEN ?<created_at_ms THEN created_at_ms ELSE ? END)
		WHERE user_id=? AND device_id=?`, now, now, principal.UserID[:], deviceID[:]); err != nil {
		return fmt.Errorf("revoke device sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE access_tokens SET
		revoked_at_ms=COALESCE(revoked_at_ms,CASE WHEN ?<issued_at_ms THEN issued_at_ms ELSE ? END)
		WHERE user_id=? AND device_id=?`, now, now, principal.UserID[:], deviceID[:]); err != nil {
		return fmt.Errorf("revoke device access: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit device revocation: %w", err)
	}
	return nil
}

type issuedTokens struct {
	tokens                  Tokens
	accessHash, refreshHash [sha256.Size]byte
}

func issueTokenPair(random io.Reader, now, absolute time.Time) (issuedTokens, error) {
	access := make([]byte, tokenBytes)
	refresh := make([]byte, tokenBytes)
	if _, err := io.ReadFull(random, access); err != nil {
		return issuedTokens{}, fmt.Errorf("generate access token: %w", err)
	}
	if _, err := io.ReadFull(random, refresh); err != nil {
		return issuedTokens{}, fmt.Errorf("generate refresh token: %w", err)
	}
	if absolute.IsZero() {
		absolute = now.Add(RefreshTTL)
	}
	accessExpiry := now.Add(AccessTTL)
	if accessExpiry.After(absolute) {
		accessExpiry = absolute
	}
	return issuedTokens{
		tokens: Tokens{
			AccessToken: base64.RawURLEncoding.EncodeToString(access), RefreshToken: base64.RawURLEncoding.EncodeToString(refresh),
			AccessExpiresAt: accessExpiry, RefreshExpiresAt: absolute,
		},
		accessHash: hashToken(accessDomain, access), refreshHash: hashToken(refreshDomain, refresh),
	}, nil
}

func insertAccess(ctx context.Context, tx *sql.Tx, userID, deviceID, sessionID uuid.UUID, issued issuedTokens, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO access_tokens(
		token_hash,user_id,session_id,device_id,issued_at_ms,expires_at_ms)
		VALUES(?,?,?,?,?,?)`, issued.accessHash[:], userID[:], sessionID[:], deviceID[:], now.UnixMilli(), issued.tokens.AccessExpiresAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("store access token: %w", err)
	}
	return nil
}

func revokeSessionTx(ctx context.Context, tx *sql.Tx, userID, sessionID uuid.UUID, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status='revoked',
		revoked_at_ms=COALESCE(revoked_at_ms,CASE WHEN ?<created_at_ms THEN created_at_ms ELSE ? END)
		WHERE user_id=? AND id=?`, now, now, userID[:], sessionID[:]); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE access_tokens SET
		revoked_at_ms=COALESCE(revoked_at_ms,CASE WHEN ?<issued_at_ms THEN issued_at_ms ELSE ? END)
		WHERE user_id=? AND session_id=?`, now, now, userID[:], sessionID[:]); err != nil {
		return fmt.Errorf("revoke session access: %w", err)
	}
	return nil
}

func parseAndHash(encoded string, domain []byte) ([sha256.Size]byte, error) {
	if len(encoded) != 43 {
		return [sha256.Size]byte{}, ErrUnauthenticated
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != tokenBytes || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return [sha256.Size]byte{}, ErrUnauthenticated
	}
	return hashToken(domain, raw), nil
}

func hashToken(domain, raw []byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write(domain)
	_, _ = hash.Write(raw)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func decodePrincipal(user, device, session []byte) (Principal, error) {
	userID, err := uuid.FromBytes(user)
	if err != nil {
		return Principal{}, err
	}
	deviceID, err := uuid.FromBytes(device)
	if err != nil {
		return Principal{}, err
	}
	sessionID, err := uuid.FromBytes(session)
	if err != nil {
		return Principal{}, err
	}
	return Principal{userID, deviceID, sessionID}, nil
}

func validateDeviceName(input string) (string, error) {
	name := strings.TrimSpace(input)
	if name == "" || !utf8.ValidString(name) || len(name) > 256 || utf8.RuneCountInString(name) > 100 {
		return "", ErrInvalidInput
	}
	return name, nil
}

func newUUIDv7(now time.Time, random io.Reader) (uuid.UUID, error) {
	var id uuid.UUID
	if _, err := io.ReadFull(random, id[:]); err != nil {
		return uuid.Nil, err
	}
	milliseconds := uint64(now.UTC().UnixMilli())
	id[0], id[1], id[2] = byte(milliseconds>>40), byte(milliseconds>>32), byte(milliseconds>>24)
	id[3], id[4], id[5] = byte(milliseconds>>16), byte(milliseconds>>8), byte(milliseconds)
	id[6] = (id[6] & 0x0f) | 0x70
	id[8] = (id[8] & 0x3f) | 0x80
	return id, nil
}

func validV7(id uuid.UUID) bool {
	return id != uuid.Nil && id.Version() == 7 && id.Variant() == uuid.RFC4122
}

func requireOne(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}
