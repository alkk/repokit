package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Package is a single row from the packages table.
type Package struct {
	ID           int64
	Distro       string
	Suite        string
	Component    string
	Architecture string
	Name         string
	Version      string
	Filename     string
	Size         int64
	MD5          string
	SHA1         string
	SHA256       string
	SHA512       string
	Control      string
	AddedAt      time.Time
}

// Filter narrows queries by exact-match equality on selected columns. Empty
// fields are ignored. Filters compose as AND across populated fields.
type Filter struct {
	Distro       string
	Suite        string
	Component    string
	Architecture string
	Name         string
	Version      string
	SHA256       string
}

// ErrDuplicate is returned by Insert when a package with the same
// (distro, suite, component, architecture, name, version) tuple already
// exists.
var ErrDuplicate = errors.New("repo: duplicate package")

// ErrNotFound is returned by Get when no row matches the requested
// coordinates.
var ErrNotFound = errors.New("repo: package not found")

// queryer is the read/write surface shared by *sql.DB and *sql.Tx that
// query helpers consume.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// scanner is implemented by *sql.Row and *sql.Rows for shared row scanning.
type scanner interface {
	Scan(dest ...any) error
}

// packageColumns is the SELECT projection used everywhere a Package is
// materialized, kept in sync with scanPackage.
const packageColumns = `id, distro, suite, component, architecture, name, version,
	filename, size, md5, sha1, sha256, sha512, control, added_at`

// Insert adds p inside tx. ErrDuplicate is returned when the NVRA tuple is
// already present. AddedAt defaults to time.Now() when zero.
func (r *Repo) Insert(ctx context.Context, tx *sql.Tx, p Package) error {
	addedAt := p.AddedAt
	if addedAt.IsZero() {
		addedAt = time.Now()
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO packages (distro, suite, component, architecture, name, version,
			filename, size, md5, sha1, sha256, sha512, control, added_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Distro, p.Suite, p.Component, p.Architecture, p.Name, p.Version,
		p.Filename, p.Size, p.MD5, p.SHA1, p.SHA256, p.SHA512, p.Control, addedAt.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("inserting %s/%s/%s/%s/%s/%s: %w",
			p.Distro, p.Suite, p.Component, p.Architecture, p.Name, p.Version, err)
	}
	return nil
}

// Get returns the row matching the full NVRA coordinates inside tx, or
// ErrNotFound if no row matches.
func (r *Repo) Get(ctx context.Context, tx *sql.Tx,
	distro, suite, component, arch, name, version string) (*Package, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+packageColumns+`
		 FROM packages
		 WHERE distro=? AND suite=? AND component=? AND architecture=?
		   AND name=? AND version=?`,
		distro, suite, component, arch, name, version)
	p, err := scanPackage(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting %s/%s/%s/%s/%s/%s: %w",
			distro, suite, component, arch, name, version, err)
	}
	return p, nil
}

// List returns all rows matching filter, ordered for stable diffing.
func (r *Repo) List(ctx context.Context, filter Filter) ([]Package, error) {
	return queryPackages(ctx, r.db, filter, "")
}

// ListConn returns rows matching filter using the supplied connection,
// reading the snapshot held by an explicit BEGIN IMMEDIATE on that
// connection (see WithWriterLock).
func (r *Repo) ListConn(ctx context.Context, conn *sql.Conn, filter Filter) ([]Package, error) {
	return queryPackages(ctx, conn, filter, "")
}

// ExistsByFilenameConn reports whether any row in distro references filename,
// using the supplied writer-locked connection (see WithWriterLock). Filename
// is the apt-relative form stored in the DB (no leading distro segment).
// Used to revalidate a pool path the caller is about to unlink: if any row
// still claims the path, deleting it would orphan that row's committed bytes.
func (r *Repo) ExistsByFilenameConn(ctx context.Context, conn *sql.Conn, distro, filename string) (bool, error) {
	row := conn.QueryRowContext(ctx,
		`SELECT 1 FROM packages WHERE distro = ? AND filename = ? LIMIT 1`,
		distro, filename)
	var found int
	if err := row.Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("checking filename %s/%s: %w", distro, filename, err)
	}
	return true, nil
}

// Search returns rows where query matches name or control text
// (case-insensitive substring match). Filter further narrows the result set.
// The search is a linear LIKE scan; acceptable up to thousands of packages.
func (r *Repo) Search(ctx context.Context, query string, filter Filter) ([]Package, error) {
	return queryPackages(ctx, r.db, filter, query)
}

// Delete removes all rows matching filter inside tx and returns the deleted
// rows so the caller can clean up pool files referenced by Filename.
// An empty filter is rejected to prevent whole-table wipes.
func (r *Repo) Delete(ctx context.Context, tx *sql.Tx, filter Filter) ([]Package, error) {
	return deletePackages(ctx, tx, filter)
}

