package measure

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/contract"
)

// isolateGit drops the ambient variables that would point a nested git call at
// the outer repository. A test must fail on its scratch repo, never on lares'
// own checkout.
func isolateGit(t *testing.T) {
	t.Helper()
	vars := []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
		"GIT_PREFIX", "GIT_CEILING_DIRECTORIES",
	}
	for _, name := range vars {
		value, had := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(name, value)
			}
		})
	}
}

// git runs one git command in dir with a fixed identity.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=lares", "GIT_AUTHOR_EMAIL=lares@example.invalid",
		"GIT_COMMITTER_NAME=lares", "GIT_COMMITTER_EMAIL=lares@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes body and fails the test on any error.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// backupSlug is the checkout slug every scratch test backs up.
const backupSlug = "order-processing"

// backupTarget is the destination branch for host node1.
const backupTarget = "backup/node1/" + backupSlug

// scratch is a work checkout with a bare remote, plus the engine that backs it
// up. The remote default branch is main, so the backup target can never collide
// with it by accident.
type scratch struct {
	remote string
	work   string
	e      *Engine
}

// newScratch builds a bare remote and a clean work checkout with one commit.
func newScratch(t *testing.T) *scratch {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	isolateGit(t)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, backupSlug)
	git(t, root, "init", "--bare", "--initial-branch=main", remote)
	git(t, root, "init", "--initial-branch=main", work)
	writeFile(t, filepath.Join(work, "README.md"), "hello\n")
	git(t, work, "add", "README.md")
	git(t, work, "commit", "-m", "initial")
	git(t, work, "remote", "add", "origin", remote)
	git(t, work, "push", "--set-upstream", "origin", "main")
	cfg := testConfig(t, t.TempDir())
	cfg.Repos = []string{work}
	e := newEngine(t, cfg, adapter.Arch, func(o *Options) {
		o.Host = "node1"
	})
	return &scratch{remote: remote, work: work, e: e}
}

// branch returns a ref's commit in the remote.
func (s *scratch) branch(t *testing.T, name string) string {
	t.Helper()
	return git(t, s.remote, "rev-parse", "--verify", "refs/heads/"+name)
}

