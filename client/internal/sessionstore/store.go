// Package sessionstore persists the one active desktop session exclusively in
// the operating system credential store.
package sessionstore

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	service            = "Remember"
	account            = "desktop-session-v1"
	version            = 1
	maxCredentialBytes = 16 * 1024
)

var ErrNotFound = errors.New("stored session not found")

type Credential struct {
	ServerURL        string    `json:"server_url"`
	UserID           string    `json:"user_id"`
	DeviceID         string    `json:"device_id"`
	SessionID        string    `json:"session_id"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

type Store interface {
	Load() (Credential, error)
	Save(Credential) error
	Delete() error
}

type keyringBackend interface {
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}

type systemKeyring struct{}

func (systemKeyring) Get(service, account string) (string, error) {
	return keyring.Get(service, account)
}
func (systemKeyring) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}
func (systemKeyring) Delete(service, account string) error { return keyring.Delete(service, account) }

type KeyringStore struct{ backend keyringBackend }

func NewKeyringStore() *KeyringStore { return &KeyringStore{backend: systemKeyring{}} }

func (s *KeyringStore) Load() (Credential, error) {
	if s == nil || s.backend == nil {
		return Credential{}, errors.New("secure credential store unavailable")
	}
	secret, err := s.backend.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, errors.New("secure credential store unavailable")
	}
	if len(secret) == 0 || len(secret) > maxCredentialBytes {
		return Credential{}, errors.New("stored session is invalid")
	}
	var envelope struct {
		Version    int        `json:"version"`
		Credential Credential `json:"credential"`
	}
	decoder := json.NewDecoder(strings.NewReader(secret))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || envelope.Version != version || envelope.Credential.ServerURL == "" || envelope.Credential.UserID == "" || envelope.Credential.DeviceID == "" || envelope.Credential.SessionID == "" || envelope.Credential.RefreshToken == "" || envelope.Credential.RefreshExpiresAt.IsZero() {
		return Credential{}, errors.New("stored session is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Credential{}, errors.New("stored session is invalid")
	}
	return envelope.Credential, nil
}

func (s *KeyringStore) Save(credential Credential) error {
	if s == nil || s.backend == nil {
		return errors.New("secure credential store unavailable")
	}
	if credential.ServerURL == "" || credential.UserID == "" || credential.DeviceID == "" || credential.SessionID == "" || credential.RefreshToken == "" || credential.RefreshExpiresAt.IsZero() {
		return errors.New("invalid session credential")
	}
	secret, err := json.Marshal(struct {
		Version    int        `json:"version"`
		Credential Credential `json:"credential"`
	}{Version: version, Credential: credential})
	if err != nil || len(secret) > maxCredentialBytes {
		return errors.New("encode session credential")
	}
	if err := s.backend.Set(service, account, string(secret)); err != nil {
		return errors.New("secure credential store unavailable")
	}
	return nil
}

func (s *KeyringStore) Delete() error {
	if s == nil || s.backend == nil {
		return errors.New("secure credential store unavailable")
	}
	if err := s.backend.Delete(service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return errors.New("secure credential store unavailable")
	}
	return nil
}
