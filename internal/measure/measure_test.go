package measure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/term"
)

// testConfig is a valid config whose directories live under dir.
func testConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = dir
	cfg.ConfigDir = dir
	cfg.ReposDir = dir
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return cfg
}

// fakeRun returns canned output for one command line and records every call.
type fakeRun struct {
	output map[string]string
	errs   map[string]error
	calls  [][]string
}

// run is the fake's command hook.
func (f *fakeRun) run(_ context.Context, _ string, argv []string) ([]byte, error) {
	line := strings.Join(argv, " ")
	f.calls = append(f.calls, argv)
	if err := f.errs[line]; err != nil {
		return nil, err
	}
	if out, ok := f.output[line]; ok {
		return []byte(out), nil
	}
	return nil, fmt.Errorf("unexpected command %q", line)
}

// newEngine builds an engine over a config, a family, and explicit checks.
func newEngine(t *testing.T, cfg config.Config, family adapter.Family, overrides ...func(*Options)) *Engine {
	t.Helper()
	opts := Options{
		Config:      cfg,
		Family:      family,
		Home:        cfg.ReposDir,
		Now:         func() time.Time { return time.Date(2026, 11, 16, 3, 0, 0, 0, time.Local) },
		LookupShell: func() (string, error) { return "/bin/sh", nil },
	}
	for _, override := range overrides {
		override(&opts)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// recordsByID indexes a measurement.
func recordsByID(t *testing.T, e *Engine) map[string]contract.Record {
	t.Helper()
	records, err := e.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	out := make(map[string]contract.Record, len(records))
	for _, rec := range records {
		out[rec.ID] = rec
	}
	return out
}

func TestMeasureAppendsSummaryLast(t *testing.T) {
	dir := t.TempDir()
	e := newEngine(t, testConfig(t, dir), adapter.FedoraAtomic, func(o *Options) {
		o.Run = (&fakeRun{output: map[string]string{
			"rpm-ostree status --json":                      `{"deployments":[{"booted":true,"staged":false,"version":"44.1"}]}`,
			"systemctl --failed --no-legend --plain":        "",
			"systemctl --user --failed --no-legend --plain": "",
		}}).run
		o.Statfs = func(string) (fsStat, error) { return fsStat{Avail: 100 << 30, Total: 200 << 30}, nil }
	})
	records, err := e.Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if last := records[len(records)-1]; !last.IsSummary() {
		t.Fatalf("last record is %q, want summary", last.ID)
	}
	if got := records[len(records)-1].State; got != contract.Live {
		t.Errorf("summary state %q, want live", got)
	}
	if len(records) != 5 {
		t.Errorf("got %d records, want 4 checks and a summary", len(records))
	}
}

func TestShellCheck(t *testing.T) {
	tests := []struct {
		name  string
		shell string
		err   error
		want  contract.State
	}{
		{"executable", "/bin/sh", nil, contract.Live},
		{"missing", "/nonexistent/shell", nil, contract.NotLive},
		{"no entry", "", nil, contract.NotLive},
		{"read fails", "", errors.New("no passwd"), contract.Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEngine(t, testConfig(t, t.TempDir()), adapter.Arch, func(o *Options) {
				o.LookupShell = func() (string, error) { return tt.shell, tt.err }
			})
			rec := recordsByID(t, e)["shell"]
			if rec.State != tt.want {
				t.Errorf("state %q (%s), want %q", rec.State, rec.Detail, tt.want)
			}
		})
	}
}

func TestPatchTrainStagedDeployment(t *testing.T) {
	body := `{"deployments":[{"booted":true,"staged":false,"version":"44.1"},{"booted":false,"staged":true,"version":"44.2"}]}`
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.FedoraAtomic, func(o *Options) {
		o.Run = (&fakeRun{output: map[string]string{"rpm-ostree status --json": body}}).run
	})
	rec := recordsByID(t, e)["patch-train"]
	if rec.State != contract.Stale {
		t.Fatalf("state %q, want stale", rec.State)
	}
	if !strings.Contains(rec.Detail, "44.2") {
		t.Errorf("detail %q does not name the staged version", rec.Detail)
	}
	if e.Safe(rec) {
		t.Error("a staged deployment must not be repaired unattended")
	}
	if got := e.Term(rec); got != term.NeedsGrant {
		t.Errorf("term %q, want %q", got, term.NeedsGrant)
	}
}

