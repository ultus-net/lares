// Package install writes the lares binary, the systemd units, and the config.
//
// It never touches sudoers and never enables linger. Both grant privilege or
// change session policy, so the installer prints the one command it will not run
// and the operator runs it. A repair that needs root stays in the queue.
package install

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
)

// Options names what to install and where.
type Options struct {
	HomeDir    string // usually os.UserHomeDir()
	BinaryPath string // the running lares binary, copied into place
	Config     config.Config
	Adapters   map[adapter.Family]adapter.Adapter
}

// Install writes the binary, the units, and the config.
//
// The binary and the units are replaced on every run. config.toml and
// adapters.toml are created only when absent, so a re-install never discards an
// edit. An empty adapter table is replaced with adapter.Defaults(). It returns
// the paths it wrote in order.
func Install(opts Options) ([]string, error) {
	if opts.HomeDir == "" {
		return nil, errors.New("install: home directory is required")
	}
	if opts.BinaryPath == "" {
		return nil, errors.New("install: binary path is required")
	}
	adapters := opts.Adapters
	if len(adapters) == 0 {
		adapters = adapter.Defaults()
	}

	binaryPath := filepath.Join(opts.HomeDir, ".local", "bin", "lares")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		return nil, fmt.Errorf("create binary directory: %w", err)
	}
	if err := copyFile(opts.BinaryPath, binaryPath, 0o755); err != nil {
		return nil, err
	}

	unitsDir := filepath.Join(opts.HomeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(unitsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create unit directory: %w", err)
	}
	servicePath := filepath.Join(unitsDir, "lares.service")
	if err := replaceFile(servicePath, ServiceUnit(), 0o644); err != nil {
		return nil, err
	}
	timerPath := filepath.Join(unitsDir, "lares.timer")
	if err := replaceFile(timerPath, TimerUnit(opts.Config.Resync), 0o644); err != nil {
		return nil, err
	}

	configDir := filepath.Join(opts.HomeDir, ".config", "lares")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	configPath := filepath.Join(configDir, "config.toml")
	if err := createIfAbsent(configPath, ConfigTOML(opts.Config), 0o600); err != nil {
		return nil, err
	}
	adapterPath := filepath.Join(configDir, "adapters.toml")
	if err := createIfAbsent(adapterPath, AdaptersTOML(adapters), 0o600); err != nil {
		return nil, err
	}

	return []string{binaryPath, servicePath, timerPath, configPath, adapterPath}, nil
}

// copyFile copies src to dst, replacing dst.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	return nil
}

// replaceFile writes body to path with mode, replacing the file.
func replaceFile(path, body string, mode os.FileMode) error {
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// createIfAbsent writes body only when path does not exist.
func createIfAbsent(path, body string, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// ConfigTOML renders a config file from cfg.
func ConfigTOML(cfg config.Config) string {
	var b strings.Builder
	b.WriteString("# lares configuration. Written once; edits survive a re-install.\n\n")
	if len(cfg.AuditCommand) > 0 {
		fmt.Fprintf(&b, "audit_command = %s\n", tomlArray(cfg.AuditCommand))
	} else {
		b.WriteString("# audit_command is optional. When unset, lares measures the host in-process.\n")
	}
	fmt.Fprintf(&b, "state_dir = %q\n", cfg.StateDir)
	fmt.Fprintf(&b, "config_dir = %q\n", cfg.ConfigDir)
	if cfg.ReposDir != "" {
		fmt.Fprintf(&b, "repos_dir = %q\n", cfg.ReposDir)
	}
	if len(cfg.Repos) > 0 {
		fmt.Fprintf(&b, "repos = %s\n", tomlArray(cfg.Repos))
	}
	fmt.Fprintf(&b, "window_start = %q\n", cfg.WindowStart)
	fmt.Fprintf(&b, "window_end = %q\n", cfg.WindowEnd)
	fmt.Fprintf(&b, "resync = %q\n", cfg.ResyncText)
	fmt.Fprintf(&b, "reboot_from_pass = %t\n", cfg.RebootFromPass)
	if cfg.AdapterOverride != "" {
		fmt.Fprintf(&b, "adapter_override = %q\n", cfg.AdapterOverride)
	}
	return b.String()
}

// AdaptersTOML renders adapters.toml, sorted by family name.
func AdaptersTOML(adapters map[adapter.Family]adapter.Adapter) string {
	var b strings.Builder
	b.WriteString("# lares update verbs, one section per distro family.\n")
	b.WriteString("# stage and activate are argv arrays and must start with a program.\n")
	b.WriteString("# Written once; edits survive a re-install.\n")
	b.WriteString("#\n")
	b.WriteString("# [arch]\n")
	b.WriteString("# stage = [\"patch-train-update\"]\n")
	b.WriteString("# activate = [\"omarchy-system\", \"reboot\"]\n")
	b.WriteString("# stamp = \"/var/lib/patch-train/version-stamp\"\n")

	if len(adapters) == 0 {
		return b.String()
	}

	families := make([]string, 0, len(adapters))
	for family := range adapters {
		families = append(families, string(family))
	}
	sort.Strings(families)
	for _, name := range families {
		entry := adapters[adapter.Family(name)]
		b.WriteString("\n")
		fmt.Fprintf(&b, "[%s]\n", name)
		fmt.Fprintf(&b, "stage = %s\n", tomlArray(entry.Stage))
		fmt.Fprintf(&b, "activate = %s\n", tomlArray(entry.Activate))
		if entry.Stamp != "" {
			fmt.Fprintf(&b, "stamp = %q\n", entry.Stamp)
		}
	}
	return b.String()
}

// tomlArray renders argv as a TOML array of strings.
func tomlArray(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, fmt.Sprintf("%q", arg))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// ServiceUnit returns lares.service.
//
// The unit runs in the user's home. lares measures the host in-process, so the
// timer needs no checkout and no working directory of its own.
func ServiceUnit() string {
	return `[Unit]
Description=lares reconcile pass

[Service]
Type=oneshot
ExecStart=%h/.local/bin/lares pass
WorkingDirectory=%h
Nice=10
IOSchedulingClass=idle
`
}

// TimerUnit returns lares.timer for the given interval.
func TimerUnit(resync time.Duration) string {
	return fmt.Sprintf(`[Unit]
Description=Run the lares reconcile pass

[Timer]
OnBootSec=5min
OnUnitActiveSec=%s
Persistent=true

[Install]
WantedBy=timers.target
`, resync)
}

// NextSteps returns the commands the operator runs once per host.
//
// There is no sudoers step: lares never asks for a standing grant, so a repair
// that needs root stays queued until the operator runs it.
func NextSteps() []string {
	user := os.Getenv("USER")
	if user == "" {
		user = "$USER"
	}
	return []string{
		fmt.Sprintf("loginctl enable-linger %s", user),
	}
}
