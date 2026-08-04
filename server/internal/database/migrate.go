package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int
	name     string
	checksum string
	body     []byte
}

// Migrate applies all embedded migrations and rejects altered history.
func Migrate(ctx context.Context, db *sql.DB) error {
	return migrateFS(ctx, db, migrationFiles, "migrations/*.sql")
}

func migrateFS(ctx context.Context, db *sql.DB, source fs.FS, pattern string) error {
	migrations, err := discoverMigrations(source, pattern)
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return fmt.Errorf("no database migrations found")
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			checksum TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	type appliedMigration struct{ name, checksum string }
	applied := make(map[int]appliedMigration)
	for rows.Next() {
		var version int
		var item appliedMigration
		if err := rows.Scan(&version, &item.name, &item.checksum); err != nil {
			rows.Close()
			return fmt.Errorf("scan migration ledger: %w", err)
		}
		applied[version] = item
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close migration ledger: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration ledger: %w", err)
	}

	known := make(map[int]migration, len(migrations))
	for _, item := range migrations {
		known[item.version] = item
	}
	for version, item := range applied {
		expected, ok := known[version]
		if !ok {
			return fmt.Errorf("database migration %d is newer than or unknown to this binary", version)
		}
		if item.name != expected.name || item.checksum != expected.checksum {
			return fmt.Errorf("applied database migration %d does not match embedded history", version)
		}
	}
	missingPrefix := false
	for _, item := range migrations {
		_, exists := applied[item.version]
		if !exists {
			missingPrefix = true
			continue
		}
		if missingPrefix {
			return fmt.Errorf("applied database migrations are not an exact history prefix")
		}
	}

	for _, item := range migrations {
		if _, exists := applied[item.version]; exists {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", item.version, err)
		}
		if _, err := tx.ExecContext(ctx, string(item.body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", item.version, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, checksum) VALUES (?, ?, ?)",
			item.version, item.name, item.checksum,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", item.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", item.version, err)
		}
	}
	return nil
}

func discoverMigrations(source fs.FS, pattern string) ([]migration, error) {
	names, err := fs.Glob(source, pattern)
	if err != nil {
		return nil, fmt.Errorf("list database migrations: %w", err)
	}
	sort.Strings(names)
	seen := make(map[int]string)
	items := make([]migration, 0, len(names))
	for _, name := range names {
		base := path.Base(name)
		prefix, _, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("invalid migration filename %q", base)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version in %q", base)
		}
		if previous, duplicate := seen[version]; duplicate {
			return nil, fmt.Errorf("duplicate migration version %d in %q and %q", version, previous, base)
		}
		body, err := fs.ReadFile(source, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", base, err)
		}
		hash := sha256.Sum256(body)
		items = append(items, migration{
			version: version, name: base, checksum: hex.EncodeToString(hash[:]), body: body,
		})
		seen[version] = base
	}
	sort.Slice(items, func(i, j int) bool { return items[i].version < items[j].version })
	return items, nil
}
