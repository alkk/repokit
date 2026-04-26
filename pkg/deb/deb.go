// Package deb extracts metadata from Debian binary packages.
package deb

import (
	"archive/tar"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/blakesmith/ar"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// PkgInfo is the metadata extracted from a .deb in a single read pass.
type PkgInfo struct {
	Name         string
	Version      string
	Architecture string
	Source       string
	Control      string
	Size         int64
	MD5          string
	SHA1         string
	SHA256       string
	SHA512       string
}

// ErrMissingControl indicates the .deb did not contain a control archive
// (control.tar or control.tar.{gz,xz,zst}).
var ErrMissingControl = errors.New("deb: missing control archive")

// ErrMissingControlFile indicates the control archive contained no `control`
// file at its root.
var ErrMissingControlFile = errors.New("deb: missing control file in control archive")

// ErrUnsafeField indicates a control field contains characters that would
// break out of the pool path (slashes, backslashes, NULs, or `..` segments).
var ErrUnsafeField = errors.New("deb: control field contains unsafe characters")

// ErrControlTooLarge indicates a .deb's control file exceeds maxControlSize
// and was rejected before being read into memory.
var ErrControlTooLarge = errors.New("deb: control file exceeds size limit")

// ErrControlMultiStanza indicates the control file contains an embedded blank
// line, which would split it into multiple stanzas when written verbatim into
// the Packages index. A crafted .deb could use that to inject extra package
// entries beyond the one being uploaded.
var ErrControlMultiStanza = errors.New("deb: control file contains embedded blank line (multi-stanza)")

// maxControlSize caps the bytes we will buffer from a control archive's
// control member. Real-world stanzas are well under 100 KiB; the cap keeps a
// crafted .deb from forcing an unbounded allocation during inspect.
const maxControlSize = 1 << 20

// srcVerSuffix matches the trailing ` (version)` of a Source field value.
var srcVerSuffix = regexp.MustCompile(` \(.*\)$`)

// Inspect parses a .deb at path and returns metadata. The full file is hashed
// with md5/sha1/sha256/sha512 in the same read pass as ar/tar parsing.
func Inspect(path string) (*PkgInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	md5h := md5.New()
	sha1h := sha1.New()
	sha256h := sha256.New()
	sha512h := sha512.New()
	tee := io.TeeReader(f, io.MultiWriter(md5h, sha1h, sha256h, sha512h))

	control, err := readDebControl(tee)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if _, drainErr := io.Copy(io.Discard, tee); drainErr != nil {
		return nil, fmt.Errorf("draining %s: %w", path, drainErr)
	}

	normalised := normaliseControl(control)
	if mErr := rejectMultiStanza(normalised); mErr != nil {
		return nil, fmt.Errorf("control from %s: %w", path, mErr)
	}
	name, version, arch, source, err := parseControl(normalised)
	if err != nil {
		return nil, fmt.Errorf("parsing control from %s: %w", path, err)
	}

	for field, val := range map[string]string{
		"Package": name, "Version": version, "Architecture": arch, "Source": source,
	} {
		if val == "" {
			continue
		}
		if !safePathComponent(val) {
			return nil, fmt.Errorf("%s=%q: %w", field, val, ErrUnsafeField)
		}
	}

	return &PkgInfo{
		Name:         name,
		Version:      version,
		Architecture: arch,
		Source:       source,
		Control:      normalised,
		Size:         stat.Size(),
		MD5:          hex.EncodeToString(md5h.Sum(nil)),
		SHA1:         hex.EncodeToString(sha1h.Sum(nil)),
		SHA256:       hex.EncodeToString(sha256h.Sum(nil)),
		SHA512:       hex.EncodeToString(sha512h.Sum(nil)),
	}, nil
}

// readDebControl walks an ar archive and returns the verbatim control file
// from the first control.tar member it encounters. Subsequent ar members are
// drained to keep the underlying reader positioned at EOF.
func readDebControl(r io.Reader) (string, error) {
	arRdr := ar.NewReader(r)
	var control string
	var found bool
	for {
		hdr, err := arRdr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading ar member: %w", err)
		}
		if found || !isControlMember(hdr.Name) {
			continue
		}
		control, err = readControlArchive(arRdr, hdr.Name)
		if err != nil {
			return "", err
		}
		found = true
	}
	if !found {
		return "", ErrMissingControl
	}
	return control, nil
}

// isControlMember reports whether name matches one of the recognized control
// archive member names. ar names may be padded with trailing `/` (BSD form).
func isControlMember(name string) bool {
	switch strings.TrimRight(name, "/") {
	case "control.tar", "control.tar.gz", "control.tar.xz", "control.tar.zst":
		return true
	}
	return false
}

