package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blakesmith/ar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectFixtures(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		wantName string
		wantVer  string
		wantArch string
		wantSrc  string
	}{
		{
			name:     "gz with no Source field",
			file:     "hello-gz_1.0_amd64.deb",
			wantName: "hello-gz",
			wantVer:  "1.0",
			wantArch: "amd64",
			wantSrc:  "",
		},
		{
			name:     "xz with bare Source",
			file:     "hello-xz_1.0_amd64.deb",
			wantName: "hello-xz",
			wantVer:  "1.0",
			wantArch: "amd64",
			wantSrc:  "hello",
		},
		{
			name:     "zst with Source name (version)",
			file:     "hello-zst_1.0_amd64.deb",
			wantName: "hello-zst",
			wantVer:  "1.0",
			wantArch: "amd64",
			wantSrc:  "foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join("testdata", tt.file)
			info, err := Inspect(path)
			require.NoError(t, err)

			assert.Equal(t, tt.wantName, info.Name)
			assert.Equal(t, tt.wantVer, info.Version)
			assert.Equal(t, tt.wantArch, info.Architecture)
			assert.Equal(t, tt.wantSrc, info.Source)

			require.NotEmpty(t, info.Control)
			assert.Equal(t, byte('\n'), info.Control[len(info.Control)-1],
				"control must end with exactly one newline")
			assert.NotEqual(t, "\n\n", info.Control[len(info.Control)-2:],
				"control must not have multiple trailing newlines")

			fileSize := statSize(t, path)
			assert.Equal(t, fileSize, info.Size)

			gotMD5, gotSHA1, gotSHA256, gotSHA512 := rehash(t, path)
			assert.Equal(t, gotMD5, info.MD5, "md5 mismatch")
			assert.Equal(t, gotSHA1, info.SHA1, "sha1 mismatch")
			assert.Equal(t, gotSHA256, info.SHA256, "sha256 mismatch")
			assert.Equal(t, gotSHA512, info.SHA512, "sha512 mismatch")
		})
	}
}

func TestInspectControlIsCanonical(t *testing.T) {
	// hello-zst was built with trailing blank/whitespace lines so we can verify
	// normalisation produces canonical single-trailing-newline form.
	info, err := Inspect(filepath.Join("testdata", "hello-zst_1.0_amd64.deb"))
	require.NoError(t, err)

	want := "Package: hello-zst\n" +
		"Source: foo (1.2-3)\n" +
		"Version: 1.0\n" +
		"Architecture: amd64\n" +
		"Maintainer: repokit tests <test@example.invalid>\n" +
		"Description: hello-zst fixture\n"
	assert.Equal(t, want, info.Control)
}

func TestInspectMissing(t *testing.T) {
	_, err := Inspect(filepath.Join(t.TempDir(), "does-not-exist.deb"))
	require.Error(t, err)
}

func TestInspectMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.deb")
	require.NoError(t, os.WriteFile(path, []byte("not an ar archive"), 0o600))

	_, err := Inspect(path)
	require.Error(t, err)
}

func TestInspectMissingControl(t *testing.T) {
	// ar archive containing only debian-binary and a bogus data member; no
	// control.tar.* at all.
	path := filepath.Join(t.TempDir(), "no-control.deb")
	writeMinimalAr(t, path, [][2]string{
		{"debian-binary", "2.0\n"},
		{"data.tar.gz", "garbage"},
	})

	_, err := Inspect(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMissingControl)
}

func TestInspectMissingControlFile(t *testing.T) {
	// build a deb whose control.tar.gz is a valid tar but contains no `control`
	// file inside.
	path := filepath.Join(t.TempDir(), "empty-control.deb")
	writeDebWithControlBody(t, path, buildEmptyControlGz(t))

	_, err := Inspect(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMissingControlFile)
}

func TestInspectRejectsMultiStanzaControl(t *testing.T) {
	// A crafted .deb with an embedded blank line in its control file would
	// inject a second stanza into the published Packages index, letting the
	// attacker advertise extra packages with attacker-chosen Filename and
	// hashes. Inspect must reject before the bytes ever land in the DB.
	const evil = "Package: legit\nVersion: 1.0\nArchitecture: amd64\n" +
		"\n" +
		"Package: legit\nVersion: 1.0\nArchitecture: amd64\n" +
		"Filename: pool/m/innocuous/innocuous_1.0_amd64.deb\n" +
		"SHA256: 0000000000000000000000000000000000000000000000000000000000000000\n"
	path := filepath.Join(t.TempDir(), "multi-stanza.deb")
	writeDebWithControlBody(t, path, buildControlGz(t, evil))

	_, err := Inspect(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrControlMultiStanza)
}

