package measure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/term"
)

const (
	// diskMinBytes is the free space below which a filesystem is a finding.
	diskMinBytes = 2 << 30
	// diskMinPercent is the free percentage below which a filesystem is a
	// finding.
	diskMinPercent = 5
	// trainMaxAge is how old an Arch version stamp may be before the patch
	// train counts as stalled.
	trainMaxAge = 35 * 24 * time.Hour
	// stReadOnly is the statfs flag for a read-only mount. It mirrors
	// ST_RDONLY, which the syscall package does not export on Linux.
	stReadOnly = 0x1
)

// shellCheck reports whether the login shell named in the password database
// exists and is executable.
func (e *Engine) shellCheck() check {
	return check{
		id:   "shell",
		term: term.NeedsOperator,
		run: func(context.Context) (contract.State, string, error) {
			shell, err := e.shell()
			if err != nil {
				return contract.Unknown, "", err
			}
			if shell == "" {
				return contract.NotLive, "the password database names no login shell", nil
			}
			if !filepath.IsAbs(shell) {
				return contract.NotLive, fmt.Sprintf("login shell %q is not an absolute path", shell), nil
			}
			info, err := os.Stat(shell)
			if err != nil {
				return contract.NotLive, fmt.Sprintf("login shell %s is missing", shell), nil
			}
			if info.Mode()&0o111 == 0 {
				return contract.NotLive, fmt.Sprintf("login shell %s is not executable", shell), nil
			}
			return contract.Live, "login shell " + shell, nil
		},
	}
}

// patchTrainCheck reports whether the host has a deployment waiting to
// activate. Staging needs root, so the item queues as needs-grant.
func (e *Engine) patchTrainCheck() check {
	return check{
		id:   "patch-train",
		term: term.NeedsGrant,
		run: func(ctx context.Context) (contract.State, string, error) {
			switch e.family {
			case adapter.Arch:
				return e.archTrain()
			case adapter.Bootc, adapter.FedoraAtomic:
				return e.ostreeTrain(ctx)
			default:
				return contract.Unknown, "no update adapter for this host", nil
			}
		},
	}
}

// archTrain reads the patch train's version stamp and its age.
func (e *Engine) archTrain() (contract.State, string, error) {
	stamp := e.adapter.Stamp
	if stamp == "" {
		stamp = adapter.Defaults()[adapter.Arch].Stamp
	}
	info, err := os.Stat(stamp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return contract.Unknown, "no version stamp at " + stamp, nil
		}
		return contract.Unknown, "", err
	}
	age := e.now().Sub(info.ModTime()).Round(time.Hour)
	version := "unknown version"
	if body, err := e.read(stamp); err == nil {
		if v := strings.TrimSpace(string(body)); v != "" {
			version = v
		}
	}
	if age > trainMaxAge {
		return contract.Stale, fmt.Sprintf("stamp %s is %s old", version, age), nil
	}
	return contract.Live, fmt.Sprintf("stamp %s, updated %s ago", version, age), nil
}

// ostreeStatus is the deployment shape that rpm-ostree reports.
type ostreeStatus struct {
	Deployments []struct {
		Booted   bool   `json:"booted"`
		Staged   bool   `json:"staged"`
		Version  string `json:"version"`
		Checksum string `json:"checksum"`
	} `json:"deployments"`
}

// bootcStatus is the deployment shape that bootc reports.
type bootcStatus struct {
	Status struct {
		Booted struct {
			Image struct {
				Version string `json:"version"`
			} `json:"image"`
		} `json:"booted"`
		Staged json.RawMessage `json:"staged"`
	} `json:"status"`
}

// ostreeAdminStatus is the deployment shape that `ostree admin status --json`
// reports. It carries no version, so the detail falls back to the checksum.
type ostreeAdminStatus struct {
	Deployments []struct {
		Booted   bool   `json:"booted"`
		Staged   bool   `json:"staged"`
		Checksum string `json:"checksum"`
	} `json:"deployments"`
}

// deploymentReader is one way to read the deployment state, in tried order.
type deploymentReader struct {
	argv  []string
	parse func([]byte) (contract.State, string, bool)
}

