package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/state"
)

// laresBinary is built once, so every test drives the real process boundary and
// the documented exit codes are observed rather than assumed.
var laresBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lares-cli-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create build directory: %v\n", err)
		os.Exit(1)
	}
	if _, err := exec.LookPath("go"); err != nil {
		fmt.Fprintln(os.Stderr, "the cmd/lares smoke tests build the binary and need the Go toolchain on PATH")
		os.RemoveAll(dir)
		os.Exit(1)
	}
	laresBinary = filepath.Join(dir, "lares")
	build := exec.Command("go", "build", "-o", laresBinary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build lares with %v: %v\n%s", build.Args, err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// cli is one hermetic install: a home, a state dir, a config, and a probe.
type cli struct {
	binary    string
	home      string
	state     string
	configDir string
	repoDir   string
	config    string
	probe     string
	env       []string
}

// newCLI writes a valid config and a clean probe into a temp root.
func newCLI(t *testing.T) *cli {
	t.Helper()
	root := t.TempDir()
	xdgState := filepath.Join(root, "xdg-state")
	c := &cli{
		binary:    laresBinary,
		home:      filepath.Join(root, "home"),
		state:     filepath.Join(xdgState, "lares"),
		configDir: filepath.Join(root, "xdg-config", "lares"),
		repoDir:   filepath.Join(root, "repo"),
		probe:     filepath.Join(root, "probe.sh"),
	}
	for _, dir := range []string{c.home, c.configDir, c.repoDir, filepath.Dir(xdgState)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	c.config = filepath.Join(c.configDir, "config.toml")
	c.writeProbe(t, `printf '%s\n' '{"id":"profile","state":"live","detail":"","fix":""}' '{"id":"summary","state":"live","detail":"0 findings","fix":""}'`)
	c.writeConfig(t)
	c.env = append(os.Environ(),
		"HOME="+c.home,
		"XDG_CONFIG_HOME="+filepath.Dir(c.configDir),
		"XDG_STATE_HOME="+xdgState,
		"USER=operator",
		"LARES_REPO="+c.repoDir,
	)
	return c
}

// writeConfig writes the config every command loads.
//
// The reboot gate is off, so a pass never reboots or queues a reboot on the test
// host.
func (c *cli) writeConfig(t *testing.T) {
	t.Helper()
	body := fmt.Sprintf(`audit_command = ["bash", %q]
state_dir = %q
config_dir = %q
repo_dir = %q
notify = "stdout"
reboot_from_pass = false
`, c.probe, c.state, c.configDir, c.repoDir)
	if err := os.WriteFile(c.config, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// writeProbe replaces the probe script. The contract reader appends --json, so
// the script ignores its arguments.
func (c *cli) writeProbe(t *testing.T, body string) {
	t.Helper()
	script := "#!/usr/bin/env bash\nset -euo pipefail\n" + body + "\n"
	if err := os.WriteFile(c.probe, []byte(script), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}
}

// run executes the binary with the hermetic environment.
func (c *cli) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(c.binary, args...)
	cmd.Env = c.env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return out.String(), errOut.String(), code
}

// verdicts decodes the append-only log, or nil when nothing was written.
func (c *cli) verdicts(t *testing.T) []state.Verdict {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.state, "verdicts.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read verdict log: %v", err)
	}
	var verdicts []state.Verdict
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var verdict state.Verdict
		if err := json.Unmarshal([]byte(line), &verdict); err != nil {
			t.Fatalf("decode verdict %q: %v", line, err)
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts
}

func TestVersionCommand(t *testing.T) {
	c := newCLI(t)
	stdout, stderr, code := c.run(t, "version")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if got := strings.TrimSpace(stdout); got != version {
		t.Errorf("version %q, want %q", got, version)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	c := newCLI(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "Usage:"},
		{"unknown command", []string{"nonsense"}, `unknown command "nonsense"`},
		{"hold without an action", []string{"--config", c.config, "hold"}, "expected add, exec, list, or rm"},
		{"hold rm without an owner", []string{"--config", c.config, "hold", "rm"}, "expected an owner"},
		{"defer without arguments", []string{"--config", c.config, "defer"}, "expected <id> <duration>"},
		{"install without --user", []string{"--config", c.config, "install"}, "only --user is supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := c.run(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2, stderr %q", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not contain %q", stderr, tc.want)
			}
		})
	}
}

