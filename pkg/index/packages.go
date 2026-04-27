// Package index generates Debian apt repository index files. The Packages
// writer produces both the plain text index and its compressed companions
// (gz, xz) in a single fan-out pass while collecting size and four hashes
// for each output file so they can be referenced verbatim in Release.
package index

import (
	"compress/gzip"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/ulikunitz/xz"

	"github.com/alkk/repokit/pkg/pool"
	"github.com/alkk/repokit/pkg/repo"
)

// File holds the size and four hashes of one written index file. Path is the
// file's basename relative to the directory it was written into; callers that
// need a path relative to dists/<distro>/<suite>/ for Release composition
// prefix it with the appropriate component/binary-arch path.
type File struct {
	Path   string
	Size   int64
	MD5    string
	SHA1   string
	SHA256 string
	SHA512 string
}

// FileSet is the trio of index files produced for one (component, architecture)
// pair: the plain Packages and its gzip/xz companions.
type FileSet struct {
	Plain File
	GZ    File
	XZ    File
}

// All returns the three Files in the fixed order plain, gz, xz so callers can
// iterate the set without coupling to field names.
func (s FileSet) All() []File {
	return []File{s.Plain, s.GZ, s.XZ}
}

const (
	packagesName   = "Packages"
	packagesGzName = "Packages.gz"
	packagesXzName = "Packages.xz"
)

// WritePackages emits rows as a Debian Packages file. Each stanza is the row's
// normalised control text (already terminated by a single newline), followed
// by Filename, Size, MD5sum, SHA1, SHA256, and SHA512 fields and a blank-line
// separator before the next stanza.
func WritePackages(rows []repo.Package, w io.Writer) error {
	for i := range rows {
		p := &rows[i]
		if _, err := io.WriteString(w, p.Control); err != nil {
			return fmt.Errorf("writing control for %s: %w", p.Name, err)
		}
		if _, err := fmt.Fprintf(w,
			"Filename: %s\nSize: %d\nMD5sum: %s\nSHA1: %s\nSHA256: %s\nSHA512: %s\n\n",
			p.Filename, p.Size, p.MD5, p.SHA1, p.SHA256, p.SHA512); err != nil {
			return fmt.Errorf("writing trailer for %s: %w", p.Name, err)
		}
	}
	return nil
}

// WritePackagesAll writes Packages, Packages.gz, and Packages.xz into dir
// using a single fan-out pass. The returned FileSet captures each output's
// basename, size on disk, and four hashes; callers compose the Release-relative
// path by prefixing with the component/binary-arch directory.
func WritePackagesAll(rows []repo.Package, dir string) (FileSet, error) {
	if err := pool.MkdirAllSync(dir); err != nil {
		return FileSet{}, fmt.Errorf("creating %s: %w", dir, err)
	}

	plain, err := newSink(filepath.Join(dir, packagesName))
	if err != nil {
		return FileSet{}, err
	}
	defer plain.cleanup()

	gz, err := newSink(filepath.Join(dir, packagesGzName))
	if err != nil {
		return FileSet{}, err
	}
	defer gz.cleanup()

	xzs, err := newSink(filepath.Join(dir, packagesXzName))
	if err != nil {
		return FileSet{}, err
	}
	defer xzs.cleanup()

	gzWriter := gzip.NewWriter(gz)
	xzWriter, xzErr := xz.NewWriter(xzs)
	if xzErr != nil {
		return FileSet{}, fmt.Errorf("creating xz writer: %w", xzErr)
	}

	fan := io.MultiWriter(plain, gzWriter, xzWriter)
	if writeErr := WritePackages(rows, fan); writeErr != nil {
		return FileSet{}, writeErr
	}

	if closeErr := gzWriter.Close(); closeErr != nil {
		return FileSet{}, fmt.Errorf("closing gzip writer: %w", closeErr)
	}
	if closeErr := xzWriter.Close(); closeErr != nil {
		return FileSet{}, fmt.Errorf("closing xz writer: %w", closeErr)
	}

	plainMeta, err := plain.finalize(packagesName)
	if err != nil {
		return FileSet{}, err
	}
	gzMeta, err := gz.finalize(packagesGzName)
	if err != nil {
		return FileSet{}, err
	}
	xzMeta, err := xzs.finalize(packagesXzName)
	if err != nil {
		return FileSet{}, err
	}

	return FileSet{Plain: plainMeta, GZ: gzMeta, XZ: xzMeta}, nil
}

// sink is one of the three index-file write targets: the on-disk file plus
// running hashes of every byte that lands in it.
type sink struct {
	path    string
	file    *os.File
	tee     io.Writer
	md5     hash.Hash
	sha1    hash.Hash
	sha256  hash.Hash
	sha512  hash.Hash
	success bool
}

func newSink(path string) (*sink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	s := &sink{
		path:   path,
		file:   f,
		md5:    md5.New(),
		sha1:   sha1.New(),
		sha256: sha256.New(),
		sha512: sha512.New(),
	}
	s.tee = io.MultiWriter(f, s.md5, s.sha1, s.sha256, s.sha512)
	return s, nil
}

// Write fans payload into the underlying file and the four running hashes.
func (s *sink) Write(p []byte) (int, error) {
	return s.tee.Write(p) //nolint:wrapcheck // hash and *os.File errors propagate verbatim; finalize wraps with context
}

// cleanup is registered via defer. When finalize succeeded it is a no-op; when
// the caller bailed early it removes the partial file so the dir is left tidy.
func (s *sink) cleanup() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	if !s.success {
		_ = os.Remove(s.path)
	}
}

// finalize syncs and closes the file, then materializes the File metadata.
// After it returns successfully cleanup becomes a no-op.
func (s *sink) finalize(name string) (File, error) {
	f := s.file
	s.file = nil
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return File{}, fmt.Errorf("fsync %s: %w", name, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return File{}, fmt.Errorf("stat %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return File{}, fmt.Errorf("closing %s: %w", name, err)
	}
	s.success = true
	return File{
		Path:   name,
		Size:   info.Size(),
		MD5:    hex.EncodeToString(s.md5.Sum(nil)),
		SHA1:   hex.EncodeToString(s.sha1.Sum(nil)),
		SHA256: hex.EncodeToString(s.sha256.Sum(nil)),
		SHA512: hex.EncodeToString(s.sha512.Sum(nil)),
	}, nil
}