// ostreeTrain reads the host's deployment state.
//
// The order is deliberate. rpm-ostree answers without root and carries the
// version. `ostree admin status` answers without root on any ostree host, which
// keeps a bootc-only image measurable. bootc is last because it demands root on
// the Universal Blue images, so it only helps when lares runs privileged.
func (e *Engine) ostreeTrain(ctx context.Context) (contract.State, string, error) {
	readers := []deploymentReader{
		{[]string{"rpm-ostree", "status", "--json"}, parseOstree},
		{[]string{"ostree", "admin", "status", "--json"}, parseOstreeAdmin},
		{[]string{"bootc", "status", "--format", "json"}, parseBootc},
	}
	var lastErr error
	for _, reader := range readers {
		out, err := e.run(ctx, "", reader.argv)
		if err != nil {
			lastErr = err
			continue
		}
		if state, detail, ok := reader.parse(out); ok {
			return state, detail, nil
		}
	}
	if lastErr != nil {
		return contract.Unknown, "", fmt.Errorf("read deployment status: %w", lastErr)
	}
	return contract.Unknown, "deployment status did not parse", nil
}

// deploymentName names a deployment by version when it has one, else by a
// checksum prefix.
func deploymentName(version, checksum string) string {
	if version != "" {
		return version
	}
	if len(checksum) > 12 {
		return checksum[:12]
	}
	if checksum != "" {
		return checksum
	}
	return "unknown"
}

// parseOstree decodes an rpm-ostree status document.
func parseOstree(out []byte) (contract.State, string, bool) {
	var st ostreeStatus
	if err := json.Unmarshal(out, &st); err != nil || len(st.Deployments) == 0 {
		return "", "", false
	}
	booted := ""
	for _, d := range st.Deployments {
		if d.Staged {
			return contract.Stale, fmt.Sprintf("deployment %s staged; a reboot activates it", deploymentName(d.Version, d.Checksum)), true
		}
		if d.Booted {
			booted = deploymentName(d.Version, d.Checksum)
		}
	}
	if booted == "" {
		return "", "", false
	}
	return contract.Live, fmt.Sprintf("booted %s, no staged deployment", booted), true
}

// parseOstreeAdmin decodes an `ostree admin status --json` document.
func parseOstreeAdmin(out []byte) (contract.State, string, bool) {
	var st ostreeAdminStatus
	if err := json.Unmarshal(out, &st); err != nil || len(st.Deployments) == 0 {
		return "", "", false
	}
	booted := ""
	for _, d := range st.Deployments {
		if d.Staged {
			return contract.Stale, fmt.Sprintf("deployment %s staged; a reboot activates it", deploymentName("", d.Checksum)), true
		}
		if d.Booted {
			booted = deploymentName("", d.Checksum)
		}
	}
	if booted == "" {
		return "", "", false
	}
	return contract.Live, fmt.Sprintf("booted %s, no staged deployment", booted), true
}

// parseBootc decodes a bootc status document.
func parseBootc(out []byte) (contract.State, string, bool) {
	var st bootcStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return "", "", false
	}
	version := st.Status.Booted.Image.Version
	if version == "" {
		return "", "", false
	}
	staged := len(st.Status.Staged) > 0 && string(st.Status.Staged) != "null"
	if staged {
		return contract.Stale, fmt.Sprintf("deployment staged on booted %s; a reboot activates it", version), true
	}
	return contract.Live, fmt.Sprintf("booted %s, no staged deployment", version), true
}

// systemdCheck reports failed system and user units.
//
// An immutable composefs root cannot be remounted, so systemd-remount-fs fails
// by design on Fedora atomic and bootc hosts with a read-only root. That unit
// is excluded there and named in the detail, because no operator action can
// clear it.
func (e *Engine) systemdCheck() check {
	return check{
		id:   "systemd-failed",
		term: term.NeedsOperator,
		run: func(ctx context.Context) (contract.State, string, error) {
			scopes := []struct {
				name string
				argv []string
			}{
				{"system", []string{"systemctl", "--failed", "--no-legend", "--plain"}},
				{"user", []string{"systemctl", "--user", "--failed", "--no-legend", "--plain"}},
			}
			ignored := e.expectedFailures()
			var failed []string
			var unreadable []string
			for _, scope := range scopes {
				out, err := e.run(ctx, "", scope.argv)
				if err != nil {
					unreadable = append(unreadable, scope.name)
					continue
				}
				for _, line := range strings.Split(string(out), "\n") {
					fields := strings.Fields(line)
					if len(fields) == 0 {
						continue
					}
					if ignored[fields[0]] {
						continue
					}
					failed = append(failed, scope.name+":"+fields[0])
				}
			}
			if len(unreadable) == len(scopes) {
				return contract.Unknown, "systemctl could not report failed units", nil
			}
			var notes []string
			if len(ignored) > 0 {
				notes = append(notes, strings.Join(sortedKeys(ignored), ", ")+" fails by design on a read-only root")
			}
			if len(unreadable) > 0 {
				notes = append(notes, strings.Join(unreadable, " and ")+" scope unreadable")
			}
			note := ""
			if len(notes) > 0 {
				note = " (" + strings.Join(notes, "; ") + ")"
			}
			if len(failed) > 0 {
				return contract.NotLive, fmt.Sprintf("%d failed units (%s)%s", len(failed), strings.Join(firstN(failed, 3), ", "), note), nil
			}
			return contract.Live, "no failed units" + note, nil
		},
	}
}