// remoteBranches lists the remote branches, one per line.
func (s *scratch) remoteBranches(t *testing.T) []string {
	t.Helper()
	out := git(t, s.remote, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// check runs the backup check and returns its record.
func (s *scratch) check(t *testing.T) contract.Record {
	t.Helper()
	c := s.e.backupCheck("backup-"+backupSlug, backupSlug, s.work)
	state, detail, err := c.run(context.Background())
	if err != nil {
		t.Fatalf("backup check: %v", err)
	}
	return contract.Record{ID: c.id, State: state, Detail: detail, Fix: c.verb}
}

// commit writes one tracked file and commits it.
func (s *scratch) commit(t *testing.T, name string) {
	t.Helper()
	writeFile(t, filepath.Join(s.work, name), name+"\n")
	git(t, s.work, "add", name)
	git(t, s.work, "commit", "-m", "add "+name)
}

func TestBackupIsInTheSafeSet(t *testing.T) {
	s := newScratch(t)
	c := s.e.backupCheck("backup-"+backupSlug, backupSlug, s.work)
	if !s.e.Safe(contract.Record{ID: c.id, State: contract.Stale, Fix: c.verb}) {
		t.Fatal("a stale backup is not safe: the push repair was removed from the safe set")
	}
	if s.e.Safe(contract.Record{ID: c.id, State: contract.NotLive, Fix: c.verb}) {
		t.Error("a not-live backup must never be safe to push")
	}
}

func TestRepoChecksIncludeTheBackupCheck(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "alpha", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, testConfig(t, root), adapter.Arch)
	if _, ok := e.byID["backup-alpha"]; !ok {
		t.Fatal("repository discovery did not add a backup check: outbound backup was removed")
	}
}

func TestBackupRefusesRemoteDefaultBranch(t *testing.T) {
	s := newScratch(t)
	// Make the backup target the remote's default branch. The check must read
	// the symref, not compare against a literal main.
	git(t, s.remote, "symbolic-ref", "HEAD", "refs/heads/"+backupTarget)
	git(t, s.work, "push", "origin", "HEAD:refs/heads/"+backupTarget)

	rec := s.check(t)
	if rec.State != contract.NotLive {
		t.Fatalf("state %q (%s), want not-live", rec.State, rec.Detail)
	}
	if !strings.Contains(rec.Detail, "default branch") {
		t.Errorf("detail %q does not name the default branch", rec.Detail)
	}
	if s.e.Safe(rec) {
		t.Error("a push to the remote default branch must never be safe")
	}
	if _, err := s.e.pushBackup(context.Background(), s.work, backupSlug); err == nil {
		t.Fatal("pushBackup accepted the remote default branch")
	}
}

func TestBackupPushCreatesOnceAndReuses(t *testing.T) {
	s := newScratch(t)
	ctx := context.Background()

	if _, err := s.e.pushBackup(ctx, s.work, backupSlug); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if got := s.branch(t, backupTarget); got == "" {
		t.Fatal("the backup branch was not created")
	}
	first := s.branch(t, backupTarget)
	if branches := s.remoteBranches(t); len(branches) != 2 {
		t.Fatalf("remote branches %v, want main and the backup branch", branches)
	}

	// A second pass with no new commit must reuse the branch and create none.
	if _, err := s.e.pushBackup(ctx, s.work, backupSlug); err != nil {
		t.Fatalf("second push: %v", err)
	}
	if got := s.branch(t, backupTarget); got != first {
		t.Errorf("second pass moved %s to %s, want %s", backupTarget, got, first)
	}
	if branches := s.remoteBranches(t); len(branches) != 2 {
		t.Errorf("second pass created branches: %v", branches)
	}
	upstream := git(t, s.work, "rev-parse", "--abbrev-ref", "--symbolic-full-name", backupTarget+"@{u}")
	if upstream != "origin/"+backupTarget {
		t.Errorf("upstream %q, want origin/%s", upstream, backupTarget)
	}
}

func TestBackupRefusesDivergedRemote(t *testing.T) {
	s := newScratch(t)
	ctx := context.Background()
	if _, err := s.e.pushBackup(ctx, s.work, backupSlug); err != nil {
		t.Fatalf("seed push: %v", err)
	}

	// A second clone adds a commit the local checkout does not have.
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "--branch", backupTarget, s.remote, other)
	writeFile(t, filepath.Join(other, "foreign.txt"), "foreign\n")
	git(t, other, "add", "foreign.txt")
	git(t, other, "commit", "-m", "foreign")
	git(t, other, "push", "origin", "HEAD:refs/heads/"+backupTarget)
	foreign := s.branch(t, backupTarget)

	// The local checkout adds its own commit, so the two histories diverge.
	s.commit(t, "local.txt")

	rec := s.check(t)
	if rec.State != contract.NotLive {
		t.Fatalf("state %q (%s), want not-live", rec.State, rec.Detail)
	}
	if !strings.Contains(rec.Detail, "diverged") {
		t.Errorf("detail %q does not name the divergence", rec.Detail)
	}
	if s.e.Safe(rec) {
		t.Error("a diverged remote must never be safe to push")
	}

	_, err := s.e.pushBackup(ctx, s.work, backupSlug)
	var pe *PushError
	if !errors.As(err, &pe) || pe.Retryable() {
		t.Fatalf("push error %v, want a terminal PushError", err)
	}
	if got := s.branch(t, backupTarget); got != foreign {
		t.Errorf("remote moved to %s, want the foreign commit %s: the push forced", got, foreign)
	}
}