// DeleteConn removes all rows matching filter using the supplied connection,
// reading and writing through the writer lock held by an explicit
// BEGIN IMMEDIATE on that connection (see WithWriterLock). The semantics
// match Delete; the caller is responsible for committing or rolling back
// the surrounding transaction on the connection.
func (r *Repo) DeleteConn(ctx context.Context, conn *sql.Conn, filter Filter) ([]Package, error) {
	return deletePackages(ctx, conn, filter)
}

// deletePackages is the shared implementation behind Delete and DeleteConn.
// It works against any queryer so the same SELECT-then-DELETE pair can run
// inside a *sql.Tx or directly on a writer-locked *sql.Conn.
func deletePackages(ctx context.Context, q queryer, filter Filter) ([]Package, error) {
	if filter == (Filter{}) {
		return nil, errors.New("repo: Delete requires at least one filter field")
	}
	rows, err := queryPackages(ctx, q, filter, "")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return rows, nil
	}
	where, args := buildWhere(filter, "")
	stmt := "DELETE FROM packages WHERE " + where
	if _, err := q.ExecContext(ctx, stmt, args...); err != nil {
		return nil, fmt.Errorf("deleting packages: %w", err)
	}
	return rows, nil
}

// EnumerateForSuite returns the distinct components and architectures present
// in the given (distro, suite) scope, sorted ascending. Empty slices are
// returned when the scope is empty (no error).
func (r *Repo) EnumerateForSuite(ctx context.Context, distro, suite string) ([]string, []string, error) {
	components, err := collectStrings(ctx, r.db,
		`SELECT DISTINCT component FROM packages
		 WHERE distro=? AND suite=? ORDER BY component`, distro, suite)
	if err != nil {
		return nil, nil, fmt.Errorf("enumerating components for %s/%s: %w", distro, suite, err)
	}
	architectures, err := collectStrings(ctx, r.db,
		`SELECT DISTINCT architecture FROM packages
		 WHERE distro=? AND suite=? ORDER BY architecture`, distro, suite)
	if err != nil {
		return nil, nil, fmt.Errorf("enumerating architectures for %s/%s: %w", distro, suite, err)
	}
	return components, architectures, nil
}

// queryPackages runs the shared package SELECT against any queryer
// (*sql.DB for read-only paths, *sql.Tx when work is part of a transaction).
func queryPackages(ctx context.Context, q queryer, filter Filter, search string) ([]Package, error) {
	where, args := buildWhere(filter, search)
	stmt := "SELECT " + packageColumns + " FROM packages"
	if where != "" {
		stmt += " WHERE " + where
	}
	stmt += " ORDER BY distro, suite, component, architecture, name, version"
	rows, err := q.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("querying packages: %w", err)
	}
	defer rows.Close()
	var out []Package
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning package: %w", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating packages: %w", err)
	}
	return out, nil
}

// buildWhere produces the WHERE clause body and args for filter (and an
// optional substring search across name/control). The returned string is
// empty when no clauses apply.
func buildWhere(f Filter, search string) (string, []any) {
	var clauses []string
	var args []any
	addEq := func(col, val string) {
		if val != "" {
			clauses = append(clauses, col+" = ?")
			args = append(args, val)
		}
	}
	addEq("distro", f.Distro)
	addEq("suite", f.Suite)
	addEq("component", f.Component)
	addEq("architecture", f.Architecture)
	addEq("name", f.Name)
	addEq("version", f.Version)
	addEq("sha256", f.SHA256)
	if search != "" {
		like := "%" + escapeLike(search) + "%"
		clauses = append(clauses,
			`(LOWER(name) LIKE LOWER(?) ESCAPE '\' OR LOWER(control) LIKE LOWER(?) ESCAPE '\')`)
		args = append(args, like, like)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return strings.Join(clauses, " AND "), args
}

// escapeLike escapes the three LIKE metacharacters (% _ \) so that user input
// is treated literally inside the surrounding %...% wrap.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// collectStrings scans a single-column query into a []string.
func collectStrings(ctx context.Context, q queryer, stmt string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("querying: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}
	return out, nil
}

// scanPackage materializes a Package from one row. Works against both
// *sql.Row and *sql.Rows.
func scanPackage(s scanner) (*Package, error) {
	var (
		p       Package
		addedAt int64
	)
	if err := s.Scan(
		&p.ID, &p.Distro, &p.Suite, &p.Component, &p.Architecture,
		&p.Name, &p.Version, &p.Filename, &p.Size,
		&p.MD5, &p.SHA1, &p.SHA256, &p.SHA512,
		&p.Control, &addedAt,
	); err != nil {
		return nil, err //nolint:wrapcheck // caller adds context including sql.ErrNoRows handling
	}
	p.AddedAt = time.Unix(addedAt, 0).UTC()
	return &p, nil
}

// isUniqueViolation reports whether err is a SQLite unique-constraint
// violation, regardless of the indexed columns.
func isUniqueViolation(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}
	return serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
