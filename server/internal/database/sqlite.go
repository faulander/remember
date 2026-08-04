// Package database owns server-side SQLite configuration and migrations.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// Open creates a single-process SQLite database with required safety PRAGMAs.
func Open(ctx context.Context, path string, busyTimeout time.Duration) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	dsn, err := connectionDSN(absolute, busyTimeout)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := verifyConfiguration(ctx, db, busyTimeout); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(absolute, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("restrict database permissions: %w", err)
	}
	return db, nil
}

func connectionDSN(absolute string, busyTimeout time.Duration) (string, error) {
	busyMilliseconds := busyTimeout.Milliseconds()
	if busyMilliseconds <= 0 {
		return "", fmt.Errorf("database busy timeout must be at least one millisecond")
	}
	location := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := url.Values{}
	for _, pragma := range []string{
		"foreign_keys(1)",
		"busy_timeout(" + strconv.FormatInt(busyMilliseconds, 10) + ")",
		"journal_mode(WAL)",
		"synchronous(FULL)",
		"trusted_schema(OFF)",
	} {
		query.Add("_pragma", pragma)
	}
	location.RawQuery = query.Encode()
	return location.String(), nil
}

func verifyConfiguration(ctx context.Context, db *sql.DB, busyTimeout time.Duration) error {
	expected := map[string]string{
		"foreign_keys":   "1",
		"journal_mode":   "wal",
		"synchronous":    "2",
		"trusted_schema": "0",
		"busy_timeout":   strconv.FormatInt(busyTimeout.Milliseconds(), 10),
	}
	for pragma, want := range expected {
		var got string
		if err := db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			return fmt.Errorf("verify sqlite configuration: %w", err)
		}
		if got != want {
			return fmt.Errorf("sqlite PRAGMA %s has unexpected value", pragma)
		}
	}
	return nil
}

// Ready performs a bounded database probe for the readiness endpoint.
func Ready(ctx context.Context, db *sql.DB) error {
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	if one != 1 {
		return fmt.Errorf("unexpected database probe result")
	}
	return nil
}