func TestBackupNeverReadsACredentialFile(t *testing.T) {
	s := newScratch(t)
	reads := 0
	s.e.read = func(path string) ([]byte, error) {
		reads++
		return nil, errors.New("credential read attempted: " + path)
	}
	if _, err := s.e.pushBackup(context.Background(), s.work, backupSlug); err != nil {
		t.Fatalf("push: %v", err)
	}
	if reads != 0 {
		t.Errorf("push read %d files, want 0: lares parses no credential", reads)
	}
}

func TestBackupFailedPushIsNeverLive(t *testing.T) {
	s := newScratch(t)
	hook := filepath.Join(s.remote, "hooks", "pre-receive")
	writeFile(t, hook, "#!/bin/sh\necho 'protected branch hook declined' >&2\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := s.e.pushBackup(context.Background(), s.work, backupSlug)
	var pe *PushError
	if !errors.As(err, &pe) {
		t.Fatalf("push error %v, want a classified failure", err)
	}
	if pe.Retryable() {
		t.Errorf("a rejected push is %q, want terminal", pe.Kind)
	}
	if rec := s.check(t); rec.State == contract.Live {
		t.Fatalf("state live after a rejected push: %s", rec.Detail)
	}
}

func TestBackupLiveRequiresBilateralConvergence(t *testing.T) {
	s := newScratch(t)
	ctx := context.Background()
	if _, err := s.e.pushBackup(ctx, s.work, backupSlug); err != nil {
		t.Fatalf("push: %v", err)
	}

	// The remote and HEAD agree, but the local upstream ref must agree too.
	rec := s.check(t)
	if rec.State != contract.Live {
		t.Fatalf("state %q (%s), want live", rec.State, rec.Detail)
	}
	ab := git(t, s.work, "rev-list", "--left-right", "--count", "HEAD..."+backupTarget+"@{u}")
	if fields := strings.Fields(ab); len(fields) != 2 || fields[0] != "0" || fields[1] != "0" {
		t.Fatalf("ahead/behind %q, want 0 0", ab)
	}

	// Drop the local backup branch. The remote still matches HEAD, but without
	// an upstream the item is not proven converged.
	git(t, s.work, "branch", "-D", backupTarget)
	if rec := s.check(t); rec.State == contract.Live {
		t.Fatalf("state live without a local upstream: %s", rec.Detail)
	}
	if _, err := s.e.pushBackup(ctx, s.work, backupSlug); err != nil {
		t.Fatalf("re-create push: %v", err)
	}
	if rec := s.check(t); rec.State != contract.Live {
		t.Fatalf("state %q (%s) after the upstream was restored, want live", rec.State, rec.Detail)
	}
}

func TestBackupCreatesOnAnEmptyRemote(t *testing.T) {
	s := newScratch(t)
	// An empty remote has no refs and no default branch. That must not read as
	// an undetermined default: the first backup has to be able to create it.
	git(t, s.remote, "update-ref", "-d", "refs/heads/main")

	if rec := s.check(t); rec.State != contract.Stale {
		t.Fatalf("state %q (%s), want stale: the first backup must be pushable", rec.State, rec.Detail)
	}
	if _, err := s.e.pushBackup(context.Background(), s.work, backupSlug); err != nil {
		t.Fatalf("first push to an empty remote: %v", err)
	}
	if got := s.branch(t, backupTarget); got == "" {
		t.Fatal("the first push to an empty remote created no branch")
	}
}

func TestBackupPushesNoTagsOrNotes(t *testing.T) {
	s := newScratch(t)
	// Ambient config must not defeat the contract: with follow-tags on, the
	// push has to override it rather than inherit it.
	git(t, s.work, "config", "push.followTags", "true")
	git(t, s.work, "tag", "-a", "v1", "-m", "release")
	git(t, s.work, "notes", "add", "-m", "a note on HEAD")
	if _, err := s.e.pushBackup(context.Background(), s.work, backupSlug); err != nil {
		t.Fatalf("push: %v", err)
	}
	extra := git(t, s.remote, "for-each-ref", "--format=%(refname)", "refs/tags", "refs/notes")
	if extra != "" {
		t.Fatalf("pushed refs outside the backup namespace:\n%s", extra)
	}
}

func TestBackupNeverForcesAndKeepsTheRefspec(t *testing.T) {
	s := newScratch(t)
	var calls [][]string
	inner := s.e.run
	s.e.run = func(ctx context.Context, dir string, argv []string) ([]byte, error) {
		calls = append(calls, append([]string(nil), argv...))
		return inner(ctx, dir, argv)
	}
	if _, err := s.e.pushBackup(context.Background(), s.work, backupSlug); err != nil {
		t.Fatalf("push: %v", err)
	}
	sawPush := false
	for _, argv := range calls {
		sub := false
		for _, arg := range argv {
			if arg == "push" {
				sub = true
				break
			}
		}
		if !sub {
			continue
		}
		line := strings.Join(argv, " ")
		if strings.Contains(line, "--force") || strings.Contains(line, " push -f") {
			t.Fatalf("push argv forces: %s", line)
		}
		if !strings.Contains(line, "-c push.followTags=false") {
			t.Errorf("push argv does not override follow-tags: %s", line)
		}
		if refspec := argv[len(argv)-1]; refspec != backupTarget+":refs/heads/"+backupTarget {
			t.Errorf("refspec %q, want the backup namespace only", refspec)
		}
		sawPush = true
	}
	if !sawPush {
		t.Fatal("no push argv was recorded")
	}
}

func TestBackupUsesGITSSHCommand(t *testing.T) {
	s := newScratch(t)
	// Route the remote through ssh and give git a script that runs the served
	// command locally. The push can only succeed when the child inherits
	// GIT_SSH_COMMAND, which is the credential mechanism the contract names.
	script := filepath.Join(t.TempDir(), "fake-ssh")
	writeFile(t, script, "#!/bin/sh\nshift\nexec sh -c \"$*\"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, s.work, "remote", "set-url", "origin", "ssh://git@example.invalid"+s.remote)
	t.Setenv("GIT_SSH_COMMAND", script)

	if _, err := s.e.pushBackup(context.Background(), s.work, backupSlug); err != nil {
		t.Fatalf("push with GIT_SSH_COMMAND: %v", err)
	}
	if got := s.branch(t, backupTarget); got == "" {
		t.Fatal("the push over ssh created no branch")
	}
}

func TestBackupRefusesToRewindTheLocalBranch(t *testing.T) {
	s := newScratch(t)
	// Put a commit on the local backup branch that HEAD does not have. Moving
	// that branch to HEAD would rewrite it, so the push must refuse.
	git(t, s.work, "checkout", "-b", backupTarget)
	s.commit(t, "local-only.txt")
	git(t, s.work, "checkout", "main")

	_, err := s.e.pushBackup(context.Background(), s.work, backupSlug)
	var pe *PushError
	if !errors.As(err, &pe) || pe.Kind != "precondition" {
		t.Fatalf("push error %v, want a precondition PushError", err)
	}
	for _, name := range s.remoteBranches(t) {
		if name == backupTarget {
			t.Fatalf("the refused push created %s", name)
		}
	}
}

func TestBackupMeasureFailureIsUnknown(t *testing.T) {
	s := newScratch(t)
	inner := s.e.run
	s.e.run = func(ctx context.Context, dir string, argv []string) ([]byte, error) {
		if len(argv) > 1 && argv[1] == "status" {
			return nil, errors.New("status unreadable")
		}
		return inner(ctx, dir, argv)
	}
	c := s.e.backupCheck("backup-"+backupSlug, backupSlug, s.work)
	st, _, err := c.run(context.Background())
	if err == nil {
		t.Fatal("an unreadable checkout reported no error")
	}
	if st != contract.Unknown {
		t.Fatalf("state %q, want unknown for an unreadable checkout", st)
	}
}
