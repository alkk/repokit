package index

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedDate is the Date: value used by every Release golden test. UTC ensures
// the formatted offset is "+0000" regardless of the host timezone.
var fixedDate = time.Date(2026, time.April, 25, 12, 0, 0, 0, time.UTC)

// sampleFile builds a deterministic File for golden assertions. The hashes
// vary by basename so tests can spot mis-routed entries.
func sampleFile(path string, size int64, seed string) File {
	return File{
		Path:   path,
		Size:   size,
		MD5:    strings.Repeat(seed, 32)[:32],
		SHA1:   strings.Repeat(seed, 40)[:40],
		SHA256: strings.Repeat(seed, 64)[:64],
		SHA512: strings.Repeat(seed, 128)[:128],
	}
}

// sampleFileSet builds a FileSet whose three entries are pre-prefixed with the
// component/binary-arch directory so they are ready for Release composition.
func sampleFileSet(component, arch, seed string) FileSet {
	prefix := component + "/binary-" + arch + "/"
	return FileSet{
		Plain: sampleFile(prefix+packagesName, 100, seed),
		GZ:    sampleFile(prefix+packagesGzName, 50, seed),
		XZ:    sampleFile(prefix+packagesXzName, 40, seed),
	}
}

func TestWriteRelease_Golden(t *testing.T) {
	meta := ReleaseMeta{
		Origin:        "MyRepo",
		Label:         "MyRepo",
		Suite:         "bookworm",
		Codename:      "bookworm",
		Description:   "Internal builds",
		Date:          fixedDate,
		Architectures: []string{"amd64", "arm64"},
		Components:    []string{"main", "contrib"},
	}
	files := []FileSet{
		sampleFileSet("main", "amd64", "a"),
		sampleFileSet("main", "arm64", "b"),
		sampleFileSet("contrib", "amd64", "c"),
	}

	var buf bytes.Buffer
	require.NoError(t, WriteRelease(meta, files, &buf))

	want := "Origin: MyRepo\n" +
		"Label: MyRepo\n" +
		"Suite: bookworm\n" +
		"Codename: bookworm\n" +
		"Description: Internal builds\n" +
		"Date: Sat, 25 Apr 2026 12:00:00 +0000\n" +
		"Architectures: amd64 arm64\n" +
		"Components: main contrib\n" +
		"Acquire-By-Hash: no\n" +
		"MD5Sum:\n" +
		" " + strings.Repeat("a", 32) + " 100 main/binary-amd64/Packages\n" +
		" " + strings.Repeat("a", 32) + " 50 main/binary-amd64/Packages.gz\n" +
		" " + strings.Repeat("a", 32) + " 40 main/binary-amd64/Packages.xz\n" +
		" " + strings.Repeat("b", 32) + " 100 main/binary-arm64/Packages\n" +
		" " + strings.Repeat("b", 32) + " 50 main/binary-arm64/Packages.gz\n" +
		" " + strings.Repeat("b", 32) + " 40 main/binary-arm64/Packages.xz\n" +
		" " + strings.Repeat("c", 32) + " 100 contrib/binary-amd64/Packages\n" +
		" " + strings.Repeat("c", 32) + " 50 contrib/binary-amd64/Packages.gz\n" +
		" " + strings.Repeat("c", 32) + " 40 contrib/binary-amd64/Packages.xz\n" +
		"SHA1:\n" +
		" " + strings.Repeat("a", 40) + " 100 main/binary-amd64/Packages\n" +
		" " + strings.Repeat("a", 40) + " 50 main/binary-amd64/Packages.gz\n" +
		" " + strings.Repeat("a", 40) + " 40 main/binary-amd64/Packages.xz\n" +
		" " + strings.Repeat("b", 40) + " 100 main/binary-arm64/Packages\n" +
		" " + strings.Repeat("b", 40) + " 50 main/binary-arm64/Packages.gz\n" +
		" " + strings.Repeat("b", 40) + " 40 main/binary-arm64/Packages.xz\n" +
		" " + strings.Repeat("c", 40) + " 100 contrib/binary-amd64/Packages\n" +
		" " + strings.Repeat("c", 40) + " 50 contrib/binary-amd64/Packages.gz\n" +
		" " + strings.Repeat("c", 40) + " 40 contrib/binary-amd64/Packages.xz\n" +
		"SHA256:\n" +
		" " + strings.Repeat("a", 64) + " 100 main/binary-amd64/Packages\n" +
		" " + strings.Repeat("a", 64) + " 50 main/binary-amd64/Packages.gz\n" +
		" " + strings.Repeat("a", 64) + " 40 main/binary-amd64/Packages.xz\n" +
		" " + strings.Repeat("b", 64) + " 100 main/binary-arm64/Packages\n" +
		" " + strings.Repeat("b", 64) + " 50 main/binary-arm64/Packages.gz\n" +
		" " + strings.Repeat("b", 64) + " 40 main/binary-arm64/Packages.xz\n" +
		" " + strings.Repeat("c", 64) + " 100 contrib/binary-amd64/Packages\n" +
		" " + strings.Repeat("c", 64) + " 50 contrib/binary-amd64/Packages.gz\n" +
		" " + strings.Repeat("c", 64) + " 40 contrib/binary-amd64/Packages.xz\n" +
		"SHA512:\n" +
		" " + strings.Repeat("a", 128) + " 100 main/binary-amd64/Packages\n" +
		" " + strings.Repeat("a", 128) + " 50 main/binary-amd64/Packages.gz\n" +
		" " + strings.Repeat("a", 128) + " 40 main/binary-amd64/Packages.xz\n" +
		" " + strings.Repeat("b", 128) + " 100 main/binary-arm64/Packages\n" +
		" " + strings.Repeat("b", 128) + " 50 main/binary-arm64/Packages.gz\n" +
		" " + strings.Repeat("b", 128) + " 40 main/binary-arm64/Packages.xz\n" +
		" " + strings.Repeat("c", 128) + " 100 contrib/binary-amd64/Packages\n" +
		" " + strings.Repeat("c", 128) + " 50 contrib/binary-amd64/Packages.gz\n" +
		" " + strings.Repeat("c", 128) + " 40 contrib/binary-amd64/Packages.xz\n"

	assert.Equal(t, want, buf.String())
}

