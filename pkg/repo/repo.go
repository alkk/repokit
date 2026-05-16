// Package repo owns the SQLite database that holds repokit's package metadata.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // sqlite driver registration
)

// driverName is the database/sql driver registered by modernc.org/sqlite.
const driverName = "sqlite"

// Repo is a handle to the repokit metadata database.
type Repo struct {
	db   *sql.DB
	path string
}

// Open opens (or creates) the SQLite database at path, applies pending
// migrations, and configures pragmas for safe concurrent use.
func Open(ctx context.Context, path string) (*Repo, error) {
	// Build the DSN through net/url so a `?`, `#`, or `&` in the user-supplied
	// path is escaped and cannot consume the pragma query parameters.
	u := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
	}
	dsn := u.String()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite at %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging sqlite at %s: %w", path, err)
	}
	r := &Repo{db: db, path: path}
	if err := r.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

// Close releases the underlying database handle.
func (r *Repo) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	if err := r.db.Close(); err != nil {
		return fmt.Errorf("closing sqlite at %s: %w", r.path, err)
	}
	return nil
}

// DB returns the underlying *sql.DB. Exposed for sibling packages that need
// read-only queries; mutations should still go through Tx for atomicity.
func (r *Repo) DB() *sql.DB {
	return r.db
}

// Tx runs fn inside a single transaction. It commits when fn returns nil and
// rolls back otherwise. Rollback errors are joined to fn's error so neither is
// silently dropped.
func (r *Repo) Tx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, fmt.Errorf("rolling back: %w", rbErr))
			}
			return
		}
		if cmErr := tx.Commit(); cmErr != nil {
			err = fmt.Errorf("committing transaction: %w", cmErr)
		}
	}()
	return fn(tx)
}

// WithWriterLock holds the SQLite writer lock for the entire callback by
// running BEGIN IMMEDIATE on a dedicated connection. Any concurrent writer
// (an Insert/Delete inside Tx, or another WithWriterLock holder) blocks
// until fn returns. Use this when fn must coordinate a database snapshot
// with filesystem mutations whose correctness depends on no other writer
// committing in between — e.g. gc, which deletes pool files
// based on the set of committed rows. Without the lock, a concurrent add
// can write its pool bytes while its transaction is still uncommitted;
// the gc walk sees that file as an orphan and removes it before the row
// becomes visible.
func (r *Repo) WithWriterLock(ctx context.Context, fn func(ctx context.Context, conn *sql.Conn) error) (err error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()

	if _, beginErr := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); beginErr != nil {
		return fmt.Errorf("acquiring writer lock: %w", beginErr)
	}
	defer func() {
		if p := recover(); p != nil {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
			panic(p)
		}
		if err != nil {
			if _, rbErr := conn.ExecContext(ctx, "ROLLBACK"); rbErr != nil {
				err = errors.Join(err, fmt.Errorf("rolling back writer lock: %w", rbErr))
			}
			return
		}
		if _, cmErr := conn.ExecContext(ctx, "COMMIT"); cmErr != nil {
			err = fmt.Errorf("releasing writer lock: %w", cmErr)
		}
	}()
	return fn(ctx, conn)
}
