package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTempRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Open(context.Background(), filepath.Join(t.TempDir(), "repo.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestOpenCreatesSchema(t *testing.T) {
	r := openTempRepo(t)

	t.Run("packages table exists with expected columns", func(t *testing.T) {
		cols := tableColumns(t, r.DB(), "packages")
		want := []string{
			"id", "distro", "suite", "component", "architecture", "name",
			"version", "filename", "size", "md5", "sha1", "sha256", "sha512",
			"control", "added_at",
		}
		assert.Equal(t, want, cols)
	})

	t.Run("meta table exists", func(t *testing.T) {
		cols := tableColumns(t, r.DB(), "meta")
		assert.Equal(t, []string{"key", "value"}, cols)
	})

	t.Run("expected indexes exist", func(t *testing.T) {
		idx := indexes(t, r.DB(), "packages")
		assert.Contains(t, idx, "idx_pkg_lookup")
		assert.Contains(t, idx, "idx_pkg_name_ver")
		assert.Contains(t, idx, "idx_pkg_sha256")
	})

	t.Run("schema_version is current", func(t *testing.T) {
		v, err := readSchemaVersion(context.Background(), r.DB())
		require.NoError(t, err)
		assert.Equal(t, CurrentSchemaVersion(), v)
	})

	t.Run("WAL journal mode is set", func(t *testing.T) {
		var mode string
		require.NoError(t, r.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode))
		assert.Equal(t, "wal", mode)
	})

	t.Run("foreign_keys pragma is on", func(t *testing.T) {
		var on int
		require.NoError(t, r.DB().QueryRow(`PRAGMA foreign_keys`).Scan(&on))
		assert.Equal(t, 1, on)
	})
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repo.db")

	r1, err := Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, r1.Close())

	r2, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r2.Close() })

	v, err := readSchemaVersion(context.Background(), r2.DB())
	require.NoError(t, err)
	assert.Equal(t, CurrentSchemaVersion(), v)

	row := r2.DB().QueryRow(`SELECT COUNT(*) FROM meta WHERE key='schema_version'`)
	var n int
	require.NoError(t, row.Scan(&n))
	assert.Equal(t, 1, n, "reopening must not re-insert schema_version")
}

func TestOpenInvalidPath(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "missing", "repo.db"))
	require.Error(t, err)
}

func TestTxCommitsOnSuccess(t *testing.T) {
	r := openTempRepo(t)

	err := r.Tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('test_key', 'val')`); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return nil
	})
	require.NoError(t, err)

	var got string
	require.NoError(t, r.DB().QueryRow(`SELECT value FROM meta WHERE key='test_key'`).Scan(&got))
	assert.Equal(t, "val", got)
}

func TestTxRollsBackOnError(t *testing.T) {
	r := openTempRepo(t)

	sentinel := errors.New("boom")
	err := r.Tx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('rb_key', 'val')`); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	row := r.DB().QueryRow(`SELECT COUNT(*) FROM meta WHERE key='rb_key'`)
	var n int
	require.NoError(t, row.Scan(&n))
	assert.Equal(t, 0, n, "Tx must roll back failed work")
}

func TestTxRollsBackOnPanic(t *testing.T) {
	r := openTempRepo(t)

	require.PanicsWithValue(t, "boom", func() {
		_ = r.Tx(context.Background(), func(tx *sql.Tx) error {
			_, _ = tx.Exec(`INSERT INTO meta(key, value) VALUES('panic_key', 'val')`)
			panic("boom")
		})
	})

	row := r.DB().QueryRow(`SELECT COUNT(*) FROM meta WHERE key='panic_key'`)
	var n int
	require.NoError(t, row.Scan(&n))
	assert.Equal(t, 0, n, "Tx must roll back on panic")
}

func TestCloseIsSafeOnNil(t *testing.T) {
	var r *Repo
	assert.NoError(t, r.Close())
}

func TestWithWriterLockBlocksConcurrentWriter(t *testing.T) {
	r := openTempRepo(t)

	released := make(chan struct{})
	holderReady := make(chan struct{})

	var wg sync.WaitGroup
	wg.Go(func() {
		_ = r.WithWriterLock(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
			close(holderReady)
			<-released
			return nil
		})
	})

	<-holderReady

	// A writer transaction starting now must block on the writer lock until
	// the holder releases it. The Tx callback never fires until the second
	// goroutine releases the lock; we observe that by racing a short timer
	// against the tx attempt.
	insertDone := make(chan error, 1)
	go func() {
		insertDone <- r.Tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(context.Background(),
				`INSERT INTO meta(key, value) VALUES('blocked', 'v')`)
			if err != nil {
				return fmt.Errorf("insert: %w", err)
			}
			return nil
		})
	}()

	select {
	case err := <-insertDone:
		t.Fatalf("concurrent writer completed while WithWriterLock was held: err=%v", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(released)
	wg.Wait()

	select {
	case err := <-insertDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent writer did not complete after writer lock release")
	}
}

func TestWithWriterLockCommitsOnSuccess(t *testing.T) {
	r := openTempRepo(t)

	err := r.WithWriterLock(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('lk', 'v')`); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return nil
	})
	require.NoError(t, err)

	var got string
	require.NoError(t, r.DB().QueryRow(`SELECT value FROM meta WHERE key='lk'`).Scan(&got))
	assert.Equal(t, "v", got)
}

func TestWithWriterLockRollsBackOnError(t *testing.T) {
	r := openTempRepo(t)

	sentinel := errors.New("boom")
	err := r.WithWriterLock(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('rb', 'v')`); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	var n int
	require.NoError(t, r.DB().QueryRow(`SELECT COUNT(*) FROM meta WHERE key='rb'`).Scan(&n))
	assert.Equal(t, 0, n, "WithWriterLock must roll back failed work")
}

// tableColumns returns column names for the named table in schema declaration
// order.
func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		out = append(out, name)
	}
	require.NoError(t, rows.Err())
	return out
}

// indexes returns user-created index names on the given table.
func indexes(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name=? AND name NOT LIKE 'sqlite_%'`, table)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		out = append(out, name)
	}
	require.NoError(t, rows.Err())
	return out
}