// expectedFailures names the units that cannot succeed on this host.
func (e *Engine) expectedFailures() map[string]bool {
	ignored := map[string]bool{}
	if e.family != adapter.FedoraAtomic && e.family != adapter.Bootc {
		return ignored
	}
	root, err := e.statfs("/")
	if err != nil || !root.ReadOnly {
		return ignored
	}
	ignored["systemd-remount-fs.service"] = true
	return ignored
}

// sortedKeys returns the keys of a set in sorted order.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// diskCheck reports free space on the root filesystem and the home directory.
func (e *Engine) diskCheck() check {
	return check{
		id:   "disk",
		term: term.NeedsOperator,
		run: func(context.Context) (contract.State, string, error) {
			targets := []struct{ name, path string }{{"root", "/"}}
			if e.home != "" {
				targets = append(targets, struct{ name, path string }{"home", e.home})
			}
			state := contract.Live
			checked := 0
			var details []string
			for _, target := range targets {
				stat, err := e.statfs(target.path)
				if err != nil {
					return contract.Unknown, fmt.Sprintf("read %s free space: %v", target.name, err), nil
				}
				if stat.ReadOnly {
					// An immutable root such as composefs is read-only by
					// design and always reads full. Its space is not the
					// operator's to manage, so it is not a finding.
					details = append(details, target.name+" read-only")
					continue
				}
				if stat.Total == 0 {
					continue
				}
				checked++
				percent := stat.Avail * 100 / stat.Total
				if stat.Avail < diskMinBytes || percent < diskMinPercent {
					state = contract.NotLive
				}
				details = append(details, fmt.Sprintf("%s %s free (%d%%)", target.name, humanGiB(stat.Avail), percent))
			}
			if checked == 0 {
				return contract.Unknown, "no writable filesystem to measure (" + strings.Join(details, ", ") + ")", nil
			}
			return state, strings.Join(details, ", "), nil
		},
	}
}

// firstN returns at most n items.
func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// humanGiB renders a byte count in GiB with one decimal.
func humanGiB(bytes uint64) string {
	return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 1, 64) + " GiB"
}

// loginShell returns the login shell named for the current uid in /etc/passwd.
func loginShell() (string, error) {
	body, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "", fmt.Errorf("read /etc/passwd: %w", err)
	}
	uid := strconv.Itoa(os.Getuid())
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[2] != uid {
			continue
		}
		return fields[6], nil
	}
	return "", fmt.Errorf("no password entry for uid %s", uid)
}

// fsStat is the free-space view of one filesystem.
type fsStat struct {
	Avail    uint64
	Total    uint64
	ReadOnly bool
}

// statfs returns available and total bytes for path, and whether the mount is
// read-only.
func statfs(path string) (fsStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fsStat{}, err
	}
	bsize := uint64(st.Bsize)
	return fsStat{
		Avail:    st.Bavail * bsize,
		Total:    st.Blocks * bsize,
		ReadOnly: st.Flags&stReadOnly != 0,
	}, nil
}

// runOutput runs argv in dir and returns stdout. A non-zero exit carries the
// last non-empty stderr line, so a failed host tool explains itself.
func runOutput(ctx context.Context, dir string, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, &exitError{Code: exit.ExitCode(), Msg: lastLine(stderr.String()), Stderr: strings.TrimSpace(stderr.String())}
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// exitError carries a command's exit status. A caller that must tell "no
// match" from an unreachable remote reads Code, and a caller that classifies a
// failure reads the full Stderr.
type exitError struct {
	Code   int
	Msg    string
	Stderr string
}

// Error renders the exit status and the last stderr line.
func (e *exitError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("exit %d: %s", e.Code, e.Msg)
	}
	return fmt.Sprintf("exit %d", e.Code)
}

// lastLine returns the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
