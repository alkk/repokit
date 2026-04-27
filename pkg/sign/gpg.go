package sign

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// gpgBinary is the gpg executable name. Tests shadow it on $PATH via a stub
// shell script rather than rebinding the constant.
const gpgBinary = "gpg"

// digestAlgo is the OpenPGP digest gpg uses to sign Release. SHA1 is rejected
// by modern apt, so the value is forced to SHA256 on every invocation.
const digestAlgo = "SHA256"

// GPGSigner shells out to gpg to clearsign InRelease and produce a detached
// Release.gpg next to a Release file. It assumes gpg-agent already holds the
// private key for KeyID — repokit never prompts for a passphrase itself.
type GPGSigner struct {
	// KeyID is the key id or fingerprint passed via --default-key.
	KeyID string
}

// Sign runs two gpg invocations: one clearsigns Release into InRelease, the
// other produces an ASCII-armored detached Release.gpg. Both files land next
// to releasePath. Stderr from gpg is propagated in the returned error so
// operators see why the signature failed (missing key, locked agent, …).
func (g GPGSigner) Sign(ctx context.Context, releasePath string) error {
	if g.KeyID == "" {
		return errors.New("gpg signer: KeyID is empty")
	}
	dir := filepath.Dir(releasePath)
	if err := runGPG(ctx, g.KeyID, releasePath,
		filepath.Join(dir, "InRelease"), "--clearsign"); err != nil {
		return fmt.Errorf("clearsigning Release: %w", err)
	}
	if err := runGPG(ctx, g.KeyID, releasePath,
		filepath.Join(dir, "Release.gpg"), "--armor", "--detach-sign"); err != nil {
		return fmt.Errorf("detach-signing Release: %w", err)
	}
	return nil
}

// runGPG builds a gpg argv with the shared flags (--batch, --yes,
// --default-key, --digest-algo SHA256) plus the mode-specific flag(s) and
// runs it. extraFlags is the trailing portion before --output and the
// release file (e.g. ["--clearsign"] or ["--armor", "--detach-sign"]).
func runGPG(ctx context.Context, keyID, releasePath, outputPath string, extraFlags ...string) error {
	args := make([]string, 0, len(extraFlags)+8)
	args = append(args,
		"--batch", "--yes",
		"--default-key", keyID,
		"--digest-algo", digestAlgo,
	)
	args = append(args, extraFlags...)
	args = append(args, "--output", outputPath, releasePath)

	cmd := exec.CommandContext(ctx, gpgBinary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return fmt.Errorf("running %s: %w", gpgBinary, err)
		}
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) == 0 {
			return fmt.Errorf("gpg %v: %w", args, err)
		}
		return fmt.Errorf("gpg %v: %w: %s", args, err, msg)
	}
	// gpg's exit status only confirms the bytes reached the kernel. fsync the
	// output so a crash between publish returning and the page cache flushing
	// can't leave a promoted suite with a missing or truncated signature.
	return fsyncFile(outputPath)
}

// fsyncFile opens path, fsyncs it, and closes the handle.
func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", filepath.Base(path), err)
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