func TestWriteRelease_ArchitecturesExcludesAll(t *testing.T) {
	meta := ReleaseMeta{
		Origin:        "r", Label: "r", Suite: "s", Codename: "s", Description: "d",
		Date:          fixedDate,
		Architectures: []string{"all", "amd64", "arm64"},
		Components:    []string{"main"},
	}

	var buf bytes.Buffer
	require.NoError(t, WriteRelease(meta, nil, &buf))

	out := buf.String()
	assert.Contains(t, out, "Architectures: amd64 arm64\n")
	assert.NotContains(t, out, "Architectures: all")
	assert.NotContains(t, out, " all ")
}

func TestWriteRelease_DateUsesNumericOffsetNotUTC(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	meta := ReleaseMeta{
		Origin: "r", Label: "r", Suite: "s", Codename: "s", Description: "d",
		Date:          time.Date(2026, time.April, 25, 8, 0, 0, 0, loc),
		Architectures: []string{"amd64"},
		Components:    []string{"main"},
	}

	var buf bytes.Buffer
	require.NoError(t, WriteRelease(meta, nil, &buf))

	out := buf.String()
	assert.Contains(t, out, "Date: Sat, 25 Apr 2026 12:00:00 +0000\n",
		"non-UTC inputs must be normalized to UTC and rendered with +0000")
	assert.NotContains(t, out, "UTC", "literal UTC is rejected by apt")
	assert.NotContains(t, out, "EDT", "host timezone abbreviations must not leak through")
}

func TestWriteRelease_AcquireByHashIsNo(t *testing.T) {
	meta := ReleaseMeta{
		Origin: "r", Label: "r", Suite: "s", Codename: "s", Description: "d",
		Date:          fixedDate,
		Architectures: []string{"amd64"},
		Components:    []string{"main"},
	}

	var buf bytes.Buffer
	require.NoError(t, WriteRelease(meta, nil, &buf))

	assert.Contains(t, buf.String(), "Acquire-By-Hash: no\n")
}

