//go:build ignore

// gen builds the .deb test fixtures used by pkg/deb.
//
// Run from the testdata directory:
//
//	go run gen.go
//
// It produces hello-gz_1.0_amd64.deb, hello-xz_1.0_amd64.deb, and
// hello-zst_1.0_amd64.deb. Each varies in control.tar compression and in
// the shape of the Source field so the parser tests cover the full matrix.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/blakesmith/ar"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

type fixture struct {
	file       string
	control    string
	controlExt string
}

func main() {
	fixtures := []fixture{
		{
			file:       "hello-gz_1.0_amd64.deb",
			controlExt: "gz",
			control: "Package: hello-gz\n" +
				"Version: 1.0\n" +
				"Architecture: amd64\n" +
				"Maintainer: repokit tests <test@example.invalid>\n" +
				"Description: hello-gz fixture\n" +
				" Multi-line description body for normalisation tests.\n",
		},
		{
			file:       "hello-xz_1.0_amd64.deb",
			controlExt: "xz",
			control: "Package: hello-xz\n" +
				"Source: hello\n" +
				"Version: 1.0\n" +
				"Architecture: amd64\n" +
				"Maintainer: repokit tests <test@example.invalid>\n" +
				"Description: hello-xz fixture\n",
		},
		{
			file:       "hello-zst_1.0_amd64.deb",
			controlExt: "zst",
			control: "Package: hello-zst\n" +
				"Source: foo (1.2-3)\n" +
				"Version: 1.0\n" +
				"Architecture: amd64\n" +
				"Maintainer: repokit tests <test@example.invalid>\n" +
				"Description: hello-zst fixture\n" +
				"\n\n   \n",
		},
	}

	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)

	for _, fx := range fixtures {
		out := filepath.Join(dir, fx.file)
		if err := writeDeb(out, fx); err != nil {
			log.Fatalf("%s: %v", fx.file, err)
		}
		fmt.Printf("wrote %s\n", out)
	}
}

func writeDeb(path string, fx fixture) error {
	controlTar, err := buildControlTar(fx.control)
	if err != nil {
		return fmt.Errorf("control tar: %w", err)
	}
	compressedControl, controlName, err := compressControlTar(controlTar, fx.controlExt)
	if err != nil {
		return fmt.Errorf("compress control: %w", err)
	}

	dataTar, err := buildDataTar()
	if err != nil {
		return fmt.Errorf("data tar: %w", err)
	}
	compressedData := bytes.NewBuffer(nil)
	gz := gzip.NewWriter(compressedData)
	if _, err := gz.Write(dataTar); err != nil {
		return fmt.Errorf("gzip data: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("gzip data close: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := ar.NewWriter(f)
	if err := w.WriteGlobalHeader(); err != nil {
		return fmt.Errorf("global header: %w", err)
	}

	mtime := time.Unix(1700000000, 0).UTC()
	if err := writeArMember(w, "debian-binary", []byte("2.0\n"), mtime); err != nil {
		return err
	}
	if err := writeArMember(w, controlName, compressedControl, mtime); err != nil {
		return err
	}
	if err := writeArMember(w, "data.tar.gz", compressedData.Bytes(), mtime); err != nil {
		return err
	}
	return nil
}

func writeArMember(w *ar.Writer, name string, body []byte, mtime time.Time) error {
	hdr := &ar.Header{
		Name:    name,
		ModTime: mtime,
		Uid:     0,
		Gid:     0,
		Mode:    0o100644,
		Size:    int64(len(body)),
	}
	if err := w.WriteHeader(hdr); err != nil {
		return fmt.Errorf("ar header %s: %w", name, err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("ar body %s: %w", name, err)
	}
	return nil
}

func buildControlTar(control string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Unix(1700000000, 0).UTC()
	if err := tw.WriteHeader(&tar.Header{
		Name:    "./",
		Mode:    0o755,
		Typeflag: tar.TypeDir,
		ModTime: mtime,
	}); err != nil {
		return nil, err
	}
	body := []byte(control)
	if err := tw.WriteHeader(&tar.Header{
		Name:    "./control",
		Mode:    0o644,
		Size:    int64(len(body)),
		Typeflag: tar.TypeReg,
		ModTime: mtime,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(body); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildDataTar() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Unix(1700000000, 0).UTC()
	if err := tw.WriteHeader(&tar.Header{
		Name:    "./",
		Mode:    0o755,
		Typeflag: tar.TypeDir,
		ModTime: mtime,
	}); err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    "./usr/",
		Mode:    0o755,
		Typeflag: tar.TypeDir,
		ModTime: mtime,
	}); err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:    "./usr/share/",
		Mode:    0o755,
		Typeflag: tar.TypeDir,
		ModTime: mtime,
	}); err != nil {
		return nil, err
	}
	body := []byte("hello, repokit tests\n")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "./usr/share/hello.txt",
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
		ModTime:  mtime,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(body); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func compressControlTar(raw []byte, ext string) ([]byte, string, error) {
	switch ext {
	case "gz":
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(raw); err != nil {
			return nil, "", err
		}
		if err := gz.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "control.tar.gz", nil
	case "xz":
		var buf bytes.Buffer
		x, err := xz.NewWriter(&buf)
		if err != nil {
			return nil, "", err
		}
		if _, err := x.Write(raw); err != nil {
			return nil, "", err
		}
		if err := x.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "control.tar.xz", nil
	case "zst":
		var buf bytes.Buffer
		z, err := zstd.NewWriter(&buf)
		if err != nil {
			return nil, "", err
		}
		if _, err := z.Write(raw); err != nil {
			return nil, "", err
		}
		if err := z.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "control.tar.zst", nil
	}
	return nil, "", fmt.Errorf("unknown ext %q", ext)
}

