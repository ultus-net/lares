package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if got := strings.Join(cfg.AuditCommand, " "); got != "bash install/lib/hygiene-audit.sh" {
		t.Errorf("audit command %q", got)
	}
	if cfg.Resync != 15*time.Minute {
		t.Errorf("resync %v, want 15m", cfg.Resync)
	}
	if cfg.Window != (Window{Start: "02:00", End: "05:00"}) {
		t.Errorf("window %+v", cfg.Window)
	}
	if cfg.StateDir != "" || cfg.ConfigDir != "" {
		t.Errorf("dirs should be empty until Load: %q %q", cfg.StateDir, cfg.ConfigDir)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state-home")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/config-home")
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StateDir != "/tmp/state-home/lares" {
		t.Errorf("state dir %q", cfg.StateDir)
	}
	if cfg.ConfigDir != "/tmp/config-home/lares" {
		t.Errorf("config dir %q", cfg.ConfigDir)
	}
	if cfg.Resync != 15*time.Minute {
		t.Errorf("resync %v", cfg.Resync)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
audit_command = ["bash", "/opt/audit.sh"]
state_dir = "/var/lib/lares"
config_dir = "/etc/lares"
adapter_override = "arch"
window_start = "23:30"
window_end = "01:15"
resync = "2h"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Resync != 2*time.Hour {
		t.Errorf("resync %v", cfg.Resync)
	}
	if cfg.Window != (Window{Start: "23:30", End: "01:15"}) {
		t.Errorf("window %+v", cfg.Window)
	}
	if cfg.AdapterOverride != "arch" {
		t.Errorf("override %q", cfg.AdapterOverride)
	}
	if cfg.StateDir != "/var/lib/lares" || cfg.ConfigDir != "/etc/lares" {
		t.Errorf("dirs %q %q", cfg.StateDir, cfg.ConfigDir)
	}
}

func TestLoadEmptyFileUsesDefaults(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state-home")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/config-home")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Resync != 15*time.Minute {
		t.Errorf("resync %v", cfg.Resync)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown key", "audit_command = [\"bash\"]\nhostname = \"x\"\n", `unknown key "hostname"`},
		{"bad toml", "audit_command = [\n", "read"},
		{"bad resync", "resync = \"fast\"\n", "resync"},
		{"short resync", "resync = \"30s\"\n", "at least 1m"},
		{"empty resync", "resync = \"\"\n", "resync"},
		{"bad window start", "window_start = \"2:00\"\n", "window_start"},
		{"bad window end", "window_end = \"25:00\"\n", "window_end"},
		{"empty audit command", "audit_command = []\n", "audit_command"},
		{"empty program", "audit_command = [\"\"]\n", "audit_command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", "/tmp/state-home")
			t.Setenv("XDG_CONFIG_HOME", "/tmp/config-home")
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted a bad file")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestValidateDerivesFields(t *testing.T) {
	cfg := Default()
	cfg.StateDir = "/state"
	cfg.ConfigDir = "/config"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Resync != 15*time.Minute {
		t.Errorf("resync %v", cfg.Resync)
	}
	if cfg.Window != (Window{Start: "02:00", End: "05:00"}) {
		t.Errorf("window %+v", cfg.Window)
	}
}

func TestWindowContains(t *testing.T) {
	tests := []struct {
		name   string
		window Window
		at     string
		want   bool
	}{
		{"same day inside", Window{"02:00", "05:00"}, "03:30", true},
		{"same day at start", Window{"02:00", "05:00"}, "02:00", true},
		{"same day at end", Window{"02:00", "05:00"}, "05:00", false},
		{"same day before", Window{"02:00", "05:00"}, "01:59", false},
		{"same day after", Window{"02:00", "05:00"}, "05:01", false},
		{"wrap late", Window{"23:00", "02:00"}, "23:30", true},
		{"wrap early", Window{"23:00", "02:00"}, "01:59", true},
		{"wrap at start", Window{"23:00", "02:00"}, "23:00", true},
		{"wrap at end", Window{"23:00", "02:00"}, "02:00", false},
		{"wrap mid day", Window{"23:00", "02:00"}, "12:00", false},
		{"zero length", Window{"02:00", "02:00"}, "02:00", false},
		{"bad start", Window{"nope", "05:00"}, "03:00", false},
		{"bad end", Window{"02:00", "nope"}, "03:00", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock, err := time.ParseInLocation("15:04", tt.at, time.Local)
			if err != nil {
				t.Fatal(err)
			}
			if got := tt.window.Contains(clock); got != tt.want {
				t.Errorf("Contains(%s) = %v, want %v", tt.at, got, tt.want)
			}
		})
	}
}

func TestWindowUsesTimeLocation(t *testing.T) {
	window := Window{Start: "02:00", End: "05:00"}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	at := time.Date(2026, 11, 15, 3, 0, 0, 0, tokyo)
	if !window.Contains(at) {
		t.Error("03:00 in Tokyo should be inside the window")
	}
}

func TestDefaultPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-config")
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if path != "/tmp/xdg-config/lares/config.toml" {
		t.Errorf("config path %q", path)
	}
	dir, err := DefaultStateDir()
	if err != nil {
		t.Fatalf("DefaultStateDir: %v", err)
	}
	if dir != "/tmp/xdg-state/lares" {
		t.Errorf("state dir %q", dir)
	}
}

func TestDefaultPathsFallBackToHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/tester")
	dir, err := DefaultStateDir()
	if err != nil {
		t.Fatalf("DefaultStateDir: %v", err)
	}
	if dir != "/home/tester/.local/state/lares" {
		t.Errorf("state dir %q", dir)
	}
}

func TestValidationErrorMessage(t *testing.T) {
	err := &ValidationError{Field: "resync", Detail: "must be at least 1m"}
	if got := err.Error(); !strings.Contains(got, "resync") || !strings.Contains(got, "1m") {
		t.Errorf("message %q names neither the field nor the reason", got)
	}
}
