package index

import (
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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"

	"github.com/netxms/debrepo/pkg/repo"
)

func samplePackage(name string) repo.Package {
	return repo.Package{
		Distro:       "debian",
		Suite:        "bookworm",
		Component:    "main",
		Architecture: "amd64",
		Name:         name,
		Version:      "1.0",
		Filename:     "pool/debian/bookworm/main/" + name[:1] + "/" + name + "/" + name + "_1.0_amd64.deb",
		Size:         1234,
		MD5:          "0123456789abcdef0123456789abcdef",
		SHA1:         "0123456789abcdef0123456789abcdef01234567",
		SHA256:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SHA512: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" +
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Control: "Package: " + name + "\nVersion: 1.0\nArchitecture: amd64\n" +
			"Maintainer: Test <test@example.com>\nDescription: sample " + name + "\n",
	}
}

func TestWritePackages_GoldenSingleStanza(t *testing.T) {
	p := samplePackage("hello")

	var buf bytes.Buffer
	require.NoError(t, WritePackages([]repo.Package{p}, &buf))

	want := "Package: hello\n" +
		"Version: 1.0\n" +
		"Architecture: amd64\n" +
		"Maintainer: Test <test@example.com>\n" +
		"Description: sample hello\n" +
		"Filename: pool/debian/bookworm/main/h/hello/hello_1.0_amd64.deb\n" +
		"Size: 1234\n" +
		"MD5sum: 0123456789abcdef0123456789abcdef\n" +
		"SHA1: 0123456789abcdef0123456789abcdef01234567\n" +
		"SHA256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n" +
		"SHA512: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n" +
		"\n"

	assert.Equal(t, want, buf.String())
}

func TestWritePackages_StanzaSeparator(t *testing.T) {
	rows := []repo.Package{samplePackage("alpha"), samplePackage("bravo")}

	var buf bytes.Buffer
	require.NoError(t, WritePackages(rows, &buf))

	out := buf.String()
	assert.Contains(t, out, "\n\nPackage: bravo\n",
		"stanzas must be separated by exactly one blank line")
	assert.True(t, strings.HasSuffix(out, "\n\n"),
		"output must end with the trailing blank-line separator")
	assert.NotContains(t, out, "\n\n\n",
		"no triple newlines should appear")
}

func TestWritePackages_EmptyRows(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WritePackages(nil, &buf))
	assert.Empty(t, buf.Bytes())
}

func TestWritePackages_PropagatesWriterError(t *testing.T) {
	err := WritePackages([]repo.Package{samplePackage("hello")}, failingWriter{})
	require.Error(t, err)
}

func TestWritePackagesAll_FilesAndHashesMatch(t *testing.T) {
	dir := t.TempDir()
	rows := []repo.Package{samplePackage("alpha"), samplePackage("bravo")}

	set, err := WritePackagesAll(rows, dir)
	require.NoError(t, err)

	for _, f := range set.All() {
		full := filepath.Join(dir, f.Path)
		bs, readErr := os.ReadFile(full)
		require.NoErrorf(t, readErr, "reading %s", f.Path)
		require.Equalf(t, f.Size, int64(len(bs)),
			"FileSet size mismatch for %s", f.Path)
		assert.Equalf(t, f.MD5, hashHex(md5.New(), bs), "MD5 mismatch for %s", f.Path)
		assert.Equalf(t, f.SHA1, hashHex(sha1.New(), bs), "SHA1 mismatch for %s", f.Path)
		assert.Equalf(t, f.SHA256, hashHex(sha256.New(), bs), "SHA256 mismatch for %s", f.Path)
		assert.Equalf(t, f.SHA512, hashHex(sha512.New(), bs), "SHA512 mismatch for %s", f.Path)
	}

	assert.Equal(t, packagesName, set.Plain.Path)
	assert.Equal(t, packagesGzName, set.GZ.Path)
	assert.Equal(t, packagesXzName, set.XZ.Path)
}