func TestBadConfigExitsThree(t *testing.T) {
	c := newCLI(t)
	bad := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(bad, []byte("resync = \"fast\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := c.run(t, "--config", bad, "status")
	if code != 3 {
		t.Fatalf("exit %d, want 3, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "load config") {
		t.Errorf("stderr %q does not name the config load", stderr)
	}
}

func TestMeasureJSONAndExitOne(t *testing.T) {
	c := newCLI(t)
	c.writeProbe(t, `printf '%s\n' '{"id":"offsite","state":"not-live","detail":"no repo","fix":"install/offsite-oauth-wizard.sh"}' '{"id":"summary","state":"not-live","detail":"1 finding","fix":""}'`)

	stdout, stderr, code := c.run(t, "--config", c.config, "measure", "--json")
	if code != 1 {
		t.Fatalf("exit %d, want 1, stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("printed %d lines, want one per record:\n%s", len(lines), stdout)
	}
	var first, last contract.Record
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode first record: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &last); err != nil {
		t.Fatalf("decode last record: %v", err)
	}
	if first.ID != "offsite" || first.State != contract.NotLive {
		t.Errorf("first record %+v, want offsite not-live", first)
	}
	if !last.IsSummary() {
		t.Errorf("last record %+v is not the summary", last)
	}

	verdicts := c.verdicts(t)
	if len(verdicts) != 1 {
		t.Fatalf("wrote %d verdicts, want one per non-summary record", len(verdicts))
	}
	if verdicts[0].ID != "offsite" || verdicts[0].State != string(contract.NotLive) {
		t.Errorf("verdict %+v, want offsite not-live", verdicts[0])
	}
	if verdicts[0].Fix != "install/offsite-oauth-wizard.sh" {
		t.Errorf("verdict fix %q, want the probe's command", verdicts[0].Fix)
	}
}

func TestPassPrintsTheNoticeOnce(t *testing.T) {
	c := newCLI(t)
	c.writeProbe(t, `printf '%s\n' '{"id":"offsite","state":"not-live","detail":"no repo","fix":"install/offsite-oauth-wizard.sh"}' '{"id":"summary","state":"not-live","detail":"1 finding","fix":""}'`)

	stdout, stderr, code := c.run(t, "--config", c.config, "pass")
	if code != 1 {
		t.Fatalf("exit %d, want 1, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "lares: offsite not-live: no repo") {
		t.Errorf("stdout does not carry the notice:\n%s", stdout)
	}

	again, _, code := c.run(t, "--config", c.config, "pass")
	if code != 1 {
		t.Fatalf("second pass exit %d, want 1", code)
	}
	if strings.Contains(again, "lares: offsite") {
		t.Errorf("second pass repeated the notice:\n%s", again)
	}
}

func TestMeasureCleanRunExitsZero(t *testing.T) {
	c := newCLI(t)
	_, stderr, code := c.run(t, "--config", c.config, "measure", "--json")
	if code != 0 {
		t.Fatalf("exit %d, want 0 for a clean measure, stderr %q", code, stderr)
	}
}

func TestProbeFailureExitsThree(t *testing.T) {
	c := newCLI(t)
	c.writeProbe(t, `echo boom >&2
exit 4`)
	_, stderr, code := c.run(t, "--config", c.config, "measure")
	if code != 3 {
		t.Fatalf("exit %d, want 3, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "exit 4") || !strings.Contains(stderr, "boom") {
		t.Errorf("stderr %q does not name the probe failure", stderr)
	}
}

func TestStatusJSONShape(t *testing.T) {
	c := newCLI(t)
	c.writeProbe(t, `printf '%s\n' '{"id":"offsite","state":"not-live","detail":"no repo","fix":"install/offsite-oauth-wizard.sh"}' '{"id":"summary","state":"not-live","detail":"1 finding","fix":""}'`)
	if _, stderr, code := c.run(t, "--config", c.config, "measure"); code != 1 {
		t.Fatalf("measure exit %d, stderr %q", code, stderr)
	}

	stdout, stderr, code := c.run(t, "--config", c.config, "status", "--json")
	if code != 0 {
		t.Fatalf("status exit %d, stderr %q", code, stderr)
	}
	var payload struct {
		Queue    state.Queue              `json:"queue"`
		Holds    []state.Hold             `json:"holds"`
		Verdicts map[string]state.Verdict `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("decode status: %v\n%s", err, stdout)
	}
	verdict, ok := payload.Verdicts["offsite"]
	if !ok {
		t.Fatalf("status has no offsite verdict: %s", stdout)
	}
	if verdict.State != string(contract.NotLive) || verdict.Detail != "no repo" {
		t.Errorf("verdict %+v, want the measured state and detail", verdict)
	}
}

func TestHoldExecHoldsAndReleases(t *testing.T) {
	c := newCLI(t)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	// A failing assertion must not leave the child waiting forever.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	// The child lives until the test releases it, so the hold is always observed
	// inside the command's lifetime and a slow runner cannot turn this into a
	// race against a fixed sleep.
	script := fmt.Sprintf("echo up > %q\nwhile [ ! -f %q ]; do sleep 0.02; done", started, release)
	cmd := exec.Command(c.binary, "--config", c.config, "hold", "exec", "--", "sh", "-c", script)
	cmd.Env = c.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hold exec: %v", err)
	}

	waitForFile(t, started)
	hold := c.awaitHold(t)
	if want := fmt.Sprintf("pid-%d", hold.PID); hold.Owner != want {
		t.Errorf("owner %q, want %q", hold.Owner, want)
	}
	if hold.PID <= 0 || !state.ProcessAlive(hold.PID) {
		t.Errorf("hold pid %d is not live while the command runs", hold.PID)
	}
	if !strings.HasPrefix(hold.Reason, "sh -c ") {
		t.Errorf("reason %q, want the command line", hold.Reason)
	}
	if hold.StartTicks == 0 {
		t.Error("hold carries no start time, so a reused pid could claim it")
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release the command: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("hold exec: %v, stderr %q", err, stderr.String())
	}
	entries, err := os.ReadDir(filepath.Join(c.state, "holds"))
	if err != nil {
		t.Fatalf("read holds: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d holds left after the command exited", len(entries))
	}
}

func TestHoldExecPropagatesTheExitCode(t *testing.T) {
	c := newCLI(t)
	_, stderr, code := c.run(t, "--config", c.config, "hold", "exec", "--", "sh", "-c", "exit 7")
	if code != 7 {
		t.Fatalf("exit %d, want the command's 7, stderr %q", code, stderr)
	}
}

func TestHoldAddRejectsADeadPID(t *testing.T) {
	c := newCLI(t)
	_, stderr, code := c.run(t, "--config", c.config, "hold", "add", "--pid", deadPID(t), "nothing runs")
	if code != 2 {
		t.Fatalf("exit %d, want 2, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "is not running") {
		t.Errorf("stderr %q does not name the dead pid", stderr)
	}
}

func TestDeferFloorAndVerdict(t *testing.T) {
	c := newCLI(t)

	_, stderr, code := c.run(t, "--config", c.config, "defer", "offsite", "30s")
	if code != 2 {
		t.Fatalf("exit %d, want 2 for a 30s deferral, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "at least 1m") {
		t.Errorf("stderr %q does not name the floor", stderr)
	}

	before := time.Now()
	stdout, stderr, code := c.run(t, "--config", c.config, "defer", "offsite", "2h")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "deferred offsite until") {
		t.Errorf("stdout %q does not report the deferral", stdout)
	}

	var record *state.Verdict
	verdicts := c.verdicts(t)
	for i := range verdicts {
		if verdicts[i].Kind == state.KindDefer {
			record = &verdicts[i]
		}
	}
	if record == nil {
		t.Fatal("no defer record in the verdict log")
	}
	if record.ID != "offsite" || record.Detail != "deferred from the CLI" {
		t.Errorf("defer record %+v, want the item and the CLI reason", record)
	}
	if record.Until.Before(before.Add(2 * time.Hour)) {
		t.Errorf("until %v is earlier than two hours from %v", record.Until, before)
	}
}

func TestInstallWritesNoSudoers(t *testing.T) {
	c := newCLI(t)
	stdout, stderr, code := c.run(t, "--config", c.config, "install", "--user")
	if code != 0 {
		t.Fatalf("install exit %d, stderr %q", code, stderr)
	}

	paths := []string{
		filepath.Join(c.home, ".local", "bin", "lares"),
		filepath.Join(c.home, ".config", "systemd", "user", "lares.service"),
		filepath.Join(c.home, ".config", "systemd", "user", "lares.timer"),
		filepath.Join(c.home, ".config", "lares", "config.toml"),
		filepath.Join(c.home, ".config", "lares", "adapters.toml"),
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("installed file: %v", err)
		}
		if !strings.Contains(stdout, path) {
			t.Errorf("stdout does not list %s", path)
		}
	}

	if _, err := os.Stat(filepath.Join(c.home, ".config", "lares", "lares.sudoers")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("installer wrote a sudoers file: %v", err)
	}
	if strings.Contains(stdout, "sudoers") || strings.Contains(stdout, "sudo install") {
		t.Errorf("stdout offers a privilege grant:\n%s", stdout)
	}
	if !strings.Contains(stdout, "loginctl enable-linger operator") {
		t.Errorf("stdout does not print the one command the operator runs:\n%s", stdout)
	}

	config, err := os.ReadFile(paths[3])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), fmt.Sprintf("repo_dir = %q", c.repoDir)) {
		t.Errorf("config does not record the checkout:\n%s", config)
	}
	adapters, err := os.ReadFile(paths[4])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(adapters), "[bootc]") {
		t.Errorf("adapter defaults missing bootc:\n%s", adapters)
	}
}

// deadPID returns a pid no process can hold: the kernel wraps at pid_max, so
// the maximum usable pid is one less and pid_max itself is never allocated.
func deadPID(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/kernel/pid_max")
	if err != nil {
		t.Fatalf("read pid_max: %v", err)
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		t.Fatal("pid_max is empty")
	}
	return pid
}

// waitForFile waits for a path to exist, so a test can observe a child that is
// alive rather than sleep for a guessed interval.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// awaitHold waits for hold exec to write its hold, then reads it.
func (c *cli) awaitHold(t *testing.T) state.Hold {
	t.Helper()
	dir := filepath.Join(c.state, "holds")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatalf("read holds: %v", err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			var hold state.Hold
			if json.Unmarshal(data, &hold) == nil && hold.PID > 0 {
				return hold
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("hold exec wrote no hold file while the command ran")
	return state.Hold{}
}
