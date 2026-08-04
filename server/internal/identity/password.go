package identity

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const PasswordPolicyVersion = 1

var (
	ErrInvalidPassword     = errors.New("password does not satisfy policy")
	ErrInvalidPasswordHash = errors.New("invalid password hash")
)

// Argon2Params is the versioned server password policy.
type Argon2Params struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	HashLength  uint32
}

const (
	productionMemory      uint32 = 64 * 1024
	productionIterations  uint32 = 3
	productionParallelism uint8  = 1
	maxArgonMemory        uint32 = 256 * 1024
	maxArgonIterations    uint32 = 10
	maxArgonParallelism   uint8  = 4
	maxPHCLength                 = 512
)

// ProductionArgon2Params returns a copy of immutable Identity Policy v1.
func ProductionArgon2Params() Argon2Params {
	return Argon2Params{
		Memory: productionMemory, Iterations: productionIterations, Parallelism: productionParallelism,
		SaltLength: 16, HashLength: 32,
	}
}

// PasswordHasher creates and verifies bounded PHC strings.
type PasswordHasher struct {
	params Argon2Params
	random io.Reader
}

// NewPasswordHasher creates the immutable production-policy hasher.
func NewPasswordHasher(random io.Reader) (*PasswordHasher, error) {
	return newPasswordHasher(ProductionArgon2Params(), random)
}

func newPasswordHasher(params Argon2Params, random io.Reader) (*PasswordHasher, error) {
	if random == nil || params.Memory < 64 || params.Memory > maxArgonMemory ||
		params.Iterations == 0 || params.Iterations > maxArgonIterations ||
		params.Parallelism == 0 || params.Parallelism > maxArgonParallelism ||
		params.Memory < 8*uint32(params.Parallelism) ||
		params.SaltLength < 8 || params.SaltLength > 64 || params.HashLength < 16 || params.HashLength > 64 {
		return nil, errors.New("invalid Argon2 configuration")
	}
	return &PasswordHasher{params: params, random: random}, nil
}

func (h *PasswordHasher) Hash(password string) (string, error) {
	if err := validateNewPassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, h.params.SaltLength)
	if _, err := io.ReadFull(h.random, salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, h.params.Iterations, h.params.Memory, h.params.Parallelism, h.params.HashLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Iterations, h.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// Verify returns whether password matches and whether a successful login should
// rehash it with the current policy.
func (h *PasswordHasher) Verify(password, encoded string, storedPolicy int) (bool, bool, error) {
	if !utf8.ValidString(password) || len(password) > 1024 {
		return false, false, ErrInvalidPassword
	}
	params, salt, expected, err := parsePHC(encoded)
	if err != nil {
		return false, false, err
	}
	actual := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, uint32(len(expected)))
	matches := subtle.ConstantTimeCompare(actual, expected) == 1
	needsRehash := matches && (storedPolicy != PasswordPolicyVersion || params != h.params)
	return matches, needsRehash, nil
}

func validateNewPassword(password string) error {
	if !utf8.ValidString(password) || len(password) > 1024 || utf8.RuneCountInString(password) < 15 {
		return ErrInvalidPassword
	}
	return nil
}

func parsePHC(encoded string) (Argon2Params, []byte, []byte, error) {
	if len(encoded) == 0 || len(encoded) > maxPHCLength || strings.ContainsAny(encoded, "\r\n") {
		return Argon2Params{}, nil, nil, ErrInvalidPasswordHash
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" ||
		len(parts[3]) > 64 || len(parts[4]) > 86 || len(parts[5]) > 86 {
		return Argon2Params{}, nil, nil, ErrInvalidPasswordHash
	}
	var params Argon2Params
	values := strings.Split(parts[3], ",")
	if len(values) != 3 {
		return Argon2Params{}, nil, nil, ErrInvalidPasswordHash
	}
	memory, errM := parseParam(values[0], "m=")
	iterations, errT := parseParam(values[1], "t=")
	parallelism, errP := parseParam(values[2], "p=")
	if errM != nil || errT != nil || errP != nil || memory < 64 || memory > uint64(maxArgonMemory) ||
		iterations == 0 || iterations > uint64(maxArgonIterations) ||
		parallelism == 0 || parallelism > uint64(maxArgonParallelism) || memory < 8*parallelism {
		return Argon2Params{}, nil, nil, ErrInvalidPasswordHash
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 || base64.RawStdEncoding.EncodeToString(salt) != parts[4] {
		return Argon2Params{}, nil, nil, ErrInvalidPasswordHash
	}
	hash, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(hash) < 16 || len(hash) > 64 || base64.RawStdEncoding.EncodeToString(hash) != parts[5] {
		return Argon2Params{}, nil, nil, ErrInvalidPasswordHash
	}
	params = Argon2Params{
		Memory: uint32(memory), Iterations: uint32(iterations), Parallelism: uint8(parallelism),
		SaltLength: uint32(len(salt)), HashLength: uint32(len(hash)),
	}
	return params, salt, hash, nil
}

func parseParam(value, prefix string) (uint64, error) {
	if !strings.HasPrefix(value, prefix) {
		return 0, ErrInvalidPasswordHash
	}
	raw := strings.TrimPrefix(value, prefix)
	parsed, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || strconv.FormatUint(parsed, 10) != raw {
		return 0, ErrInvalidPasswordHash
	}
	return parsed, nil
}
