package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// samplePackage produces a Package populated with deterministic values for
// the supplied coordinates. Hash strings are synthetic but unique across
// (name, version, arch) so duplicate-detection tests can compare them.
func samplePackage(distro, suite, component, arch, name, version string) Package {
	id := fmt.Sprintf("%s_%s_%s", name, version, arch)
	return Package{
		Distro:       distro,
		Suite:        suite,
		Component:    component,
		Architecture: arch,
		Name:         name,
		Version:      version,
		Filename:     fmt.Sprintf("pool/%s/%s/%c/%s/%s.deb", suite, component, name[0], name, id),
		Size:         int64(len(id) * 100),
		MD5:          "md5-" + id,
		SHA1:         "sha1-" + id,
		SHA256:       "sha256-" + id,
		SHA512:       "sha512-" + id,
		Control: "Package: " + name + "\n" +
			"Version: " + version + "\n" +
			"Architecture: " + arch + "\n" +
			"Description: example " + name + " package\n",
		AddedAt: time.Unix(1700000000, 0).UTC(),
	}
}

// insertAll inserts every supplied package inside one transaction.
func insertAll(t *testing.T, r *Repo, pkgs ...Package) {
	t.Helper()
	require.NoError(t, r.Tx(context.Background(), func(tx *sql.Tx) error {
		for _, p := range pkgs {
			if err := r.Insert(context.Background(), tx, p); err != nil {
				return fmt.Errorf("insert %s: %w", p.Name, err)
			}
		}
		return nil
	}))
}

func TestInsertAndGet(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()
	p := samplePackage("debian", "bookworm", "main", "amd64", "hello", "1.0")

	insertAll(t, r, p)

	var got *Package
	require.NoError(t, r.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		got, err = r.Get(ctx, tx, p.Distro, p.Suite, p.Component, p.Architecture, p.Name, p.Version)
		return err
	}))

	require.NotNil(t, got)
	assert.NotZero(t, got.ID)
	assert.Equal(t, p.Distro, got.Distro)
	assert.Equal(t, p.Suite, got.Suite)
	assert.Equal(t, p.Component, got.Component)
	assert.Equal(t, p.Architecture, got.Architecture)
	assert.Equal(t, p.Name, got.Name)
	assert.Equal(t, p.Version, got.Version)
	assert.Equal(t, p.Filename, got.Filename)
	assert.Equal(t, p.Size, got.Size)
	assert.Equal(t, p.MD5, got.MD5)
	assert.Equal(t, p.SHA1, got.SHA1)
	assert.Equal(t, p.SHA256, got.SHA256)
	assert.Equal(t, p.SHA512, got.SHA512)
	assert.Equal(t, p.Control, got.Control)
	assert.Equal(t, p.AddedAt.Unix(), got.AddedAt.Unix())
}

func TestInsertDuplicateReturnsErrDuplicate(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()
	p := samplePackage("debian", "bookworm", "main", "amd64", "hello", "1.0")

	insertAll(t, r, p)

	err := r.Tx(ctx, func(tx *sql.Tx) error {
		return r.Insert(ctx, tx, p)
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDuplicate)
}

func TestInsertDefaultsAddedAtWhenZero(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()
	p := samplePackage("debian", "bookworm", "main", "amd64", "hello", "1.0")
	p.AddedAt = time.Time{}

	before := time.Now().Add(-time.Second)
	insertAll(t, r, p)
	after := time.Now().Add(time.Second)

	var got *Package
	require.NoError(t, r.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		got, err = r.Get(ctx, tx, p.Distro, p.Suite, p.Component, p.Architecture, p.Name, p.Version)
		return err
	}))
	require.NotNil(t, got)
	assert.False(t, got.AddedAt.Before(before.Truncate(time.Second)),
		"AddedAt %s should be >= before %s", got.AddedAt, before)
	assert.False(t, got.AddedAt.After(after),
		"AddedAt %s should be <= after %s", got.AddedAt, after)
}

