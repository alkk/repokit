package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// migrations are applied in order; each entry's Version is the value written
// to meta.schema_version after a successful apply. Adding a new migration
// means appending to this slice — never reordering or rewriting existing
// entries.
var migrations = []migration{
	{
		Version: 1,
		Stmts: []string{
			`CREATE TABLE packages (
				id            INTEGER PRIMARY KEY,
				distro        TEXT NOT NULL,
				suite         TEXT NOT NULL,
				component     TEXT NOT NULL,
				architecture  TEXT NOT NULL,
				name          TEXT NOT NULL,
				version       TEXT NOT NULL,
				filename      TEXT NOT NULL,
				size          INTEGER NOT NULL,
				md5           TEXT NOT NULL,
				sha1          TEXT NOT NULL,
				sha256        TEXT NOT NULL,
				sha512        TEXT NOT NULL,
				control       TEXT NOT NULL,
				added_at      INTEGER NOT NULL,
				UNIQUE (distro, suite, component, architecture, name, version)
			)`,
			`CREATE INDEX idx_pkg_lookup    ON packages(distro, suite, component, architecture)`,
			`CREATE INDEX idx_pkg_name_ver  ON packages(name, version)`,
			`CREATE INDEX idx_pkg_sha256    ON packages(sha256)`,
		},
	},
}

// migration is a single forward-only schema step.
type migration struct {
	Version int
	Stmts   []string
}

// CurrentSchemaVersion is the highest schema version this binary knows how to
// produce.
func CurrentSchemaVersion() int {
	return migrations[len(migrations)-1].Version
}

// migrate brings the database up to CurrentSchemaVersion, applying any
// missing migrations in a single transaction each.
func (r *Repo) migrate(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("ensuring meta table: %w", err)
	}
	current, err := readSchemaVersion(ctx, r.db)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if err := r.applyMigration(ctx, m); err != nil {
			return fmt.Errorf("applying migration v%d: %w", m.Version, err)
		}
	}
	return nil
}

// applyMigration runs one migration's statements then bumps schema_version,
// all inside a single transaction.
func (r *Repo) applyMigration(ctx context.Context, m migration) error {
	return r.Tx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range m.Stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("exec %q: %w", firstLine(stmt), err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			strconv.Itoa(m.Version)); err != nil {
			return fmt.Errorf("recording schema_version: %w", err)
		}
		return nil
	})
}

// readSchemaVersion returns the integer value stored under meta.schema_version,
// or 0 if the key is missing (fresh DB).
func readSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading schema_version: %w", err)
	}
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		return 0, fmt.Errorf("parsing schema_version %q: %w", raw, err)
	}
	return v, nil
}

// firstLine returns the first non-empty line of s, useful for error messages
// that quote multi-line SQL statements.
func firstLine(s string) string {
	for i := range len(s) {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
