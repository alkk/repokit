// Package config loads and validates the repokit YAML config file.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the parsed repokit config file.
type Config struct {
	SigningKey string                 `yaml:"signing_key"`
	PublicDir  string                 `yaml:"public_dir"`
	Distros    []string               `yaml:"distros"`
	Suites     map[string]SuiteConfig `yaml:"suites"`
}

// SuiteConfig is the per-suite Release header data.
type SuiteConfig struct {
	Origin      string `yaml:"origin"`
	Label       string `yaml:"label"`
	Suite       string `yaml:"suite"`
	Codename    string `yaml:"codename"`
	Description string `yaml:"description"`
}

// Load reads, parses, and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := validateSigningKey(c.SigningKey); err != nil {
		return nil, err
	}
	if c.PublicDir != "" {
		for _, r := range c.PublicDir {
			if r < 0x20 || r == 0x7f {
				return nil, errors.New("config: public_dir must not contain control characters")
			}
		}
	}
	distros := make(map[string]struct{}, len(c.Distros))
	for _, d := range c.Distros {
		if err := ValidateDistroName(d); err != nil {
			return nil, fmt.Errorf("config: distros entry %q: %w", d, err)
		}
		if _, dup := distros[d]; dup {
			return nil, fmt.Errorf("config: duplicate distros entry %q", d)
		}
		distros[d] = struct{}{}
	}
	// Reject namespaced entries whose first segment is also a plain distro:
	// publish.filesystemScopes walks <publicDir>/<distro>/dists/ and would not
	// be able to disambiguate between a plain "customer" and a namespaced
	// "customer/acme" that share the same top-level directory.
	for _, d := range c.Distros {
		prefix, _, namespaced := strings.Cut(d, "/")
		if !namespaced {
			continue
		}
		if _, conflict := distros[prefix]; conflict {
			return nil, fmt.Errorf("config: distros entry %q conflicts with plain distro %q (namespaced and plain forms cannot share a top-level segment)", d, prefix)
		}
	}
	for key, suite := range c.Suites {
		distro, name, ok := splitSuiteKey(key)
		if !ok {
			return nil, fmt.Errorf("config: suite key %q must be in <distro>/<suite> form", key)
		}
		if err := ValidateDistroName(distro); err != nil {
			return nil, fmt.Errorf("config: suite key %q: %w", key, err)
		}
		if _, found := distros[distro]; !found {
			return nil, fmt.Errorf("config: suite %q references distro %q not in distros", key, distro)
		}
		if err := validateSuiteHeaders(key, suite); err != nil {
			return nil, err
		}
		c.Suites[key] = applyDefaults(suite, distro, name)
	}
	return c, nil
}

// ValidateDistroName enforces a strict positive allowlist for distro names.
// Distro names are user-facing scope identifiers and end up as path components
// under <publicDir>/<distro>/...; keeping the alphabet narrow (ASCII letters in
// either case, digits, ASCII hyphen) eliminates the need for path-segment
// escaping or Release-header sanitization downstream.
//
// A name is either a single plain segment ([a-zA-Z0-9-]+, no leading '-') or
// two such segments joined by exactly one '/'. Depth >= 3 is rejected. The
// returned error is context-neutral ("must be ...", "segment ...") so callers
// can frame it for their own surface — `config.Load` prefixes "config: distros
// entry %q:" and `pkg/cli` prefixes "--distro %q:".
func ValidateDistroName(name string) error {
	if name == "" {
		return errors.New("must not be empty")
	}
	if strings.Count(name, "/") > 1 {
		return errors.New("must be a single segment or two segments separated by '/'")
	}
	left, right, namespaced := strings.Cut(name, "/")
	if !namespaced {
		return validateDistroSegment(name)
	}
	if err := validateDistroSegment(left); err != nil {
		return err
	}
	return validateDistroSegment(right)
}