func TestGetNotFound(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()

	err := r.Tx(ctx, func(tx *sql.Tx) error {
		_, err := r.Get(ctx, tx, "debian", "bookworm", "main", "amd64", "missing", "1.0")
		return err
	})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestList(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()

	a := samplePackage("debian", "bookworm", "main", "amd64", "alpha", "1.0")
	b := samplePackage("debian", "bookworm", "main", "arm64", "alpha", "1.0")
	c := samplePackage("debian", "bookworm", "contrib", "amd64", "beta", "2.0")
	d := samplePackage("debian", "bullseye", "main", "amd64", "alpha", "1.0")
	insertAll(t, r, a, b, c, d)

	tests := []struct {
		name      string
		filter    Filter
		wantNames []string
	}{
		{
			// ORDER BY distro, suite, component, architecture, name, version:
			// bookworm/contrib/amd64/beta, bookworm/main/amd64/alpha,
			// bookworm/main/arm64/alpha, bullseye/main/amd64/alpha.
			name:      "no filter returns everything ordered",
			filter:    Filter{},
			wantNames: []string{"beta", "alpha", "alpha", "alpha"},
		},
		{
			name:      "filter by suite",
			filter:    Filter{Suite: "bookworm"},
			wantNames: []string{"beta", "alpha", "alpha"},
		},
		{
			name:      "filter by distro+suite+arch",
			filter:    Filter{Distro: "debian", Suite: "bookworm", Architecture: "arm64"},
			wantNames: []string{"alpha"},
		},
		{
			name:      "filter by component",
			filter:    Filter{Component: "contrib"},
			wantNames: []string{"beta"},
		},
		{
			name:      "filter by sha256",
			filter:    Filter{SHA256: c.SHA256},
			wantNames: []string{"beta"},
		},
		{
			name:      "filter that matches nothing",
			filter:    Filter{Distro: "ubuntu"},
			wantNames: []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := r.List(ctx, tc.filter)
			require.NoError(t, err)
			got := make([]string, len(rows))
			for i, p := range rows {
				got[i] = p.Name
			}
			assert.Equal(t, tc.wantNames, got)
		})
	}
}

func TestListConn(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()

	a := samplePackage("debian", "bookworm", "main", "amd64", "alpha", "1.0")
	b := samplePackage("debian", "bookworm", "main", "arm64", "alpha", "1.0")
	c := samplePackage("debian", "bookworm", "contrib", "amd64", "beta", "2.0")
	d := samplePackage("ubuntu", "noble", "main", "amd64", "alpha", "1.0")
	insertAll(t, r, a, b, c, d)

	listConn := func(t *testing.T, filter Filter) []Package {
		t.Helper()
		var rows []Package
		require.NoError(t, r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
			var qerr error
			rows, qerr = r.ListConn(ctx, conn, filter)
			return qerr
		}))
		return rows
	}

	t.Run("returns the same rows as List for the same filter", func(t *testing.T) {
		for _, filter := range []Filter{
			{},
			{Distro: "debian"},
			{Suite: "bookworm", Name: "alpha"},
		} {
			want, err := r.List(ctx, filter)
			require.NoError(t, err)
			assert.Equal(t, want, listConn(t, filter),
				"ListConn must match List for filter %+v", filter)
		}
	})

	t.Run("honors Distro filter", func(t *testing.T) {
		rows := listConn(t, Filter{Distro: "ubuntu"})
		require.Len(t, rows, 1)
		assert.Equal(t, "ubuntu", rows[0].Distro)
	})

	t.Run("honors Suite filter", func(t *testing.T) {
		rows := listConn(t, Filter{Suite: "noble"})
		require.Len(t, rows, 1)
		assert.Equal(t, "noble", rows[0].Suite)
	})

	t.Run("honors Name filter", func(t *testing.T) {
		rows := listConn(t, Filter{Name: "beta"})
		require.Len(t, rows, 1)
		assert.Equal(t, "beta", rows[0].Name)
	})
}

