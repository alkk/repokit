package sign

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// NopSigner produces signature stand-ins that have the right shape but no
// cryptographic value. It exists so the publish flow can be exercised in tests
// without touching gpg-agent. Production code must use GPGSigner.
type NopSigner struct{}

// nopSignatureBlock is the empty PGP signature appended after the cleartext.
// The format mirrors what gpg --clearsign emits so downstream consumers that
// merely look for the markers find them.
const nopSignatureBlock = "-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n"

// Sign writes a clearsigned-shaped InRelease whose body is the verbatim
// Release content and an empty Release.gpg detached-signature stub.
func (NopSigner) Sign(_ context.Context, releasePath string) error {
	body, err := os.ReadFile(releasePath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", releasePath, err)
	}
	dir := filepath.Dir(releasePath)
	inRelease := "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" + string(body) + nopSignatureBlock
	if err := writeAndSync(filepath.Join(dir, "InRelease"), []byte(inRelease)); err != nil {
		return err
	}
	if err := writeAndSync(filepath.Join(dir, "Release.gpg"), nil); err != nil {
		return err
	}
	return nil
}

// writeAndSync writes data to path, fsyncs the file before closing so the
// bytes are durable before the surrounding publish atomically promotes the
// staged tree. Without fsync, a crash after promote could leave the live
// suite missing or truncating its signature files.
func writeAndSync(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", filepath.Base(path), err)
	}
	return nil
}