func TestInspectRejectsOversizeControl(t *testing.T) {
	// Tar header advertises maxControlSize+1 bytes, capped before any
	// allocation. The body is short so the test stays cheap.
	path := filepath.Join(t.TempDir(), "huge-control.deb")
	gz := buildOversizeControlGz(t)
	writeDebWithControlBody(t, path, gz)

	_, err := Inspect(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrControlTooLarge)
}

// buildOversizeControlGz writes a control.tar.gz whose `./control` member
// header advertises maxControlSize+1 bytes. The actual body bytes are not
// shipped — Inspect must reject based on the header before reading.
func buildOversizeControlGz(t *testing.T) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	mtime := time.Unix(1700000000, 0).UTC()
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "./", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: mtime,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "./control", Mode: 0o644, Size: maxControlSize + 1,
		Typeflag: tar.TypeReg, ModTime: mtime,
	}))
	// Don't actually write maxControlSize+1 bytes — the size in the header
	// is what triggers the rejection. tar.Writer.Close will pad short writes
	// to match the declared size, so write a single byte and let tar pad.
	_, err := tw.Write([]byte{'P'})
	require.NoError(t, err)
	// Close will fail because we did not write the declared size; ignore the
	// error and use the partial bytes — the Inspect check happens before
	// archive/tar tries to read the body.
	_ = tw.Close()

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, err = gz.Write(tarBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	return gzBuf.Bytes()
}

func TestSafePathComponent(t *testing.T) {
	tests := []struct {
		name string
		in   string
		safe bool
	}{
		{"hello", "hello", true},
		{"hello-doc", "hello-doc", true},
		{"libfoo.bar", "libfoo.bar", true},
		{"1.0", "1.0", true},
		{"amd64", "amd64", true},
		{"empty", "", false},
		{"dot", ".", false},
		{"dotdot", "..", false},
		{"hidden", ".hidden", false},
		{"startswithdash", "-startswithdash", false},
		{"slash", "foo/bar", false},
		{"backslash", "foo\\bar", false},
		{"nul", "foo\x00bar", false},
		{"traversal", "../etc/passwd", false},
		{"tab", "foo\tbar", false},
		{"newline", "foo\nbar", false},
		{"esc", "foo\x1b[31mred", false},
		{"del", "foo\x7fbar", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.safe, safePathComponent(tt.in))
		})
	}
}

func TestInspectRejectsUnsafeFields(t *testing.T) {
	tests := []struct {
		name    string
		control string
	}{
		{
			name:    "name with slash",
			control: "Package: foo/../etc\nVersion: 1\nArchitecture: amd64\n",
		},
		{
			name:    "version with slash",
			control: "Package: foo\nVersion: 1/2\nArchitecture: amd64\n",
		},
		{
			name:    "architecture with traversal",
			control: "Package: foo\nVersion: 1\nArchitecture: ../../etc\n",
		},
		{
			name:    "source with backslash",
			control: "Package: foo\nVersion: 1\nArchitecture: amd64\nSource: bad\\name\n",
		},
		{
			name:    "version with tab",
			control: "Package: foo\nVersion: 1.0\tinjected\nArchitecture: amd64\n",
		},
		{
			name:    "architecture with ANSI escape",
			control: "Package: foo\nVersion: 1\nArchitecture: amd\x1b[31m64\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evil.deb")
			gz := buildControlGz(t, tt.control)
			writeDebWithControlBody(t, path, gz)

			_, err := Inspect(path)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnsafeField)
		})
	}
}

// buildControlGz wraps a control file body in a control.tar.gz for use by
// writeDebWithControlBody.
func buildControlGz(t *testing.T, control string) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	mtime := time.Unix(1700000000, 0).UTC()
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "./", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: mtime,
	}))
	body := []byte(control)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "./control", Mode: 0o644, Size: int64(len(body)),
		Typeflag: tar.TypeReg, ModTime: mtime,
	}))
	_, err := tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, err = gz.Write(tarBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	return gzBuf.Bytes()
}

func TestNormaliseControl(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"whitespace only collapses to empty", "   \n\n\t\n", ""},
		{"ensures single trailing newline", "Package: x\nVersion: 1\n\n\n", "Package: x\nVersion: 1\n"},
		{"adds missing trailing newline", "Package: x", "Package: x\n"},
		{"strips trailing whitespace per line", "Package: x   \nVersion: 1\t\n", "Package: x\nVersion: 1\n"},
		{"strips CR", "Package: x\r\nVersion: 1\r\n", "Package: x\nVersion: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normaliseControl(tt.in))
		})
	}
}