func TestSearch(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()

	hello := samplePackage("debian", "bookworm", "main", "amd64", "hello", "1.0")
	hello.Control = "Package: hello\nDescription: greets the world politely\n"
	world := samplePackage("debian", "bookworm", "main", "amd64", "world", "1.0")
	world.Control = "Package: world\nDescription: planet representation\n"
	other := samplePackage("debian", "bullseye", "main", "amd64", "tools", "1.0")
	other.Control = "Package: tools\nDescription: assorted utilities\n"
	insertAll(t, r, hello, world, other)

	t.Run("matches by name (case-insensitive)", func(t *testing.T) {
		rows, err := r.Search(ctx, "HELLO", Filter{})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "hello", rows[0].Name)
	})

	t.Run("matches by description in control", func(t *testing.T) {
		rows, err := r.Search(ctx, "planet", Filter{})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "world", rows[0].Name)
	})

	t.Run("filter narrows search scope", func(t *testing.T) {
		rows, err := r.Search(ctx, "Description", Filter{Suite: "bullseye"})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "tools", rows[0].Name)
	})

	t.Run("no matches returns empty slice", func(t *testing.T) {
		rows, err := r.Search(ctx, "nonexistent", Filter{})
		require.NoError(t, err)
		assert.Empty(t, rows)
	})

	t.Run("LIKE metacharacters in query are treated literally", func(t *testing.T) {
		for _, meta := range []string{"%", "_", `\`} {
			t.Run(meta, func(t *testing.T) {
				rows, err := r.Search(ctx, meta, Filter{})
				require.NoError(t, err)
				assert.Empty(t, rows, "metacharacter %q must not match anything", meta)
			})
		}
	})
}

func TestDelete(t *testing.T) {
	ctx := context.Background()

	a := samplePackage("debian", "bookworm", "main", "amd64", "alpha", "1.0")
	b := samplePackage("debian", "bookworm", "main", "amd64", "alpha", "2.0")
	c := samplePackage("debian", "bookworm", "main", "arm64", "alpha", "1.0")
	d := samplePackage("debian", "bullseye", "main", "amd64", "alpha", "1.0")

	seed := func(t *testing.T) *Repo {
		t.Helper()
		r := openTempRepo(t)
		insertAll(t, r, a, b, c, d)
		return r
	}

	t.Run("delete by name+version returns matching rows", func(t *testing.T) {
		r := seed(t)
		var deleted []Package
		require.NoError(t, r.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			deleted, err = r.Delete(ctx, tx, Filter{Name: "alpha", Version: "1.0"})
			return err
		}))
		require.Len(t, deleted, 3)
		remaining, err := r.List(ctx, Filter{})
		require.NoError(t, err)
		require.Len(t, remaining, 1)
		assert.Equal(t, "2.0", remaining[0].Version)
	})

	t.Run("delete by sha256", func(t *testing.T) {
		r := seed(t)
		var deleted []Package
		require.NoError(t, r.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			deleted, err = r.Delete(ctx, tx, Filter{SHA256: c.SHA256})
			return err
		}))
		require.Len(t, deleted, 1)
		assert.Equal(t, c.SHA256, deleted[0].SHA256)
	})

	t.Run("empty filter is rejected", func(t *testing.T) {
		r := seed(t)
		err := r.Tx(ctx, func(tx *sql.Tx) error {
			_, derr := r.Delete(ctx, tx, Filter{})
			return derr
		})
		require.Error(t, err)
	})

	t.Run("no matches returns empty slice without error", func(t *testing.T) {
		r := seed(t)
		var deleted []Package
		require.NoError(t, r.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			deleted, err = r.Delete(ctx, tx, Filter{Name: "ghost"})
			return err
		}))
		assert.Empty(t, deleted)
	})
}

func TestExistsByFilenameConn(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()

	a := samplePackage("debian", "bookworm", "main", "amd64", "alpha", "1.0")
	b := samplePackage("ubuntu", "noble", "main", "amd64", "alpha", "1.0")
	b.Filename = a.Filename // same filename in a different distro
	insertAll(t, r, a, b)

	tests := []struct {
		name     string
		distro   string
		filename string
		want     bool
	}{
		{"matches in distro", "debian", a.Filename, true},
		{"distro scopes the match", "ubuntu", a.Filename, true},
		{"different distro with same filename does not bleed", "fedora", a.Filename, false},
		{"unknown filename returns false", "debian", "pool/bookworm/main/x/missing/missing_1.0_amd64.deb", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
				got, qerr := r.ExistsByFilenameConn(ctx, conn, tc.distro, tc.filename)
				if qerr != nil {
					return qerr
				}
				assert.Equal(t, tc.want, got)
				return nil
			})
			require.NoError(t, err)
		})
	}
}

func TestPoolRefs(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()

	a := samplePackage("debian", "bookworm", "main", "amd64", "alpha", "1.0")
	b := samplePackage("debian", "bookworm", "main", "amd64", "beta", "1.0")
	c := samplePackage("debian", "bullseye", "main", "amd64", "alpha", "1.0")
	d := samplePackage("ubuntu", "noble", "main", "amd64", "gamma", "1.0")
	insertAll(t, r, a, b, c, d)

	t.Run("unfiltered returns every distinct pool ref", func(t *testing.T) {
		refs, err := r.PoolRefs(ctx, Filter{})
		require.NoError(t, err)
		assert.Equal(t, []PoolRef{
			{Distro: "debian", Filename: a.Filename},
			{Distro: "debian", Filename: b.Filename},
			{Distro: "debian", Filename: c.Filename},
			{Distro: "ubuntu", Filename: d.Filename},
		}, refs)
	})

	t.Run("distro filter narrows the set", func(t *testing.T) {
		refs, err := r.PoolRefs(ctx, Filter{Distro: "ubuntu"})
		require.NoError(t, err)
		assert.Equal(t, []PoolRef{{Distro: "ubuntu", Filename: d.Filename}}, refs)
	})

	t.Run("distro+suite filter narrows further", func(t *testing.T) {
		refs, err := r.PoolRefs(ctx, Filter{Distro: "debian", Suite: "bullseye"})
		require.NoError(t, err)
		assert.Equal(t, []PoolRef{{Distro: "debian", Filename: c.Filename}}, refs)
	})

	t.Run("PoolRefsConn reads through the writer lock", func(t *testing.T) {
		var refs []PoolRef
		require.NoError(t, r.WithWriterLock(ctx, func(ctx context.Context, conn *sql.Conn) error {
			var qerr error
			refs, qerr = r.PoolRefsConn(ctx, conn, Filter{Distro: "debian", Suite: "bookworm"})
			return qerr
		}))
		assert.Equal(t, []PoolRef{
			{Distro: "debian", Filename: a.Filename},
			{Distro: "debian", Filename: b.Filename},
		}, refs)
	})
}

func TestDeleteRollsBackOnError(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()
	p := samplePackage("debian", "bookworm", "main", "amd64", "hello", "1.0")
	insertAll(t, r, p)

	sentinel := errors.New("rollback me")
	err := r.Tx(ctx, func(tx *sql.Tx) error {
		if _, derr := r.Delete(ctx, tx, Filter{Name: "hello"}); derr != nil {
			return derr
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	rows, err := r.List(ctx, Filter{})
	require.NoError(t, err)
	require.Len(t, rows, 1, "rolled-back delete must leave row in place")
}

func TestEnumerateForSuite(t *testing.T) {
	r := openTempRepo(t)
	ctx := context.Background()

	insertAll(t, r,
		samplePackage("debian", "bookworm", "main", "amd64", "hello", "1.0"),
		samplePackage("debian", "bookworm", "main", "arm64", "hello", "1.0"),
		samplePackage("debian", "bookworm", "main", "all", "docs", "1.0"),
		samplePackage("debian", "bookworm", "contrib", "amd64", "world", "1.0"),
		samplePackage("debian", "bullseye", "main", "amd64", "hello", "1.0"),
		samplePackage("ubuntu", "noble", "universe", "riscv64", "tools", "1.0"),
	)

	t.Run("returns distinct components and architectures for debian/bookworm", func(t *testing.T) {
		comps, arches, err := r.EnumerateForSuite(ctx, "debian", "bookworm")
		require.NoError(t, err)
		assert.Equal(t, []string{"contrib", "main"}, comps)
		assert.Equal(t, []string{"all", "amd64", "arm64"}, arches)
	})

	t.Run("scope filters to ubuntu/noble", func(t *testing.T) {
		comps, arches, err := r.EnumerateForSuite(ctx, "ubuntu", "noble")
		require.NoError(t, err)
		assert.Equal(t, []string{"universe"}, comps)
		assert.Equal(t, []string{"riscv64"}, arches)
	})

	t.Run("empty scope returns empty slices", func(t *testing.T) {
		comps, arches, err := r.EnumerateForSuite(ctx, "fedora", "rawhide")
		require.NoError(t, err)
		assert.Empty(t, comps)
		assert.Empty(t, arches)
	})
}
