package cli

import (
	"fmt"
	"io"

	"github.com/alkk/repokit/pkg/deb"
	"github.com/alkk/repokit/pkg/pool"
)

// InfoCmd inspects a single .deb file and prints what `add` would do, without
// touching the repo. Distro/Suite/Component are optional flags that, when
// provided, materialize the full target pool path.
type InfoCmd struct {
	Distro    string `long:"distro" description:"distribution to compute target pool path for"`
	Suite     string `long:"suite" description:"suite to compute target pool path for"`
	Component string `long:"component" default:"main" description:"component to compute target pool path for"`

	Args struct {
		Deb string `positional-arg-name:"deb" required:"yes"`
	} `positional-args:"yes"`
}

// Execute runs the info subcommand.
func (cmd *InfoCmd) Execute(c *Context) int {
	if err := cmd.run(c.Stdout); err != nil {
		fmt.Fprintf(c.Stderr, "info: %v\n", err)
		return ExitError
	}
	return ExitOK
}

func (cmd *InfoCmd) run(w io.Writer) error {
	for field, val := range map[string]string{
		fieldDistro: cmd.Distro, fieldSuite: cmd.Suite, "component": cmd.Component,
	} {
		if err := validateScopeName(field, val); err != nil {
			return err
		}
	}
	info, err := deb.Inspect(cmd.Args.Deb)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", cmd.Args.Deb, err)
	}

	fmt.Fprintf(w, "Package:      %s\n", info.Name)
	fmt.Fprintf(w, "Version:      %s\n", info.Version)
	fmt.Fprintf(w, "Architecture: %s\n", info.Architecture)
	if info.Source != "" {
		fmt.Fprintf(w, "Source:       %s\n", info.Source)
	}
	fmt.Fprintf(w, "Size:         %d\n", info.Size)
	fmt.Fprintf(w, "MD5:          %s\n", info.MD5)
	fmt.Fprintf(w, "SHA1:         %s\n", info.SHA1)
	fmt.Fprintf(w, "SHA256:       %s\n", info.SHA256)
	fmt.Fprintf(w, "SHA512:       %s\n", info.SHA512)

	if cmd.Distro != "" && cmd.Suite != "" {
		filename := pool.Filename(cmd.Suite, cmd.Component,
			info.Source, info.Name, info.Version, info.Architecture)
		diskPath := pool.DiskPath(cmd.Distro, filename)
		fmt.Fprintf(w, "Pool path (apt):  %s\n", filename)
		fmt.Fprintf(w, "Pool path (disk): %s\n", diskPath)
	} else {
		fmt.Fprintln(w, "Pool path:    (provide --distro and --suite to compute)")
	}
	return nil
}
