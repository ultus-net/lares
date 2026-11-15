// Package config loads lares' top-level configuration.
//
// The file names the probe command, the checkout that holds it, the state and
// config directories, an optional adapter override, the resync interval, and
// the local-clock window in which a disruptive repair or a reboot may run. A
// pass evaluates the reboot gate unless reboot_from_pass is false. There is no
// host name and no machine-specific path in the defaults.
//
// Load and Validate both leave Resync and Window set and parsed, so a caller
// never reads ResyncText, WindowStart, or WindowEnd. Validate has a pointer
// receiver for that reason: it writes the parsed values back.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Window is a repair window on the local clock.
//
// A window whose start is before its end does not wrap midnight. A window whose
// start is after its end wraps, so 23:00-02:00 covers 23:00 through 01:59. A
// window with equal ends contains no instant.
type Window struct {
	Start string
	End   string
}

// Config is the whole configuration file.
type Config struct {
	AuditCommand    []string      `toml:"audit_command"`
	StateDir        string        `toml:"state_dir"`
	ConfigDir       string        `toml:"config_dir"`
	RepoDir         string        `toml:"repo_dir"`
	AdapterOverride string        `toml:"adapter_override"`
	WindowStart     string        `toml:"window_start"`
	WindowEnd       string        `toml:"window_end"`
	ResyncText      string        `toml:"resync"`
	RebootFromPass  bool          `toml:"reboot_from_pass"`
	Resync          time.Duration `toml:"-"`
	Window          Window        `toml:"-"`
}

// Default returns the configuration used when no file exists.
//
// The probe is ["bash", "install/lib/hygiene-audit.sh"], the resync interval is
// 15 minutes, the window is 02:00-05:00, and a pass evaluates the reboot gate.
// StateDir and ConfigDir are empty until Load fills them from the XDG defaults.
func Default() Config {
	return Config{
		AuditCommand:   []string{"bash", "install/lib/hygiene-audit.sh"},
		ResyncText:     "15m",
		Resync:         15 * time.Minute,
		WindowStart:    "02:00",
		WindowEnd:      "05:00",
		Window:         Window{Start: "02:00", End: "05:00"},
		RebootFromPass: true,
	}
}

// Load reads path.
//
// A missing file returns Default with the derived directories and no error. An
// unreadable file, invalid TOML, an unknown key, an unparsable resync, or a
// value that fails Validate is an error.
func Load(path string) (Config, error) {
	cfg := Default()
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if err := cfg.ensureDirs(); err != nil {
				return Config{}, err
			}
			return cfg, cfg.Validate()
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return Config{}, fmt.Errorf("config %s: unknown key %q", path, keys[0])
	}
	if err := cfg.ensureDirs(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// ensureDirs fills StateDir and ConfigDir when the file left them empty.
func (c *Config) ensureDirs() error {
	if c.StateDir == "" {
		dir, err := DefaultStateDir()
		if err != nil {
			return err
		}
		c.StateDir = dir
	}
	if c.ConfigDir == "" {
		path, err := DefaultConfigPath()
		if err != nil {
			return err
		}
		c.ConfigDir = filepath.Dir(path)
	}
	return nil
}

// DefaultConfigPath returns $XDG_CONFIG_HOME/lares/config.toml, falling back to
// os.UserConfigDir.
func DefaultConfigPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		var err error
		dir, err = os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("locate config directory: %w", err)
		}
	}
	return filepath.Join(dir, "lares", "config.toml"), nil
}

// DefaultStateDir returns $XDG_STATE_HOME/lares, falling back to
// $HOME/.local/state/lares.
func DefaultStateDir() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("neither XDG_STATE_HOME nor HOME is set")
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "lares"), nil
}

// Validate parses ResyncText and the window, checks every required value, and
// writes Resync and Window.
func (c *Config) Validate() error {
	if len(c.AuditCommand) == 0 || c.AuditCommand[0] == "" {
		return &ValidationError{Field: "audit_command", Detail: "must name a program"}
	}
	resync, err := time.ParseDuration(c.ResyncText)
	if err != nil {
		return &ValidationError{Field: "resync", Detail: "must be a duration such as 15m"}
	}
	if resync < time.Minute {
		return &ValidationError{Field: "resync", Detail: "must be at least 1m"}
	}
	if _, err := parseClock(c.WindowStart); err != nil {
		return &ValidationError{Field: "window_start", Detail: "must be HH:MM"}
	}
	if _, err := parseClock(c.WindowEnd); err != nil {
		return &ValidationError{Field: "window_end", Detail: "must be HH:MM"}
	}
	if c.StateDir == "" {
		return &ValidationError{Field: "state_dir", Detail: "must be non-empty"}
	}
	if c.ConfigDir == "" {
		return &ValidationError{Field: "config_dir", Detail: "must be non-empty"}
	}
	c.Resync = resync
	c.Window = Window{Start: c.WindowStart, End: c.WindowEnd}
	return nil
}

// ValidationError is a bad configuration value.
type ValidationError struct {
	Field  string
	Detail string
}

// Error names the field and what is wrong with it.
func (e *ValidationError) Error() string {
	return "invalid " + e.Field + ": " + e.Detail
}

// Contains reports whether t falls inside the window, in t's own location.
func (w Window) Contains(t time.Time) bool {
	start, err := parseClock(w.Start)
	if err != nil {
		return false
	}
	end, err := parseClock(w.End)
	if err != nil {
		return false
	}
	now := t.Hour()*60 + t.Minute()
	switch {
	case start == end:
		return false
	case start < end:
		return now >= start && now < end
	default:
		return now >= start || now < end
	}
}

// String renders the window as stored.
func (w Window) String() string {
	return w.Start + "-" + w.End
}

// parseClock turns HH:MM into minutes since midnight.
func parseClock(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' || strings.Count(s, ":") != 1 {
		return 0, errors.New("not HH:MM")
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}
