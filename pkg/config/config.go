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
		if err := validateDistroName(d); err != nil {
			return nil, err
		}
		if _, dup := distros[d]; dup {
			return nil, fmt.Errorf("config: duplicate distros entry %q", d)
		}
		distros[d] = struct{}{}
	}
	for key, suite := range c.Suites {
		distro, name, ok := splitSuiteKey(key)
		if !ok {
			return nil, fmt.Errorf("config: suite key %q must be in <distro>/<suite> form", key)
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

// validateDistroName enforces a strict positive allowlist for entries in the
// distros: config list. Distro names are user-facing scope identifiers and end
// up as path components under <publicDir>/<distro>/...; keeping the alphabet
// narrow (ASCII letters in either case, digits, ASCII hyphen) eliminates the
// need for path-segment escaping or Release-header sanitization downstream.
func validateDistroName(name string) error {
	if name == "" {
		return errors.New("config: distros entry must not be empty")
	}
	if name[0] == '-' {
		return fmt.Errorf("config: distros entry %q must not start with '-'", name)
	}
	for i := range len(name) {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-'
		if !ok {
			return fmt.Errorf("config: distros entry %q: only [a-zA-Z0-9-] allowed", name)
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

// splitSuiteKey parses "<distro>/<suite>" keys; both halves must be non-empty
// and neither half may itself contain a "/". An extra slash (e.g.
// "debian/bookworm/security") would silently bypass SuiteOrDefault, whose
// lookup key is always "<distro>/<suite>" — so we reject it here rather than
// loading a dead config entry.
func splitSuiteKey(key string) (distro, suite string, ok bool) {
	d, s, found := strings.Cut(key, "/")
	if !found || d == "" || s == "" || strings.Contains(s, "/") {
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
