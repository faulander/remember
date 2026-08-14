// Package config loads and validates server process configuration.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	EnvListenAddr        = "REMEMBER_LISTEN_ADDR"
	EnvDatabasePath      = "REMEMBER_DB_PATH"
	EnvBlobRoot          = "REMEMBER_BLOB_ROOT"
	EnvStagingPath       = "REMEMBER_STAGING_PATH"
	EnvUserBlobQuota     = "REMEMBER_USER_BLOB_QUOTA_BYTES"
	EnvReadHeaderTimeout = "REMEMBER_HTTP_READ_HEADER_TIMEOUT"
	EnvReadTimeout       = "REMEMBER_HTTP_READ_TIMEOUT"
	EnvWriteTimeout      = "REMEMBER_HTTP_WRITE_TIMEOUT"
	EnvIdleTimeout       = "REMEMBER_HTTP_IDLE_TIMEOUT"
	EnvShutdownTimeout   = "REMEMBER_SHUTDOWN_TIMEOUT"
	EnvDatabaseBusy      = "REMEMBER_DB_BUSY_TIMEOUT"
	EnvSMTPAddress       = "REMEMBER_SMTP_ADDR"
	EnvSMTPUsername      = "REMEMBER_SMTP_USERNAME"
	EnvSMTPPassword      = "REMEMBER_SMTP_PASSWORD"
	EnvSMTPFrom          = "REMEMBER_SMTP_FROM"
	EnvSMTPTimeout       = "REMEMBER_SMTP_TIMEOUT"
	EnvEmailTokenKey     = "REMEMBER_EMAIL_TOKEN_KEY"
	EmailTokenKeyBytes   = 32
)

// Config contains only the foundation server settings.
type Config struct {
	ListenAddr         string
	DatabasePath       string
	BlobRoot           string
	StagingPath        string
	ReadHeaderTimeout  time.Duration
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	IdleTimeout        time.Duration
	ShutdownTimeout    time.Duration
	DatabaseBusy       time.Duration
	UserBlobQuotaBytes int64
	SMTPAddress        string
	SMTPUsername       string
	SMTPPassword       string
	SMTPFrom           string
	SMTPTimeout        time.Duration
	EmailTokenKey      []byte
}

// LookupEnv is compatible with os.LookupEnv and makes loading testable.
type LookupEnv func(string) (string, bool)

