// Package install writes the lares binary, the systemd units, and the config.
//
// It never touches sudoers and never enables linger. Both grant privilege or
// change session policy, so the installer prints the exact commands and the
// operator runs them.
package install

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
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

// Install writes the binary, the units, the config, and the sudoers template.
//
// The binary and the units are replaced on every run. config.toml,
// adapters.toml, and lares.sudoers are created only when absent, so a re-install
// never discards an edit. It returns the paths it wrote in order.
func Install(opts Options) ([]string, error) {
	if opts.HomeDir == "" {
		return nil, errors.New("install: home directory is required")
	}
	if opts.BinaryPath == "" {
		return nil, errors.New("install: binary path is required")
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
	if err := createIfAbsent(adapterPath, AdaptersTOML(opts.Adapters), 0o600); err != nil {
		return nil, err
	}
	sudoersPath := filepath.Join(configDir, "lares.sudoers")
	if err := createIfAbsent(sudoersPath, SudoersText(opts.Adapters), 0o644); err != nil {
		return nil, err
	}

	return []string{binaryPath, servicePath, timerPath, configPath, adapterPath, sudoersPath}, nil
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
	fmt.Fprintf(&b, "audit_command = %s\n", tomlArray(cfg.AuditCommand))
	fmt.Fprintf(&b, "state_dir = %q\n", cfg.StateDir)
	fmt.Fprintf(&b, "config_dir = %q\n", cfg.ConfigDir)
	fmt.Fprintf(&b, "window_start = %q\n", cfg.WindowStart)
	fmt.Fprintf(&b, "window_end = %q\n", cfg.WindowEnd)
	fmt.Fprintf(&b, "resync = %q\n", cfg.ResyncText)
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
func ServiceUnit() string {
	return `[Unit]
Description=lares reconcile pass

[Service]
Type=oneshot
ExecStart=%h/.local/bin/lares pass
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

// SudoersText renders the drop-in for the stage verbs.
//
// Each stage program is resolved to an absolute path now, because sudoers does
// not search PATH. A program that is not installed yet falls back to
// /usr/local/bin, and the operator fixes the path before installing the file.
func SudoersText(adapters map[adapter.Family]adapter.Adapter) string {
	programs := uniqueStagePrograms(adapters)
	var b strings.Builder
	b.WriteString("# lares: the stage verbs this host may run as root.\n")
	b.WriteString("# Review the paths, then install with:\n")
	b.WriteString("#   sudo install -m 0440 lares.sudoers /etc/sudoers.d/lares\n")
	if len(programs) == 0 {
		b.WriteString("# No adapters configured, so no command is granted.\n")
		return b.String()
	}
	b.WriteString("Cmnd_Alias LARES_STAGE = ")
	paths := make([]string, 0, len(programs))
	for _, program := range programs {
		paths = append(paths, resolvedProgram(program))
	}
	b.WriteString(strings.Join(paths, ", "))
	b.WriteString("\n")
	b.WriteString("# Change the group to the one your user belongs to.\n")
	b.WriteString("%wheel ALL=(root) NOPASSWD: LARES_STAGE\n")
	return b.String()
}

// uniqueStagePrograms returns the stage programs once, sorted.
func uniqueStagePrograms(adapters map[adapter.Family]adapter.Adapter) []string {
	seen := make(map[string]bool)
	var programs []string
	for _, entry := range adapters {
		if len(entry.Stage) == 0 {
			continue
		}
		program := entry.Stage[0]
		if program == "" || seen[program] {
			continue
		}
		seen[program] = true
		programs = append(programs, program)
	}
	sort.Strings(programs)
	return programs
}

// resolvedProgram returns an absolute path for a stage program.
func resolvedProgram(program string) string {
	if filepath.IsAbs(program) {
		return program
	}
	if path, err := exec.LookPath(program); err == nil {
		if absolute, err := filepath.Abs(path); err == nil {
			return absolute
		}
	}
	return filepath.Join("/usr/local/bin", program)
}

// NextSteps returns the two commands the operator runs once per host.
func NextSteps(homeDir string) []string {
	user := os.Getenv("USER")
	if user == "" {
		user = "$USER"
	}
	sudoers := filepath.Join(homeDir, ".config", "lares", "lares.sudoers")
	return []string{
		fmt.Sprintf("sudo install -m 0440 %s /etc/sudoers.d/lares", sudoers),
		fmt.Sprintf("loginctl enable-linger %s", user),
	}
}
