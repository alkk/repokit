package index

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// ReleaseMeta is the suite-level metadata that becomes the Release file
// header: the values from the suite's config block plus the architectures
// and components present in the suite.
type ReleaseMeta struct {
	Origin        string
	Label         string
	Suite         string
	Codename      string
	Description   string
	Date          time.Time
	Architectures []string
	Components    []string
}

// releaseDateFormat renders a time as Debian's RFC 2822 layout. The reference
// "-0700" token always emits a numeric offset, so a UTC time produces "+0000"
// — apt rejects the literal "UTC".
const releaseDateFormat = "Mon, 02 Jan 2006 15:04:05 -0700"

// archAll is filtered out of the Release Architectures: line. arch=all rows
// are listed inside every concrete binary-<arch> Packages, and apt clients
// expect Architectures: to enumerate concrete arches only.
const archAll = "all"

// hashBlock pairs a Release checksum-block label with the accessor that picks
// the matching hash from a File.
type hashBlock struct {
	name string
	pick func(File) string
}

var hashBlocks = []hashBlock{
	{"MD5Sum", func(f File) string { return f.MD5 }},
	{"SHA1", func(f File) string { return f.SHA1 }},
	{"SHA256", func(f File) string { return f.SHA256 }},
	{"SHA512", func(f File) string { return f.SHA512 }},
}

// WriteRelease emits a Debian Release file for one (distro, suite) scope.
// Each File.Path inside files must already be relative to
// dists/<distro>/<suite>/ — callers prefix the component/binary-arch directory
// onto the basenames returned by WritePackagesAll before calling this.
func WriteRelease(meta ReleaseMeta, files []FileSet, w io.Writer) error {
	arches := ConcreteArches(meta.Architectures)
	if _, err := fmt.Fprintf(w,
		"Origin: %s\n"+
			"Label: %s\n"+
			"Suite: %s\n"+
			"Codename: %s\n"+
			"Description: %s\n"+
			"Date: %s\n"+
			"Architectures: %s\n"+
			"Components: %s\n"+
			"Acquire-By-Hash: no\n",
		meta.Origin, meta.Label, meta.Suite, meta.Codename, meta.Description,
		meta.Date.UTC().Format(releaseDateFormat),
		strings.Join(arches, " "),
		strings.Join(meta.Components, " "),
	); err != nil {
		return fmt.Errorf("writing Release header: %w", err)
	}
	return writeChecksumBlocks(w, files)
}

// ConcreteArches returns a copy of arches with "all" removed, preserving order.
// arch=all rows are merged into every concrete binary-<arch> Packages and
// must not appear on their own in the Release Architectures: line.
func ConcreteArches(arches []string) []string {
	out := make([]string, 0, len(arches))
	for _, a := range arches {
		if a == archAll {
			continue
		}
		out = append(out, a)
	}
	return out
}

// writeChecksumBlocks emits the four MD5Sum/SHA1/SHA256/SHA512 sections, each
// containing one " <hash> <size> <path>" line per file in iteration order.
func writeChecksumBlocks(w io.Writer, files []FileSet) error {
	for _, block := range hashBlocks {
		if _, err := fmt.Fprintf(w, "%s:\n", block.name); err != nil {
			return fmt.Errorf("writing %s header: %w", block.name, err)
		}
		for _, set := range files {
			for _, f := range set.All() {
				if _, err := fmt.Fprintf(w, " %s %d %s\n", block.pick(f), f.Size, f.Path); err != nil {
					return fmt.Errorf("writing %s entry for %s: %w", block.name, f.Path, err)
				}
			}
		}
	}
	return nil
}
