// Package adapter selects the per-family update verbs.
//
// A family is detected from the host, never configured by hand. The verbs
// themselves live in adapters.toml so a distribution change is a config edit,
// not a release. An unrecognised host is not a failure: it measures and never
// acts.
//
// The file holds one table per family, with argv arrays:
//
//	[arch]
//	stage = ["patch-train-update"]
//	activate = ["omarchy-system", "reboot"]
//	stamp = "/var/lib/patch-train/version-stamp"
//
//	[fedora-atomic]
//	stage = ["rpm-ostree", "upgrade"]
//	activate = ["systemctl", "reboot"]
package adapter

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Family names a distribution family that lares knows how to stage updates on.
type Family string

const (
	Arch         Family = "arch"
	FedoraAtomic Family = "fedora-atomic"
	Bootc        Family = "bootc"
	Unknown      Family = "unknown"
)

// ErrUnknownFamily reports that no adapter can be selected without an override.
var ErrUnknownFamily = errors.New("unknown distro family")

// Adapter is one family's update verbs.
type Adapter struct {
	Family   Family
	Stage    []string // argv that stages an update
	Activate []string // argv that activates a staged update
	Stamp    string   // optional file that records the staged version
}

// Defaults returns the adapters a fresh install ships, one per known family.
//
// The verbs are argv arrays, never shell strings, and the Arch stage is the
// repository's own wrapper rather than pacman or omarchy-update.
func Defaults() map[Family]Adapter {
	return map[Family]Adapter{
		Arch: {
			Family:   Arch,
			Stage:    []string{"patch-train-update"},
			Activate: []string{"omarchy-system", "reboot"},
			Stamp:    "/var/lib/patch-train/version-stamp",
		},
		FedoraAtomic: {
			Family:   FedoraAtomic,
			Stage:    []string{"rpm-ostree", "upgrade"},
			Activate: []string{"systemctl", "reboot"},
		},
		Bootc: {
			Family:   Bootc,
			Stage:    []string{"bootc", "upgrade"},
			Activate: []string{"systemctl", "reboot"},
		},
	}
}

// Detect classifies os-release bytes.
//
// ID and ID_LIKE values may be quoted, and ID_LIKE is space-separated. A host
// whose ID or ID_LIKE names arch is Arch. A host that names fedora is Bootc when
// bootc is present, FedoraAtomic when rpm-ostree is present, and Unknown
// otherwise. Everything else, including input that does not parse, is Unknown
// with no error.
func Detect(osRelease []byte, hasRpmOstree, hasBootc bool) (Family, error) {
	fields := parseOSRelease(osRelease)
	ids := append([]string{fields["id"]}, strings.Fields(fields["id_like"])...)
	for _, id := range ids {
		if id == "arch" {
			return Arch, nil
		}
	}
	for _, id := range ids {
		if id != "fedora" {
			continue
		}
		if hasBootc {
			return Bootc, nil
		}
		if hasRpmOstree {
			return FedoraAtomic, nil
		}
	}
	return Unknown, nil
}

// parseOSRelease reads KEY=value pairs from os-release content. Blank lines,
// comments, and malformed lines are ignored.
func parseOSRelease(data []byte) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = unquote(strings.TrimSpace(value))
	}
	return fields
}

// unquote strips one matching pair of single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if first, last := s[0], s[len(s)-1]; (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// DetectHost classifies the running host. It reads /etc/os-release, so a host
// without that file is an error.
func DetectHost() (Family, error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return Unknown, fmt.Errorf("read /etc/os-release: %w", err)
	}
	_, ostreeErr := exec.LookPath("rpm-ostree")
	_, bootcErr := exec.LookPath("bootc")
	return Detect(data, ostreeErr == nil, bootcErr == nil)
}

// Load reads adapters.toml into a table keyed by family.
//
// An empty file is an error. Every section needs a non-empty stage array and a
// non-empty activate array, each starting with a program name. An unknown key
// is an error.
func Load(path string) (map[Family]Adapter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("adapter file %s is empty", path)
	}

	var raw map[string]fileAdapter
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("adapter file %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("adapter file %s: unknown key %q", path, keys[0])
	}

	table := make(map[Family]Adapter, len(raw))
	for name, entry := range raw {
		if err := entry.validate(); err != nil {
			return nil, fmt.Errorf("adapter file %s: section %q: %w", path, name, err)
		}
		table[Family(name)] = Adapter{
			Family:   Family(name),
			Stage:    entry.Stage,
			Activate: entry.Activate,
			Stamp:    entry.Stamp,
		}
	}
	return table, nil
}

// fileAdapter is one adapters.toml section as written.
type fileAdapter struct {
	Stage    []string `toml:"stage"`
	Activate []string `toml:"activate"`
	Stamp    string   `toml:"stamp"`
}

// validate checks one section's verbs.
func (a fileAdapter) validate() error {
	if len(a.Stage) == 0 || a.Stage[0] == "" {
		return errors.New("stage must name a program")
	}
	if len(a.Activate) == 0 || a.Activate[0] == "" {
		return errors.New("activate must name a program")
	}
	return nil
}

// Select returns the adapter to use for family.
//
// A non-empty override wins and must name a section. Without an override,
// Unknown and a family with no section both return an error wrapping
// ErrUnknownFamily that suggests setting adapter_override.
func Select(family Family, override string, table map[Family]Adapter) (Adapter, error) {
	if override != "" {
		entry, ok := table[Family(override)]
		if !ok {
			return Adapter{}, fmt.Errorf("adapter override %q has no section in adapters.toml", override)
		}
		return entry, nil
	}
	if family == Unknown {
		return Adapter{}, fmt.Errorf("%w: detected no known family; set adapter_override", ErrUnknownFamily)
	}
	entry, ok := table[family]
	if !ok {
		return Adapter{}, fmt.Errorf("%w: no section for %q; set adapter_override", ErrUnknownFamily, family)
	}
	return entry, nil
}