// readControlArchive decompresses the named control archive and extracts the
// `control` file's contents.
func readControlArchive(r io.Reader, memberName string) (string, error) {
	decompressed, closeFn, err := openControlReader(r, memberName)
	if err != nil {
		return "", fmt.Errorf("decompressing %s: %w", memberName, err)
	}
	if closeFn != nil {
		defer closeFn()
	}

	tr := tar.NewReader(decompressed)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", ErrMissingControlFile
		}
		if err != nil {
			return "", fmt.Errorf("reading control tar: %w", err)
		}
		if strings.TrimPrefix(hdr.Name, "./") != "control" {
			continue
		}
		if hdr.Size > maxControlSize {
			return "", fmt.Errorf("%w: %d > %d bytes", ErrControlTooLarge, hdr.Size, maxControlSize)
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxControlSize+1))
		if err != nil {
			return "", fmt.Errorf("reading control file body: %w", err)
		}
		if int64(len(body)) > maxControlSize {
			return "", fmt.Errorf("%w: read more than %d bytes", ErrControlTooLarge, maxControlSize)
		}
		return string(body), nil
	}
}

// openControlReader returns a decompressed reader for the named control
// member. The returned closer (when non-nil) must be called when finished.
func openControlReader(r io.Reader, memberName string) (io.Reader, func(), error) {
	switch strings.TrimRight(memberName, "/") {
	case "control.tar":
		return r, nil, nil
	case "control.tar.gz":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gz, func() { _ = gz.Close() }, nil
	case "control.tar.xz":
		x, err := xz.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("xz reader: %w", err)
		}
		return x, nil, nil
	case "control.tar.zst":
		z, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd reader: %w", err)
		}
		return z, z.Close, nil
	}
	return nil, nil, fmt.Errorf("unsupported control member %q", memberName)
}

// safePathComponent rejects values that would let a malicious .deb escape the
// pool root or smuggle terminal-control bytes into operator-facing output:
// slashes, backslashes, leading dot or hyphen, dot/dot-dot segments, any
// C0 control byte (0x00–0x1F), space (0x20), or DEL (0x7F). The control-byte
// check covers NUL implicitly and blocks tabs/newlines/escape sequences that
// would otherwise be printed verbatim by commands like `info` (terminal
// injection) and produce hard-to-handle on-disk filenames. Space is rejected
// because Debian Policy forbids it in package, source, version, and arch
// fields, and allowing it would create pool paths that break naive shell
// pipelines and apt clients that don't quote paths.
func safePathComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if s[0] == '.' || s[0] == '-' {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch c {
		case '/', '\\':
			return false
		}
		if c <= 0x20 || c == 0x7F {
			return false
		}
	}
	return true
}

// rejectMultiStanza fails when the normalised control text has any blank line
// before the trailing newline. WritePackages writes Control verbatim followed
// by Filename/Size/hash trailers and a blank-line separator, so an embedded
// blank line would split one upload into multiple Packages stanzas — letting
// a crafted .deb advertise additional packages with attacker-chosen Filename
// and hashes.
func rejectMultiStanza(normalised string) error {
	body := strings.TrimSuffix(normalised, "\n")
	if strings.Contains(body, "\n\n") {
		return ErrControlMultiStanza
	}
	return nil
}

// normaliseControl strips trailing whitespace from each line and trailing
// blank lines, then ensures exactly one trailing newline. The result is the
// canonical form stored in the database and re-emitted on publish.
func normaliseControl(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	end := len(lines)
	for end > 0 && lines[end-1] == "" {
		end--
	}
	if end == 0 {
		return ""
	}
	return strings.Join(lines[:end], "\n") + "\n"
}

// parseControl extracts Package, Version, Architecture, and Source from a
// normalised control stanza. Source is reduced to its bare name when the
// value is in `name (version)` form. Continuation lines (indented) are
// ignored for header lookup.
func parseControl(stanza string) (name, version, arch, source string, err error) {
	for line := range strings.SplitSeq(stanza, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := line[:idx]
		val := strings.TrimSpace(line[idx+1:])
		switch {
		case strings.EqualFold(key, "Package"):
			name = val
		case strings.EqualFold(key, "Version"):
			version = val
		case strings.EqualFold(key, "Architecture"):
			arch = val
		case strings.EqualFold(key, "Source"):
			source = val
		}
	}

	switch {
	case name == "":
		return "", "", "", "", errors.New("control: missing Package")
	case version == "":
		return "", "", "", "", errors.New("control: missing Version")
	case arch == "":
		return "", "", "", "", errors.New("control: missing Architecture")
	}

	if source != "" {
		source = srcVerSuffix.ReplaceAllString(source, "")
	}
	return name, version, arch, source, nil
}
