// Package sign produces the detached and inline OpenPGP signatures that apt
// requires alongside a Release file. The Signer interface decouples the
// publish flow from any particular signing backend so tests can substitute a
// nop implementation and production code can shell out to gpg.
package sign

import "context"

// Signer produces the InRelease (clearsigned) and Release.gpg (detached,
// ASCII-armored) artifacts that sit next to a Release file. The releasePath
// argument is the absolute path to the Release file; implementations write
// InRelease and Release.gpg into the same directory.
type Signer interface {
	Sign(ctx context.Context, releasePath string) error
}
