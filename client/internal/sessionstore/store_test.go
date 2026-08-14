package sessionstore

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

type memoryKeyring struct {
	secret string
	err    error
}

func (m *memoryKeyring) Get(_, _ string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if m.secret == "" {
		return "", keyring.ErrNotFound
	}
	return m.secret, nil
}
func (m *memoryKeyring) Set(_, _, secret string) error {
	if m.err != nil {
		return m.err
	}
	m.secret = secret
	return nil
}
func (m *memoryKeyring) Delete(_, _ string) error {
	if m.err != nil {
		return m.err
	}
	if m.secret == "" {
		return keyring.ErrNotFound
	}
	m.secret = ""
	return nil
}

func TestKeyringStoreRoundTripAndDelete(t *testing.T) {
	t.Parallel()
	backend := &memoryKeyring{}
	store := &KeyringStore{backend: backend}
	want := Credential{
		ServerURL: "https://remember.example", UserID: "user", DeviceID: "device", SessionID: "session",
		RefreshToken: "refresh-secret", RefreshExpiresAt: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(backend.secret, "refresh-secret") || strings.Contains(backend.secret, "access_token") {
		t.Fatalf("unexpected credential envelope: %q", backend.secret)
	}
	got, err := store.Load()
	if err != nil || got != want {
		t.Fatalf("Load() = %#v, %v", got, err)
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-delete Load() error = %v", err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
}

func TestKeyringStoreRejectsCorruptAndUnavailableRecords(t *testing.T) {
	t.Parallel()
	for _, secret := range []string{
		`{"version":2,"credential":{}}`,
		`{"version":1,"credential":{"server_url":"https://remember.example"}}`,
		`{"version":1,"credential":{}} trailing`,
	} {
		store := &KeyringStore{backend: &memoryKeyring{secret: secret}}
		if _, err := store.Load(); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("corrupt record accepted: %q, %v", secret, err)
		}
	}
	store := &KeyringStore{backend: &memoryKeyring{err: errors.New("locked")}}
	if err := store.Save(Credential{}); err == nil || strings.Contains(err.Error(), "locked") {
		t.Fatalf("backend error leaked: %v", err)
	}
}
