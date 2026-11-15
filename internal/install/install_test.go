package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
)

// sourceBinary writes a fake lares binary and returns its path.
func sourceBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lares")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho lares\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// testConfig is a valid config with every path inside home.
func testConfig(t *testing.T, home string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = filepath.Join(home, ".local", "state", "lares")
	cfg.ConfigDir = filepath.Join(home, ".config", "lares")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return cfg
}

func testAdapters() map[adapter.Family]adapter.Adapter {
	return map[adapter.Family]adapter.Adapter{
		adapter.Arch: {
			Family:   adapter.Arch,
			Stage:    []string{"patch-train-update"},
			Activate: []string{"omarchy-system", "reboot"},
			Stamp:    "/var/lib/patch-train/version-stamp",
		},
		adapter.FedoraAtomic: {
			Family:   adapter.FedoraAtomic,
			Stage:    []string{"rpm-ostree", "upgrade"},
			Activate: []string{"systemctl", "reboot"},
		},
	}
}

func TestInstallWritesEveryFile(t *testing.T) {
	home := t.TempDir()
	written, err := Install(Options{
		HomeDir:    home,
		BinaryPath: sourceBinary(t),
		Config:     testConfig(t, home),
		Adapters:   testAdapters(),
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := []string{
		filepath.Join(home, ".local", "bin", "lares"),
		filepath.Join(home, ".config", "systemd", "user", "lares.service"),
		filepath.Join(home, ".config", "systemd", "user", "lares.timer"),
		filepath.Join(home, ".config", "lares", "config.toml"),
		filepath.Join(home, ".config", "lares", "adapters.toml"),
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "lares", "lares.sudoers")); !os.IsNotExist(err) {
		t.Fatalf("installer wrote a sudoers file: %v", err)
	}
	if len(written) != len(want) {
		t.Fatalf("wrote %d files, want %d: %v", len(written), len(want), written)
	}
	for i, path := range want {
		if written[i] != path {
			t.Errorf("written[%d] = %q, want %q", i, written[i], path)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", path)
		}
	}
}

func TestInstallModes(t *testing.T) {
	home := t.TempDir()
	written, err := Install(Options{
		HomeDir:    home,
		BinaryPath: sourceBinary(t),
		Config:     testConfig(t, home),
		Adapters:   testAdapters(),
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := map[string]os.FileMode{
		written[0]: 0o755,
		written[1]: 0o644,
		written[2]: 0o644,
		written[3]: 0o600,
		written[4]: 0o600,
	}
	for path, mode := range want {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("%s mode %o, want %o", filepath.Base(path), got, mode)
		}
	}
	for _, dir := range []string{
		filepath.Join(home, ".config", "lares"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode %o, want 700", filepath.Base(dir), got)
		}
	}
}

func TestInstallIsIdempotentForConfig(t *testing.T) {
	home := t.TempDir()
	opts := Options{
		HomeDir:    home,
		BinaryPath: sourceBinary(t),
		Config:     testConfig(t, home),
		Adapters:   testAdapters(),
	}
	if _, err := Install(opts); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	configPath := filepath.Join(home, ".config", "lares", "config.toml")
	edit := []byte("# operator edit\nresync = \"45m\"\n")
	if err := os.WriteFile(configPath, edit, 0o600); err != nil {
		t.Fatal(err)
	}
	adapterPath := filepath.Join(home, ".config", "lares", "adapters.toml")
	adapterEdit := []byte("[arch]\nstage = [\"custom\"]\nactivate = [\"reboot\"]\n")
	if err := os.WriteFile(adapterPath, adapterEdit, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Install(opts); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	for path, want := range map[string][]byte{configPath: edit, adapterPath: adapterEdit} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s was overwritten:\n%s", filepath.Base(path), got)
		}
	}
}

func TestInstallReplacesTheBinaryAndUnits(t *testing.T) {
	home := t.TempDir()
	opts := Options{
		HomeDir:    home,
		BinaryPath: sourceBinary(t),
		Config:     testConfig(t, home),
		Adapters:   testAdapters(),
	}
	if _, err := Install(opts); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	binaryPath := filepath.Join(home, ".local", "bin", "lares")
	if err := os.WriteFile(binaryPath, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(opts); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	got, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "stale" {
		t.Error("binary was not replaced")
	}
}

func TestInstallRejectsMissingBinary(t *testing.T) {
	home := t.TempDir()
	if _, err := Install(Options{HomeDir: home, Config: testConfig(t, home)}); err == nil {
		t.Fatal("Install accepted an empty BinaryPath")
	}
	if _, err := Install(Options{BinaryPath: sourceBinary(t), Config: testConfig(t, home)}); err == nil {
		t.Fatal("Install accepted an empty HomeDir")
	}
}

func TestConfigTOMLRoundTrips(t *testing.T) {
	home := t.TempDir()
	cfg := testConfig(t, home)
	cfg.RepoDir = "/home/tester/dotfiles"
	cfg.AdapterOverride = "arch"
	cfg.WindowStart = "23:30"
	cfg.WindowEnd = "01:15"
	cfg.RebootFromPass = false
	body := ConfigTOML(cfg)

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v\n%s", err, body)
	}
	if got.StateDir != cfg.StateDir || got.ConfigDir != cfg.ConfigDir {
		t.Errorf("dirs %q %q, want %q %q", got.StateDir, got.ConfigDir, cfg.StateDir, cfg.ConfigDir)
	}
	if got.RepoDir != "/home/tester/dotfiles" {
		t.Errorf("repo dir %q", got.RepoDir)
	}
	if got.RebootFromPass {
		t.Error("reboot_from_pass = false did not survive the round trip")
	}
	if got.WindowStart != "23:30" || got.WindowEnd != "01:15" {
		t.Errorf("window %+v", got.Window)
	}
	if got.AdapterOverride != "arch" {
		t.Errorf("override %q", got.AdapterOverride)
	}
	if strings.Join(got.AuditCommand, " ") != strings.Join(cfg.AuditCommand, " ") {
		t.Errorf("audit command %v, want %v", got.AuditCommand, cfg.AuditCommand)
	}
}

func TestAdaptersTOMLRoundTrips(t *testing.T) {
	body := AdaptersTOML(testAdapters())
	path := filepath.Join(t.TempDir(), "adapters.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := adapter.Load(path)
	if err != nil {
		t.Fatalf("adapter.Load: %v\n%s", err, body)
	}
	if len(table) != 2 {
		t.Fatalf("got %d sections, want 2", len(table))
	}
	arch, ok := table[adapter.Arch]
	if !ok {
		t.Fatal("arch section missing")
	}
	if strings.Join(arch.Stage, " ") != "patch-train-update" {
		t.Errorf("stage %v", arch.Stage)
	}
	if strings.Join(arch.Activate, " ") != "omarchy-system reboot" {
		t.Errorf("activate %v", arch.Activate)
	}
	if arch.Stamp != "/var/lib/patch-train/version-stamp" {
		t.Errorf("stamp %q", arch.Stamp)
	}
}

func TestAdaptersTOMLEmptyIsHeaderOnly(t *testing.T) {
	body := AdaptersTOML(map[adapter.Family]adapter.Adapter{})
	if strings.Contains(body, "[arch]\nstage") {
		t.Errorf("empty table wrote a section:\n%s", body)
	}
	if !strings.HasPrefix(body, "#") {
		t.Errorf("body does not start with a comment:\n%s", body)
	}
}

func TestServiceUnit(t *testing.T) {
	body := ServiceUnit("/home/tester/dotfiles")
	for _, want := range []string{
		"Type=oneshot",
		"ExecStart=%h/.local/bin/lares pass",
		"Nice=10",
		"IOSchedulingClass=idle",
		"WorkingDirectory=/home/tester/dotfiles",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("service unit missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(ServiceUnit(""), "WorkingDirectory") {
		t.Error("an empty repo dir still wrote WorkingDirectory")
	}
}

func TestTimerUnit(t *testing.T) {
	body := TimerUnit(15 * time.Minute)
	for _, want := range []string{
		"OnBootSec=5min",
		"OnUnitActiveSec=15m0s",
		"Persistent=true",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("timer unit missing %q:\n%s", want, body)
		}
	}
}

func TestInstallShipsDefaultAdapters(t *testing.T) {
	home := t.TempDir()
	opts := Options{HomeDir: home, BinaryPath: sourceBinary(t), Config: testConfig(t, home)}
	if _, err := Install(opts); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path := filepath.Join(home, ".config", "lares", "adapters.toml")
	table, err := adapter.Load(path)
	if err != nil {
		t.Fatalf("adapter.Load: %v", err)
	}
	for _, family := range []adapter.Family{adapter.Arch, adapter.FedoraAtomic, adapter.Bootc} {
		if _, ok := table[family]; !ok {
			t.Errorf("shipped adapters.toml has no %q section", family)
		}
	}
}

func TestNextSteps(t *testing.T) {
	steps := NextSteps()
	if len(steps) != 1 {
		t.Fatalf("got %d steps, want 1: %v", len(steps), steps)
	}
	if !strings.HasPrefix(steps[0], "loginctl enable-linger") {
		t.Errorf("step %q is not enable-linger", steps[0])
	}
}