func TestWritePackagesAll_GzAndXzDecompressIdentical(t *testing.T) {
	dir := t.TempDir()
	rows := []repo.Package{samplePackage("alpha"), samplePackage("bravo")}

	_, err := WritePackagesAll(rows, dir)
	require.NoError(t, err)

	plain, err := os.ReadFile(filepath.Join(dir, packagesName))
	require.NoError(t, err)

	gzBytes, err := os.ReadFile(filepath.Join(dir, packagesGzName))
	require.NoError(t, err)
	gzReader, err := gzip.NewReader(bytes.NewReader(gzBytes))
	require.NoError(t, err)
	defer gzReader.Close()
	gzDecoded, err := io.ReadAll(gzReader)
	require.NoError(t, err)
	assert.Equal(t, plain, gzDecoded, "Packages.gz must decompress to byte-identical Packages")

	xzBytes, err := os.ReadFile(filepath.Join(dir, packagesXzName))
	require.NoError(t, err)
	xzReader, err := xz.NewReader(bytes.NewReader(xzBytes))
	require.NoError(t, err)
	xzDecoded, err := io.ReadAll(xzReader)
	require.NoError(t, err)
	assert.Equal(t, plain, xzDecoded, "Packages.xz must decompress to byte-identical Packages")
}

func TestWritePackagesAll_RoundTripParsesAllRows(t *testing.T) {
	dir := t.TempDir()
	rows := []repo.Package{samplePackage("alpha"), samplePackage("bravo"), samplePackage("charlie")}

	_, err := WritePackagesAll(rows, dir)
	require.NoError(t, err)

	plain, err := os.ReadFile(filepath.Join(dir, packagesName))
	require.NoError(t, err)

	stanzas := splitStanzas(plain)
	require.Len(t, stanzas, len(rows))
	for i, want := range rows {
		fields := parseStanza(stanzas[i])
		assert.Equal(t, want.Name, fields["Package"], "stanza %d Package", i)
		assert.Equal(t, want.Version, fields["Version"], "stanza %d Version", i)
		assert.Equal(t, want.Filename, fields["Filename"], "stanza %d Filename", i)
		assert.Equal(t, want.SHA256, fields["SHA256"], "stanza %d SHA256", i)
	}
}

func TestWritePackagesAll_EmptyRowsProducesEmptyPlain(t *testing.T) {
	dir := t.TempDir()

	set, err := WritePackagesAll(nil, dir)
	require.NoError(t, err)

	plain, err := os.ReadFile(filepath.Join(dir, packagesName))
	require.NoError(t, err)
	assert.Empty(t, plain)
	assert.Equal(t, int64(0), set.Plain.Size)
	// gz and xz still emit their framing headers, so they are non-empty even
	// for zero-row input — verify the recorded size matches what landed on disk.
	for _, f := range []File{set.GZ, set.XZ} {
		bs, readErr := os.ReadFile(filepath.Join(dir, f.Path))
		require.NoError(t, readErr)
		assert.Equal(t, f.Size, int64(len(bs)))
	}
}

func TestWritePackagesAll_MkdirCreatesMissingDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "deep", "nested", "binary-amd64")

	_, err := WritePackagesAll([]repo.Package{samplePackage("hello")}, dir)
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.FileExists(t, filepath.Join(dir, packagesName))
}

// hashHex feeds data through h and returns the lowercase hex digest.
func hashHex(h hash.Hash, data []byte) string {
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// splitStanzas slices a Packages file body into per-stanza byte slices,
// dropping the trailing blank line each stanza terminator leaves behind.
func splitStanzas(b []byte) [][]byte {
	trimmed := bytes.TrimRight(b, "\n")
	if len(trimmed) == 0 {
		return nil
	}
	parts := bytes.Split(trimmed, []byte("\n\n"))
	out := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// parseStanza turns one Debian-control stanza into a key→value map. Continuation
// lines (leading whitespace) are appended to the previous key with a single
// space. Sufficient for round-trip assertions in tests.
func parseStanza(stanza []byte) map[string]string {
	out := map[string]string{}
	var lastKey string
	for line := range strings.SplitSeq(string(stanza), "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if lastKey != "" {
				out[lastKey] += " " + strings.TrimSpace(line)
			}
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := line[:idx]
		val := strings.TrimSpace(line[idx+1:])
		out[key] = val
		lastKey = key
	}
	return out
}

// failingWriter returns a non-nil error on every Write to verify error paths.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrShortWrite
}