func TestPatchTrainNoStagedDeployment(t *testing.T) {
	body := `{"deployments":[{"booted":true,"staged":false,"version":"44.1"}]}`
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.FedoraAtomic, func(o *Options) {
		o.Run = (&fakeRun{output: map[string]string{"rpm-ostree status --json": body}}).run
	})
	if rec := recordsByID(t, e)["patch-train"]; rec.State != contract.Live {
		t.Errorf("state %q (%s), want live", rec.State, rec.Detail)
	}
}

func TestPatchTrainFallsBackToBootc(t *testing.T) {
	out := &fakeRun{
		errs: map[string]error{
			"rpm-ostree status --json":   errors.New("not found"),
			"ostree admin status --json": errors.New("not found"),
		},
		output: map[string]string{"bootc status --format json": `{"status":{"booted":{"image":{"version":"bluefin-44"}},"staged":null}}`},
	}
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Bootc, func(o *Options) { o.Run = out.run })
	if rec := recordsByID(t, e)["patch-train"]; rec.State != contract.Live {
		t.Errorf("state %q (%s), want live", rec.State, rec.Detail)
	}
}

func TestPatchTrainFallsBackToOSTreeAdmin(t *testing.T) {
	const checksum = "dac5db3210bc87b257ce459ddf6aa6f1a1e6e98f06a74a4066ebee508125745e"
	body := fmt.Sprintf(`{"deployments":[{"booted":true,"staged":false,"checksum":"59b6f53c09085245cbea60545756c36a263f07431311d2a41cd4e903b10abba2"},{"booted":false,"staged":true,"checksum":%q}]}`, checksum)
	out := &fakeRun{
		errs: map[string]error{
			// bootc needs root on the Universal Blue images, so the
			// unprivileged ostree read must answer first.
			"rpm-ostree status --json":   errors.New("not found"),
			"bootc status --format json": errors.New("must be root"),
		},
		output: map[string]string{"ostree admin status --json": body},
	}
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Bootc, func(o *Options) { o.Run = out.run })
	rec := recordsByID(t, e)["patch-train"]
	if rec.State != contract.Stale {
		t.Fatalf("state %q (%s), want stale", rec.State, rec.Detail)
	}
	if !strings.Contains(rec.Detail, checksum[:12]) {
		t.Errorf("detail %q does not name the staged checksum", rec.Detail)
	}
}

func TestPatchTrainUnreadableIsUnknown(t *testing.T) {
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Bootc, func(o *Options) {
		o.Run = (&fakeRun{errs: map[string]error{
			"rpm-ostree status --json":   errors.New("no rpm-ostree"),
			"ostree admin status --json": errors.New("no ostree"),
			"bootc status --format json": errors.New("must be root"),
		}}).run
	})
	if rec := recordsByID(t, e)["patch-train"]; rec.State != contract.Unknown {
		t.Errorf("state %q (%s), want unknown", rec.State, rec.Detail)
	}
}

func TestPatchTrainUnknownFamilyIsUnknown(t *testing.T) {
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Unknown)
	if rec := recordsByID(t, e)["patch-train"]; rec.State != contract.Unknown {
		t.Errorf("state %q, want unknown", rec.State)
	}
}