// validateDistroSegment enforces the per-segment alphabet rules shared by plain
// and namespaced distro names. Rejects empty, leading-'-', dot-segments, and
// any byte outside [a-zA-Z0-9-].
func validateDistroSegment(seg string) error {
	if seg == "" {
		return errors.New("segment must not be empty")
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("segment %q must not be '.' or '..'", seg)
	}
	if seg[0] == '-' {
		return fmt.Errorf("segment %q must not start with '-'", seg)
	}
	for i := range len(seg) {
		c := seg[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-'
		if !ok {
			return fmt.Errorf("segment %q: only [a-zA-Z0-9-] allowed", seg)
		}
	}
	return nil
}

// validateSigningKey rejects signing_key values that would be unsafe to pass
// to gpg as the --default-key argument: newlines (which would split the value
// into multiple arguments via shell glue if a wrapper ever stringifies argv)
// and a leading "-" (which gpg would interpret as a flag rather than a key
// id, since runGPG does not insert a "--" argv terminator before the key).
func validateSigningKey(key string) error {
	if strings.ContainsAny(key, "\r\n") {
		return errors.New("config: signing_key must not contain newline characters")
	}
	if strings.HasPrefix(strings.TrimSpace(key), "-") {
		return errors.New("config: signing_key must not start with '-'")
	}
	return nil
}

// validateSuiteHeaders rejects control bytes in suite header fields. These
// values are written verbatim into Release file headers; an embedded newline
// would inject attacker-chosen headers into the signed Release file.
func validateSuiteHeaders(key string, s SuiteConfig) error {
	fields := []struct {
		name, val string
	}{
		{"origin", s.Origin},
		{"label", s.Label},
		{"suite", s.Suite},
		{"codename", s.Codename},
		{"description", s.Description},
	}
	for _, f := range fields {
		if strings.ContainsAny(f.val, "\r\n") {
			return fmt.Errorf("config: suite %q field %q must not contain newline characters", key, f.name)
		}
	}
	return nil
}

// SuiteOrDefault returns the configured suite block for (distro, suite), or a
// synthesized default when no entry exists in the config.
func (c *Config) SuiteOrDefault(distro, suite string) SuiteConfig {
	if c != nil {
		if s, ok := c.Suites[distro+"/"+suite]; ok {
			return s
		}
	}
	return defaultSuite(distro, suite)
}

// splitSuiteKey parses "<distro>/<suite>" keys. The distro half may itself be
// namespaced (one '/'), so we split on the LAST '/' — that always yields a
// non-slashed suite name on the right. Both halves must be non-empty.
// "customer/acme/stable" parses to ("customer/acme", "stable"); a key with a
// deeper distro half like "a/b/c/d" parses to ("a/b/c", "d") and ValidateDistroName
// on the left then rejects "a/b/c" for carrying more than one '/'.
func splitSuiteKey(key string) (distro, suite string, ok bool) {
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return "", "", false
	}
	d, s := key[:i], key[i+1:]
	if d == "" || s == "" {
		return "", "", false
	}
	return d, s, true
}

// applyDefaults fills any blank SuiteConfig field with a value derived from the
// (distro, suite) pair.
func applyDefaults(s SuiteConfig, distro, suite string) SuiteConfig {
	d := defaultSuite(distro, suite)
	if s.Origin == "" {
		s.Origin = d.Origin
	}
	if s.Label == "" {
		s.Label = d.Label
	}
	if s.Suite == "" {
		s.Suite = d.Suite
	}
	if s.Codename == "" {
		s.Codename = d.Codename
	}
	if s.Description == "" {
		s.Description = d.Description
	}
	return s
}

// defaultSuite synthesizes a SuiteConfig from a (distro, suite) pair.
func defaultSuite(distro, suite string) SuiteConfig {
	return SuiteConfig{
		Origin:      distro,
		Label:       distro,
		Suite:       suite,
		Codename:    suite,
		Description: fmt.Sprintf("%s %s", distro, suite),
	}
}