func TestParseControl(t *testing.T) {
	t.Run("valid stanza", func(t *testing.T) {
		stanza := "Package: hello\nVersion: 1.0\nArchitecture: amd64\nSource: foo (1.2-3)\nDescription: hi\n .\n"
		name, version, arch, source, err := parseControl(stanza)
		require.NoError(t, err)
		assert.Equal(t, "hello", name)
		assert.Equal(t, "1.0", version)
		assert.Equal(t, "amd64", arch)
		assert.Equal(t, "foo", source)
	})

	t.Run("missing Package", func(t *testing.T) {
		_, _, _, _, err := parseControl("Version: 1\nArchitecture: amd64\n")
		require.Error(t, err)
	})

	t.Run("missing Version", func(t *testing.T) {
		_, _, _, _, err := parseControl("Package: x\nArchitecture: amd64\n")
		require.Error(t, err)
	})

	t.Run("missing Architecture", func(t *testing.T) {
		_, _, _, _, err := parseControl("Package: x\nVersion: 1\n")
		require.Error(t, err)
	})

	t.Run("case-insensitive header lookup", func(t *testing.T) {
		stanza := "PACKAGE: x\nVERSION: 1\nARCHITECTURE: amd64\n"
		name, version, arch, _, err := parseControl(stanza)
		require.NoError(t, err)
		assert.Equal(t, "x", name)
		assert.Equal(t, "1", version)
		assert.Equal(t, "amd64", arch)
	})

	t.Run("source bare name preserved", func(t *testing.T) {
		stanza := "Package: x\nVersion: 1\nArchitecture: amd64\nSource: hello\n"
		_, _, _, source, err := parseControl(stanza)
		require.NoError(t, err)
		assert.Equal(t, "hello", source)
	})
}

func TestIsControlMember(t *testing.T) {
	cases := map[string]bool{
		"control.tar":         true,
		"control.tar.gz":      true,
		"control.tar.xz":      true,
		"control.tar.zst":     true,
		"control.tar.gz/":     true,
		"control.tar.xz//":    true,
		"data.tar.gz":         false,
		"debian-binary":       false,
		"control":             false,
		"control.tar.bz2":     false,
		"":                    false,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, isControlMember(name))
		})
	}
}

func statSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

func rehash(t *testing.T, path string) (string, string, string, string) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	md5h := md5.New()
	sha1h := sha1.New()
	sha256h := sha256.New()
	sha512h := sha512.New()
	hashes := []hash.Hash{md5h, sha1h, sha256h, sha512h}
	writers := make([]io.Writer, len(hashes))
	for i, h := range hashes {
		writers[i] = h
	}
	_, err = io.Copy(io.MultiWriter(writers...), f)
	require.NoError(t, err)

	return hex.EncodeToString(md5h.Sum(nil)),
		hex.EncodeToString(sha1h.Sum(nil)),
		hex.EncodeToString(sha256h.Sum(nil)),
		hex.EncodeToString(sha512h.Sum(nil))
}

func writeMinimalAr(t *testing.T, path string, members [][2]string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w := ar.NewWriter(f)
	require.NoError(t, w.WriteGlobalHeader())

	mtime := time.Unix(1700000000, 0).UTC()
	for _, m := range members {
		body := []byte(m[1])
		require.NoError(t, w.WriteHeader(&ar.Header{
			Name:    m[0],
			ModTime: mtime,
			Mode:    0o100644,
			Size:    int64(len(body)),
		}))
		_, err := w.Write(body)
		require.NoError(t, err)
	}
}

func buildEmptyControlGz(t *testing.T) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "./",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
		ModTime:  time.Unix(1700000000, 0).UTC(),
	}))
	require.NoError(t, tw.Close())

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	_, err := gz.Write(tarBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	return gzBuf.Bytes()
}

func writeDebWithControlBody(t *testing.T, path string, controlGz []byte) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	w := ar.NewWriter(f)
	require.NoError(t, w.WriteGlobalHeader())

	mtime := time.Unix(1700000000, 0).UTC()
	members := []struct {
		name string
		body []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", controlGz},
		{"data.tar.gz", []byte("not really tar")},
	}
	for _, m := range members {
		require.NoError(t, w.WriteHeader(&ar.Header{
			Name:    m.name,
			ModTime: mtime,
			Mode:    0o100644,
			Size:    int64(len(m.body)),
		}))
		_, err := w.Write(m.body)
		require.NoError(t, err)
	}
}