func TestSystemdFailed(t *testing.T) {
	tests := []struct {
		name       string
		system     string
		user       string
		sysOK      bool
		userOK     bool
		want       contract.State
		wantDetail string
	}{
		{"none", "", "", true, true, contract.Live, ""},
		{"system", "foo.service loaded failed failed Broke\n", "", true, true, contract.NotLive, ""},
		{"user", "", "bar.service loaded failed failed Broke\n", true, true, contract.NotLive, ""},
		{"user unreadable", "", "", true, false, contract.Live, "user scope unreadable"},
		{"system unreadable", "", "bar.service loaded failed failed Broke\n", false, true, contract.NotLive, "system scope unreadable"},
		{"both unreadable", "", "", false, false, contract.Unknown, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRun{output: map[string]string{
				"systemctl --failed --no-legend --plain":        tt.system,
				"systemctl --user --failed --no-legend --plain": tt.user,
			}}
			errs := map[string]error{}
			if !tt.sysOK {
				errs["systemctl --failed --no-legend --plain"] = errors.New("no bus")
			}
			if !tt.userOK {
				errs["systemctl --user --failed --no-legend --plain"] = errors.New("no bus")
			}
			if len(errs) > 0 {
				fake.errs = errs
			}
			e := newEngine(t, testConfig(t, t.TempDir()), adapter.Arch, func(o *Options) { o.Run = fake.run })
			rec := recordsByID(t, e)["systemd-failed"]
			if rec.State != tt.want {
				t.Errorf("state %q (%s), want %q", rec.State, rec.Detail, tt.want)
			}
			if tt.wantDetail != "" && !strings.Contains(rec.Detail, tt.wantDetail) {
				t.Errorf("detail %q does not name %q", rec.Detail, tt.wantDetail)
			}
		})
	}
}

func TestSystemdRemountFailureOnAnImmutableRoot(t *testing.T) {
	const remount = "systemd-remount-fs.service loaded failed failed Remount Root and Kernel File Systems\n"
	tests := []struct {
		name     string
		family   adapter.Family
		readOnly bool
		want     contract.State
	}{
		{"bootc read-only root", adapter.Bootc, true, contract.Live},
		{"fedora atomic read-only root", adapter.FedoraAtomic, true, contract.Live},
		{"bootc writable root", adapter.Bootc, false, contract.NotLive},
		{"arch read-only root", adapter.Arch, true, contract.NotLive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeRun{output: map[string]string{
				"systemctl --failed --no-legend --plain":        remount,
				"systemctl --user --failed --no-legend --plain": "",
			}}
			e := newEngine(t, testConfig(t, t.TempDir()), tt.family, func(o *Options) {
				o.Run = fake.run
				o.Statfs = func(string) (fsStat, error) { return fsStat{ReadOnly: tt.readOnly}, nil }
			})
			rec := recordsByID(t, e)["systemd-failed"]
			if rec.State != tt.want {
				t.Fatalf("state %q (%s), want %q", rec.State, rec.Detail, tt.want)
			}
			if tt.want == contract.Live && !strings.Contains(rec.Detail, "systemd-remount-fs.service") {
				t.Errorf("detail %q does not name the excluded unit", rec.Detail)
			}
		})
	}
}

func TestDiskCheck(t *testing.T) {
	tests := []struct {
		name  string
		avail uint64
		total uint64
		want  contract.State
	}{
		{"plenty", 100 << 30, 200 << 30, contract.Live},
		{"low bytes", 1 << 30, 200 << 30, contract.NotLive},
		{"low percent", 4 << 30, 200 << 30, contract.NotLive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEngine(t, testConfig(t, t.TempDir()), adapter.Arch, func(o *Options) {
				o.Statfs = func(string) (fsStat, error) { return fsStat{Avail: tt.avail, Total: tt.total}, nil }
			})
			if rec := recordsByID(t, e)["disk"]; rec.State != tt.want {
				t.Errorf("state %q (%s), want %q", rec.State, rec.Detail, tt.want)
			}
		})
	}
}

func TestDiskCheckIgnoresReadOnlyRoot(t *testing.T) {
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Arch, func(o *Options) {
		o.Statfs = func(path string) (fsStat, error) {
			if path == "/" {
				// An immutable root such as composefs reads full by design.
				return fsStat{Avail: 0, Total: 34 << 20, ReadOnly: true}, nil
			}
			return fsStat{Avail: 100 << 30, Total: 200 << 30}, nil
		}
	})
	rec := recordsByID(t, e)["disk"]
	if rec.State != contract.Live {
		t.Fatalf("state %q (%s), want live: a read-only root is not a finding", rec.State, rec.Detail)
	}
	if !strings.Contains(rec.Detail, "root read-only") {
		t.Errorf("detail %q does not report the read-only root", rec.Detail)
	}
}