func TestWriteRelease_RoundTripParse(t *testing.T) {
	meta := ReleaseMeta{
		Origin:        "MyRepo",
		Label:         "MyLabel",
		Suite:         "bookworm",
		Codename:      "bookworm",
		Description:   "round-trip",
		Date:          fixedDate,
		Architectures: []string{"amd64", "arm64"},
		Components:    []string{"main", "contrib"},
	}
	files := []FileSet{
		sampleFileSet("main", "amd64", "a"),
		sampleFileSet("contrib", "arm64", "b"),
	}

	var buf bytes.Buffer
	require.NoError(t, WriteRelease(meta, files, &buf))

	header, blocks := parseRelease(t, buf.Bytes())

	assert.Equal(t, "MyRepo", header["Origin"])
	assert.Equal(t, "MyLabel", header["Label"])
	assert.Equal(t, "bookworm", header["Suite"])
	assert.Equal(t, "bookworm", header["Codename"])
	assert.Equal(t, "round-trip", header["Description"])
	assert.Equal(t, "Sat, 25 Apr 2026 12:00:00 +0000", header["Date"])
	assert.Equal(t, "amd64 arm64", header["Architectures"])
	assert.Equal(t, "main contrib", header["Components"])
	assert.Equal(t, "no", header["Acquire-By-Hash"])

	for _, name := range []string{"MD5Sum", "SHA1", "SHA256", "SHA512"} {
		require.Lenf(t, blocks[name], 6, "%s should list 2 FileSets × 3 files = 6 entries", name)
	}

	first := blocks["SHA256"][0]
	assert.Equal(t, strings.Repeat("a", 64), first.hash)
	assert.Equal(t, int64(100), first.size)
	assert.Equal(t, "main/binary-amd64/Packages", first.path)
}

func TestWriteRelease_EmptyFiles(t *testing.T) {
	meta := ReleaseMeta{
		Origin: "r", Label: "r", Suite: "s", Codename: "s", Description: "d",
		Date:          fixedDate,
		Architectures: []string{"amd64"},
		Components:    []string{"main"},
	}

	var buf bytes.Buffer
	require.NoError(t, WriteRelease(meta, nil, &buf))

	out := buf.String()
	for _, block := range []string{"MD5Sum:\n", "SHA1:\n", "SHA256:\n", "SHA512:\n"} {
		assert.Contains(t, out, block)
	}
	// No checksum entry lines when files is empty — every space-prefixed line
	// would be a continuation of one of the four block headers.
	for line := range strings.SplitSeq(out, "\n") {
		assert.Falsef(t, strings.HasPrefix(line, " "),
			"unexpected continuation line %q in empty-files Release", line)
	}
}

func TestWriteRelease_PropagatesWriterError(t *testing.T) {
	meta := ReleaseMeta{
		Origin: "r", Label: "r", Suite: "s", Codename: "s", Description: "d",
		Date:          fixedDate,
		Architectures: []string{"amd64"},
		Components:    []string{"main"},
	}
	require.Error(t, WriteRelease(meta, nil, failingWriter{}))
}

// checksumEntry is a parsed " <hash> <size> <path>" line.
type checksumEntry struct {
	hash string
	size int64
	path string
}

// parseRelease splits a Release file into its header map and per-block list of
// checksum entries. It is intentionally minimal — sufficient for round-trip
// assertions in tests.
func parseRelease(t *testing.T, data []byte) (map[string]string, map[string][]checksumEntry) {
	t.Helper()
	header := map[string]string{}
	blocks := map[string][]checksumEntry{}
	var currentBlock string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") {
			require.NotEmptyf(t, currentBlock, "continuation line outside any block: %q", line)
			fields := strings.Fields(line)
			require.Lenf(t, fields, 3, "checksum line must have hash size path: %q", line)
			var size int64
			_, err := fmt.Sscanf(fields[1], "%d", &size)
			require.NoErrorf(t, err, "parsing size in %q", line)
			blocks[currentBlock] = append(blocks[currentBlock], checksumEntry{
				hash: fields[0], size: size, path: fields[2],
			})
			continue
		}
		idx := strings.Index(line, ":")
		require.GreaterOrEqualf(t, idx, 1, "header line missing colon: %q", line)
		key := line[:idx]
		val := strings.TrimSpace(line[idx+1:])
		if val == "" {
			currentBlock = key
			continue
		}
		header[key] = val
		currentBlock = ""
	}
	return header, blocks
}
