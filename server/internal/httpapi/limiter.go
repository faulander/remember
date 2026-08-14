package httpapi

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
	"time"

	"github.com/faulander/remember/server/internal/identity"
)

const (
	loginKeyLimit           = 10
	loginGlobalLimit        = 100
	loginWindow             = 15 * time.Minute
	refreshKeyLimit         = 30
	refreshGlobalLimit      = 300
	refreshWindow           = time.Minute
	maxLimiterKeys          = 4096
	maxConcurrentLogin      = 4
	registrationKeyLimit    = 5
	registrationGlobalLimit = 100
	registrationWindow      = time.Hour
	verificationKeyLimit    = 10
	verificationGlobalLimit = 300
	verificationWindow      = time.Minute
)

var (
	loginKeyDomain        = []byte("remember:http-login-limit:v1\x00")
	refreshKeyDomain      = []byte("remember:http-refresh-limit:v1\x00")
	registrationKeyDomain = []byte("remember:http-registration-limit:v1\x00")
	verificationKeyDomain = []byte("remember:http-verification-limit:v1\x00")
)

type Clock interface{ Now() time.Time }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type limitEntry struct {
	started time.Time
	count   int
}

type fixedWindowLimiter struct {
	mu      sync.Mutex
	clock   Clock
	limit   int
	window  time.Duration
	maxKeys int
	entries map[[sha256.Size]byte]limitEntry
}

func newFixedWindowLimiter(clock Clock, limit int, window time.Duration, maxKeys int) *fixedWindowLimiter {
	return &fixedWindowLimiter{
		clock: clock, limit: limit, window: window, maxKeys: maxKeys,
		entries: make(map[[sha256.Size]byte]limitEntry),
	}
}

func (l *fixedWindowLimiter) allow(key [sha256.Size]byte) (bool, time.Duration) {
	now := l.clock.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()

	if entry, ok := l.entries[key]; ok {
		if now.Sub(entry.started) >= l.window || now.Before(entry.started) {
			l.entries[key] = limitEntry{started: now, count: 1}
			return true, 0
		}
		if entry.count >= l.limit {
			return false, positiveDuration(entry.started.Add(l.window).Sub(now))
		}
		entry.count++
		l.entries[key] = entry
		return true, 0
	}

	if len(l.entries) >= l.maxKeys {
		earliest := now.Add(l.window)
		for existingKey, entry := range l.entries {
			if now.Sub(entry.started) >= l.window || now.Before(entry.started) {
				delete(l.entries, existingKey)
				continue
			}
			if expiry := entry.started.Add(l.window); expiry.Before(earliest) {
				earliest = expiry
			}
		}
		if len(l.entries) >= l.maxKeys {
			return false, positiveDuration(earliest.Sub(now))
		}
	}
	l.entries[key] = limitEntry{started: now, count: 1}
	return true, 0
}

func positiveDuration(value time.Duration) time.Duration {
	if value <= 0 {
		return time.Second
	}
	return value
}

type abuseLimiters struct {
	loginKey, loginGlobal               *fixedWindowLimiter
	refreshKey, refreshGlobal           *fixedWindowLimiter
	registrationKey, registrationGlobal *fixedWindowLimiter
	verificationKey, verificationGlobal *fixedWindowLimiter
}

func newAbuseLimiters(clock Clock) *abuseLimiters {
	return &abuseLimiters{
		loginKey:           newFixedWindowLimiter(clock, loginKeyLimit, loginWindow, maxLimiterKeys),
		loginGlobal:        newFixedWindowLimiter(clock, loginGlobalLimit, loginWindow, 1),
		refreshKey:         newFixedWindowLimiter(clock, refreshKeyLimit, refreshWindow, maxLimiterKeys),
		refreshGlobal:      newFixedWindowLimiter(clock, refreshGlobalLimit, refreshWindow, 1),
		registrationKey:    newFixedWindowLimiter(clock, registrationKeyLimit, registrationWindow, maxLimiterKeys),
		registrationGlobal: newFixedWindowLimiter(clock, registrationGlobalLimit, registrationWindow, 1),
		verificationKey:    newFixedWindowLimiter(clock, verificationKeyLimit, verificationWindow, maxLimiterKeys),
		verificationGlobal: newFixedWindowLimiter(clock, verificationGlobalLimit, verificationWindow, 1),
	}
}

func (l *abuseLimiters) allowLogin(email string) (bool, time.Duration) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if canonical, err := identity.CanonicalizeEmail(email); err == nil {
		normalized = canonical.Canonical
	}
	if allowed, retry := l.loginGlobal.allow(globalLimitKey()); !allowed {
		return false, retry
	}
	return l.loginKey.allow(limitKey(loginKeyDomain, normalized))
}

func (l *abuseLimiters) allowRefresh(token string) (bool, time.Duration) {
	if allowed, retry := l.refreshGlobal.allow(globalLimitKey()); !allowed {
		return false, retry
	}
	return l.refreshKey.allow(limitKey(refreshKeyDomain, token))
}

func (l *abuseLimiters) allowRegistration(email string) (bool, time.Duration) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if canonical, err := identity.CanonicalizeEmail(email); err == nil {
		normalized = canonical.Canonical
	}
	if allowed, retry := l.registrationGlobal.allow(globalLimitKey()); !allowed {
		return false, retry
	}
	return l.registrationKey.allow(limitKey(registrationKeyDomain, normalized))
}

func (l *abuseLimiters) allowVerification(token string) (bool, time.Duration) {
	if allowed, retry := l.verificationGlobal.allow(globalLimitKey()); !allowed {
		return false, retry
	}
	return l.verificationKey.allow(limitKey(verificationKeyDomain, token))
}

func limitKey(domain []byte, value string) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write(domain)
	_, _ = hash.Write([]byte(value))
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func globalLimitKey() [sha256.Size]byte {
	var key [sha256.Size]byte
	binary.BigEndian.PutUint64(key[:8], 1)
	return key
}