func TestDiskCheckAllReadOnlyIsUnknown(t *testing.T) {
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Arch, func(o *Options) {
		o.Statfs = func(string) (fsStat, error) {
			return fsStat{Avail: 0, Total: 34 << 20, ReadOnly: true}, nil
		}
	})
	if rec := recordsByID(t, e)["disk"]; rec.State != contract.Unknown {
		t.Errorf("state %q (%s), want unknown when no writable filesystem was measured", rec.State, rec.Detail)
	}
}

func TestDiskCheckUnreadableIsUnknown(t *testing.T) {
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Arch, func(o *Options) {
		o.Statfs = func(string) (fsStat, error) { return fsStat{}, errors.New("no statfs") }
	})
	if rec := recordsByID(t, e)["disk"]; rec.State != contract.Unknown {
		t.Errorf("state %q, want unknown", rec.State)
	}
}

func TestRepoDiscoveryIsBoundedToImmediateChildren(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "not-a-repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A nested checkout is not an immediate child, so it is not enumerated.
	if err := os.MkdirAll(filepath.Join(root, "alpha", "nested", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, testConfig(t, root), adapter.Arch)
	repos, err := e.sortedRepos()
	if err != nil {
		t.Fatalf("sortedRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("repos %v, want alpha and beta", repos)
	}
	if filepath.Base(repos[0]) != "alpha" || filepath.Base(repos[1]) != "beta" {
		t.Errorf("repos %v are not sorted by path", repos)
	}
}

func TestRepoDiscoveryUnreadableIsUnknown(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	e := newEngine(t, cfg, adapter.Arch, func(o *Options) {
		o.ReadDir = func(string) ([]os.DirEntry, error) {
			return nil, errors.New("permission denied")
		}
	})
	records := recordsByID(t, e)
	rec, ok := records["repo-discovery"]
	if !ok {
		t.Fatal("no repo-discovery record; an unreadable repos_dir was silently skipped")
	}
	if rec.State != contract.Unknown {
		t.Errorf("state %q (%s), want unknown", rec.State, rec.Detail)
	}
}

func TestExplicitReposOverrideDiscovery(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t, root)
	cfg.Repos = []string{"a", "/abs/b"}
	e := newEngine(t, cfg, adapter.Arch)
	repos, err := e.sortedRepos()
	if err != nil {
		t.Fatalf("sortedRepos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("repos %v", repos)
	}
	want := map[string]bool{
		filepath.Join(root, "a"): true,
		"/abs/b":                 true,
	}
	for _, repo := range repos {
		if !want[repo] {
			t.Errorf("unexpected repo %q; a relative entry must anchor at home and an absolute one must survive", repo)
		}
	}
}

func TestSlugify(t *testing.T) {
	tests := map[string]string{
		"order-processing": "order-processing",
		"Vera":             "vera",
		"a b":              "a-b",
		"  weird!!name  ":  "weird-name",
		"@@@":              "",
	}
	for in, want := range tests {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoIDDeduplicates(t *testing.T) {
	used := map[string]bool{}
	if got := repoID("/x/a", used); got != "repo-a" {
		t.Errorf("id %q", got)
	}
	if got := repoID("/y/a", used); got != "repo-a-2" {
		t.Errorf("second id %q", got)
	}
	if got := repoID("/z/a", used); got != "repo-a-3" {
		t.Errorf("third id %q", got)
	}
}

// repoEngine builds an engine over one explicit checkout with a fake git.
func repoEngine(t *testing.T, dir string, fake *fakeRun) *Engine {
	t.Helper()
	cfg := testConfig(t, dir)
	cfg.Repos = []string{dir}
	return newEngine(t, cfg, adapter.Arch, func(o *Options) { o.Run = fake.run })
}

func TestRepoBehindIsStaleAndSafe(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRun{output: map[string]string{
		"git fetch --prune --quiet":                                   "",
		"git status --porcelain=v2 --branch --untracked-files=normal": "# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -3\n",
		"git worktree list --porcelain":                               "",
	}}
	e := repoEngine(t, dir, fake)
	rec := recordsByID(t, e)["repo-"+slugify(filepath.Base(dir))]
	if rec.State != contract.Stale {
		t.Fatalf("state %q (%s), want stale", rec.State, rec.Detail)
	}
	if !e.Safe(rec) {
		t.Error("a clean checkout behind its upstream must be safe")
	}
	if rec.Fix != "fast-forward" {
		t.Errorf("fix %q, want fast-forward", rec.Fix)
	}
}

func TestRepoDirtyIsNotSafe(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRun{output: map[string]string{
		"git fetch --prune --quiet":                                   "",
		"git status --porcelain=v2 --branch --untracked-files=normal": "# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -3\n1 .M N... 100644 100644 100644 abc abc file.go\n",
		"git worktree list --porcelain":                               "",
	}}
	e := repoEngine(t, dir, fake)
	rec := recordsByID(t, e)["repo-"+slugify(filepath.Base(dir))]
	if rec.State != contract.NotLive {
		t.Fatalf("state %q, want not-live", rec.State)
	}
	if e.Safe(rec) {
		t.Error("a dirty checkout must not be safe")
	}
	if got := e.Term(rec); got != term.NeedsOperator {
		t.Errorf("term %q", got)
	}
}

func TestRepoAheadIsNotSafe(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRun{output: map[string]string{
		"git fetch --prune --quiet":                                   "",
		"git status --porcelain=v2 --branch --untracked-files=normal": "# branch.head main\n# branch.upstream origin/main\n# branch.ab +2 -0\n",
		"git worktree list --porcelain":                               "",
	}}
	e := repoEngine(t, dir, fake)
	rec := recordsByID(t, e)["repo-"+slugify(filepath.Base(dir))]
	if rec.State != contract.NotLive || e.Safe(rec) {
		t.Errorf("state %q safe=%v, want not-live and unsafe", rec.State, e.Safe(rec))
	}
}

func TestRepoDetachedIsNotSafe(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRun{output: map[string]string{
		"git fetch --prune --quiet":                                   "",
		"git status --porcelain=v2 --branch --untracked-files=normal": "# branch.head (detached)\n",
		"git worktree list --porcelain":                               "",
	}}
	e := repoEngine(t, dir, fake)
	rec := recordsByID(t, e)["repo-"+slugify(filepath.Base(dir))]
	if rec.State != contract.NotLive || e.Safe(rec) {
		t.Errorf("state %q safe=%v, want not-live and unsafe", rec.State, e.Safe(rec))
	}
}

func TestRepoCleanIsLive(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRun{output: map[string]string{
		"git fetch --prune --quiet":                                   "",
		"git status --porcelain=v2 --branch --untracked-files=normal": "# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -0\n",
		"git worktree list --porcelain":                               "",
	}}
	e := repoEngine(t, dir, fake)
	if rec := recordsByID(t, e)["repo-"+slugify(filepath.Base(dir))]; rec.State != contract.Live {
		t.Errorf("state %q (%s), want live", rec.State, rec.Detail)
	}
}

func TestRepoPrunableWorktreesAreStale(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRun{output: map[string]string{
		"git fetch --prune --quiet":                                   "",
		"git status --porcelain=v2 --branch --untracked-files=normal": "# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -0\n",
		"git worktree list --porcelain":                               "worktree /gone\nHEAD abc\nbranch refs/heads/old\nprunable gitdir file points to non-existent location\n",
	}}
	e := repoEngine(t, dir, fake)
	rec := recordsByID(t, e)["repo-"+slugify(filepath.Base(dir))]
	if rec.State != contract.Stale || !e.Safe(rec) {
		t.Errorf("state %q safe=%v, want stale and safe", rec.State, e.Safe(rec))
	}
}

func TestRepoRepairFastForwards(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRun{output: map[string]string{
		"git worktree prune --verbose":                                "",
		"git fetch --prune --quiet":                                   "",
		"git status --porcelain=v2 --branch --untracked-files=normal": "# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -2\n",
		"git worktree list --porcelain":                               "",
		"git merge --ff-only origin/main":                             "Updating abc..def\nFast-forward\n",
	}}
	e := repoEngine(t, dir, fake)
	id := "repo-" + slugify(filepath.Base(dir))
	rec := recordsByID(t, e)[id]
	out, err := e.Repair(context.Background(), rec)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if !strings.Contains(out, "Fast-forward") {
		t.Errorf("output %q does not record the merge", out)
	}
	found := false
	for _, call := range fake.calls {
		if strings.Join(call, " ") == "git merge --ff-only origin/main" {
			found = true
		}
	}
	if !found {
		t.Errorf("calls %v do not include the fast-forward", fake.calls)
	}
}

func TestRepoRepairRefusesDirty(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRun{output: map[string]string{
		"git worktree prune --verbose":                                "",
		"git fetch --prune --quiet":                                   "",
		"git status --porcelain=v2 --branch --untracked-files=normal": "# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -2\n? file.go\n",
		"git worktree list --porcelain":                               "",
	}}
	e := repoEngine(t, dir, fake)
	id := "repo-" + slugify(filepath.Base(dir))
	rec := recordsByID(t, e)[id]
	// Force the safe path to prove the repair itself refuses, not just Safe.
	rec.State = contract.Stale
	if _, err := e.Repair(context.Background(), rec); err == nil {
		t.Fatal("Repair fast-forwarded a dirty checkout")
	}
}

func TestRepairRejectsUnknownItem(t *testing.T) {
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Arch)
	if _, err := e.Repair(context.Background(), contract.Record{ID: "nope"}); err == nil {
		t.Fatal("Repair accepted an unknown item")
	}
}

func TestRepairRejectsUnsafeItem(t *testing.T) {
	e := newEngine(t, testConfig(t, t.TempDir()), adapter.Arch)
	if _, err := e.Repair(context.Background(), contract.Record{ID: "disk"}); err == nil {
		t.Fatal("Repair ran an unsafe item")
	}
}

// runGit runs one git command in dir and returns its trimmed stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestRepoFastForwardAgainstRealGit proves the repair against real git rather
// than a scripted status. The fixture is a local clone one commit behind its
// upstream, which is the only state the safe set covers.
func TestRepoFastForwardAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	origin := filepath.Join(base, "origin")
	work := filepath.Join(base, "work")
	identity := []string{"-c", "user.email=lares@test", "-c", "user.name=lares"}

	runGit(t, base, "init", "-q", "-b", "trunk", "--bare", origin)
	seed := filepath.Join(base, "seed")
	runGit(t, base, "clone", "-q", origin, seed)
	runGit(t, seed, append(identity, "commit", "-q", "--allow-empty", "-m", "one")...)
	runGit(t, seed, "push", "-q", "origin", "trunk")
	runGit(t, base, "clone", "-q", "--branch", "trunk", origin, work)
	runGit(t, seed, append(identity, "commit", "-q", "--allow-empty", "-m", "two")...)
	runGit(t, seed, "push", "-q", "origin", "trunk")

	cfg := testConfig(t, base)
	cfg.Repos = []string{work}
	e := newEngine(t, cfg, adapter.Arch)
	id := "repo-" + slugify(filepath.Base(work))

	rec := recordsByID(t, e)[id]
	if rec.State != contract.Stale || !e.Safe(rec) {
		t.Fatalf("state %q safe=%v (%s), want stale and safe", rec.State, e.Safe(rec), rec.Detail)
	}

	if _, err := e.Repair(context.Background(), rec); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if got, want := runGit(t, work, "rev-parse", "HEAD"), runGit(t, origin, "rev-parse", "trunk"); got != want {
		t.Errorf("work HEAD %s, want %s", got, want)
	}
	if rec := recordsByID(t, e)[id]; rec.State != contract.Live {
		t.Errorf("state after repair %q (%s), want live", rec.State, rec.Detail)
	}
	if dirty := runGit(t, work, "status", "--porcelain"); dirty != "" {
		t.Errorf("repair left the worktree dirty:\n%s", dirty)
	}
}