// Load reads strict environment overrides over secure development defaults.
func Load(lookup LookupEnv) (Config, error) {
	cfg := Config{
		ListenAddr:         "127.0.0.1:8080",
		DatabasePath:       "data/sqlite/remember.db",
		BlobRoot:           "data/blobs",
		StagingPath:        "data/staging",
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        15 * time.Second,
		WriteTimeout:       30 * time.Second,
		IdleTimeout:        60 * time.Second,
		ShutdownTimeout:    15 * time.Second,
		DatabaseBusy:       5 * time.Second,
		UserBlobQuotaBytes: 1024 * 1024 * 1024,
		SMTPTimeout:        10 * time.Second,
	}

	stringValue(lookup, EnvListenAddr, &cfg.ListenAddr)
	stringValue(lookup, EnvDatabasePath, &cfg.DatabasePath)
	stringValue(lookup, EnvBlobRoot, &cfg.BlobRoot)
	stringValue(lookup, EnvStagingPath, &cfg.StagingPath)
	stringValue(lookup, EnvSMTPAddress, &cfg.SMTPAddress)
	stringValue(lookup, EnvSMTPUsername, &cfg.SMTPUsername)
	stringValue(lookup, EnvSMTPPassword, &cfg.SMTPPassword)
	stringValue(lookup, EnvSMTPFrom, &cfg.SMTPFrom)
	if value, ok := lookup(EnvEmailTokenKey); ok {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
		if err != nil || len(decoded) != EmailTokenKeyBytes || base64.RawURLEncoding.EncodeToString(decoded) != value {
			return Config{}, fmt.Errorf("parse %s: expected canonical base64url-encoded 32-byte key", EnvEmailTokenKey)
		}
		cfg.EmailTokenKey = decoded
	}
	if value, ok := lookup(EnvUserBlobQuota); ok {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || strconv.FormatInt(parsed, 10) != value {
			return Config{}, fmt.Errorf("parse %s: expected canonical positive integer bytes", EnvUserBlobQuota)
		}
		cfg.UserBlobQuotaBytes = parsed
	}
	for name, target := range map[string]*time.Duration{
		EnvReadHeaderTimeout: &cfg.ReadHeaderTimeout,
		EnvReadTimeout:       &cfg.ReadTimeout,
		EnvWriteTimeout:      &cfg.WriteTimeout,
		EnvIdleTimeout:       &cfg.IdleTimeout,
		EnvShutdownTimeout:   &cfg.ShutdownTimeout,
		EnvDatabaseBusy:      &cfg.DatabaseBusy,
		EnvSMTPTimeout:       &cfg.SMTPTimeout,
	} {
		if value, ok := lookup(name); ok {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return Config{}, fmt.Errorf("parse %s: %w", name, err)
			}
			*target = parsed
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects configurations that can disable process safety bounds.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return fmt.Errorf("%s must not be empty", EnvListenAddr)
	}
	_, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", EnvListenAddr, err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("invalid %s port", EnvListenAddr)
	}
	if strings.TrimSpace(c.DatabasePath) == "" || strings.ContainsRune(c.DatabasePath, '\x00') {
		return fmt.Errorf("%s must be a filesystem path", EnvDatabasePath)
	}
	lowerPath := strings.ToLower(strings.TrimSpace(c.DatabasePath))
	if strings.HasPrefix(lowerPath, "file:") || strings.Contains(lowerPath, "?mode=") {
		return fmt.Errorf("%s must not be a SQLite DSN", EnvDatabasePath)
	}
	paths := map[string]string{EnvDatabasePath: c.DatabasePath, EnvBlobRoot: c.BlobRoot, EnvStagingPath: c.StagingPath}
	absolute := make(map[string]string, len(paths))
	for name, value := range paths {
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s must be a filesystem path", name)
		}
		resolved, err := filepath.Abs(value)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", name, err)
		}
		absolute[name] = filepath.Clean(resolved)
	}
	if pathsOverlap(absolute[EnvBlobRoot], absolute[EnvStagingPath]) ||
		pathsOverlap(absolute[EnvDatabasePath], absolute[EnvBlobRoot]) ||
		pathsOverlap(absolute[EnvDatabasePath], absolute[EnvStagingPath]) {
		return fmt.Errorf("database, blob, and staging paths must be distinct and non-overlapping")
	}
	for name, value := range map[string]time.Duration{
		EnvReadHeaderTimeout: c.ReadHeaderTimeout,
		EnvReadTimeout:       c.ReadTimeout,
		EnvWriteTimeout:      c.WriteTimeout,
		EnvIdleTimeout:       c.IdleTimeout,
		EnvShutdownTimeout:   c.ShutdownTimeout,
		EnvDatabaseBusy:      c.DatabaseBusy,
		EnvSMTPTimeout:       c.SMTPTimeout,
	} {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.DatabaseBusy < time.Millisecond {
		return fmt.Errorf("%s must be at least 1ms", EnvDatabaseBusy)
	}
	if c.ReadHeaderTimeout > c.ReadTimeout {
		return fmt.Errorf("%s must not exceed %s", EnvReadHeaderTimeout, EnvReadTimeout)
	}
	if c.UserBlobQuotaBytes <= 0 || c.UserBlobQuotaBytes > 1024*1024*1024*1024 {
		return fmt.Errorf("%s must be between 1 byte and 1 TiB", EnvUserBlobQuota)
	}
	if len(c.EmailTokenKey) != 0 && len(c.EmailTokenKey) != EmailTokenKeyBytes {
		return fmt.Errorf("%s must decode to 32 bytes", EnvEmailTokenKey)
	}
	configuredSMTPValues := 0
	for _, value := range []string{c.SMTPAddress, c.SMTPUsername, c.SMTPPassword, c.SMTPFrom} {
		if value != "" {
			configuredSMTPValues++
		}
	}
	if len(c.EmailTokenKey) != 0 {
		configuredSMTPValues++
	}
	if configuredSMTPValues != 0 && configuredSMTPValues != 5 {
		return errors.New("SMTP delivery settings and email token key must be configured together")
	}
	if configuredSMTPValues == 5 {
		host, port, err := net.SplitHostPort(c.SMTPAddress)
		if err != nil || host == "" || port == "" {
			return fmt.Errorf("invalid %s", EnvSMTPAddress)
		}
		address, err := mail.ParseAddress(c.SMTPFrom)
		if err != nil || address.Address != c.SMTPFrom {
			return fmt.Errorf("invalid %s", EnvSMTPFrom)
		}
		if strings.ContainsAny(c.SMTPUsername, "\r\n\x00") || strings.ContainsRune(c.SMTPPassword, '\x00') {
			return errors.New("invalid SMTP credentials")
		}
	}
	return nil
}

func (c Config) EmailDeliveryEnabled() bool {
	return c.SMTPAddress != "" && len(c.EmailTokenKey) == EmailTokenKeyBytes
}

func pathsOverlap(first, second string) bool {
	return pathWithin(first, second) || pathWithin(second, first)
}

func pathWithin(candidate, root string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func stringValue(lookup LookupEnv, name string, target *string) {
	if value, ok := lookup(name); ok {
		*target = value
	}
}
