package measure

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/term"
)

// pushTimeout bounds the outbound write. A first backup copies the whole
// history, so it gets a longer bound than a read.
const pushTimeout = 10 * time.Minute

// PushError classifies a failed outbound sync. Retryable reports whether the
// queue may try again without operator action; every other kind is terminal
// until a person acts.
type PushError struct {
	Kind string
	Err  error
}

// Error renders the kind and the underlying git failure.
func (e *PushError) Error() string { return e.Kind + ": " + e.Err.Error() }

// Unwrap exposes the git failure.
func (e *PushError) Unwrap() error { return e.Err }

// Retryable reports whether the failure is a transient network fault.
func (e *PushError) Retryable() bool { return e.Kind == "network" }

// backupState is one checkout's outbound position against its backup branch.
type backupState struct {
	remote string
	target string
	head   string
	tip    string
	exists bool
	ahead  int
	behind int
}

// readBackup runs the preconditions shared by the check and the repair, then
// reads the backup branch after a fetch. One function owns the guards so a
// safety rule cannot exist on one path and be missing from the other.
func (e *Engine) readBackup(ctx context.Context, dir, slug string) (backupState, error) {
	st, err := e.readRepo(ctx, dir)
	if err != nil {
		return backupState{}, err
	}
	if st.dirty > 0 {
		return backupState{}, &PushError{Kind: "precondition", Err: fmt.Errorf("%d uncommitted paths; refusing to push", st.dirty)}
	}
	if st.head == "" || st.head == "(detached)" {
		return backupState{}, &PushError{Kind: "precondition", Err: errors.New("HEAD is detached; refusing to push")}
	}
	remote, err := e.repoRemote(ctx, dir)
	if err != nil {
		return backupState{}, classifyGitError("locate remote", err)
	}
	target := e.backupBranch(slug)
	def, err := e.remoteDefault(ctx, dir, remote)
	if err != nil {
		return backupState{}, classifyGitError("read "+remote+" default branch", err)
	}
	if target == def {
		return backupState{}, &PushError{Kind: "protected", Err: fmt.Errorf("%s is the remote default branch; refusing", target)}
	}
	head, err := e.head(ctx, dir)
	if err != nil {
		return backupState{}, &PushError{Kind: "precondition", Err: errors.New("no commit to back up")}
	}
	bs := backupState{remote: remote, target: target, head: head}
	tip, exists, err := e.lsRemoteBranch(ctx, dir, remote, target)
	if err != nil {
		return backupState{}, classifyGitError("read "+target, err)
	}
	bs.tip, bs.exists = tip, exists
	if !exists {
		return bs, nil
	}
	if err := e.fetchBackup(ctx, dir, remote, target); err != nil {
		return backupState{}, classifyGitError("fetch "+target, err)
	}
	ahead, behind, err := e.aheadBehind(ctx, dir, "HEAD", "refs/remotes/"+remote+"/"+target)
	if err != nil {
		return backupState{}, err
	}
	bs.ahead, bs.behind = ahead, behind
	return bs, nil
}

// backupCheck measures one checkout's outbound backup.
//
// The target is backup/<host>/<repo>. The check is safe only while the local
// branch is ahead of the backup branch or the branch is absent. A diverged or
// behind remote is never safe, because lares never force-pushes.
func (e *Engine) backupCheck(id, slug, dir string) check {
	return check{
		id:   id,
		term: term.NeedsOperator,
		safe: true,
		verb: "push",
		run: func(ctx context.Context) (contract.State, string, error) {
			bs, err := e.readBackup(ctx, dir, slug)
			if err != nil {
				var pe *PushError
				if errors.As(err, &pe) {
					return pushState("backup", err)
				}
				// A checkout that could not be read is unknown, never not-live.
				// Only a classified git failure names a real backup state.
				return contract.Unknown, "", err
			}
			if !bs.exists {
				return contract.Stale, "no backup branch " + bs.target + "; push creates it", nil
			}
			// Convergence is bilateral: the remote tip, the local tracking ref,
			// and HEAD must all agree. A push exit code alone proves nothing.
			if bs.ahead == 0 && bs.behind == 0 && bs.tip == bs.head && e.upstreamMatches(ctx, dir, bs.target, bs.head) {
				return contract.Live, "backup " + bs.target + " matches HEAD", nil
			}
			switch {
			case bs.ahead > 0 && bs.behind > 0:
				return contract.NotLive, fmt.Sprintf("diverged from %s (%d ahead, %d behind); never force", bs.target, bs.ahead, bs.behind), nil
			case bs.behind > 0:
				return contract.NotLive, fmt.Sprintf("backup %s is %d commits ahead; never force", bs.target, bs.behind), nil
			case bs.ahead > 0:
				return contract.Stale, fmt.Sprintf("%d commits ahead of %s; push", bs.ahead, bs.target), nil
			default:
				return contract.Stale, "backup matches HEAD but the local upstream is missing or stale; refresh it", nil
			}
		},
		repair: func(ctx context.Context) (string, error) {
			return e.pushBackup(ctx, dir, slug)
		},
	}
}

// backupBranch names the destination branch for one checkout.
func (e *Engine) backupBranch(slug string) string {
	host := slugify(e.host)
	if host == "" {
		host = "host"
	}
	return "backup/" + host + "/" + slug
}

// pushBackup creates or updates the backup branch and proves the remote moved.
//
// It never forces. A clean checkout on a branch is required, the target must
// differ from the remote default branch, and a behind or diverged remote is
// refused. Success is only reported after ls-remote and the local upstream both
// name the new HEAD.
func (e *Engine) pushBackup(ctx context.Context, dir, slug string) (string, error) {
	var log strings.Builder
	bs, err := e.readBackup(ctx, dir, slug)
	if err != nil {
		return log.String(), err
	}
	if bs.behind > 0 {
		kind := "diverged"
		if bs.ahead == 0 {
			kind = "non-fast-forward"
		}
		return log.String(), &PushError{Kind: kind, Err: fmt.Errorf("%s is %d commits ahead of HEAD; never force", bs.target, bs.behind)}
	}
	if err := e.ensureBackupBranch(ctx, dir, bs.target); err != nil {
		return log.String(), err
	}

	// Ambient push.followTags would send reachable annotated tags, which the
	// contract forbids. Disable it for this push rather than trusting config.
	argv := []string{"git", "-c", "push.followTags=false", "push", "--porcelain", "--set-upstream", bs.remote, bs.target + ":refs/heads/" + bs.target}
	out, err := e.pushRun(ctx, dir, argv)
	log.Write(out)
	if err != nil {
		return log.String(), classifyGitError("push "+bs.target, err)
	}

	after, ok, err := e.lsRemoteBranch(ctx, dir, bs.remote, bs.target)
	if err != nil {
		return log.String(), classifyGitError("verify "+bs.target, err)
	}
	if !ok {
		return log.String(), &PushError{Kind: "verify", Err: errors.New("push reported success but the remote branch is absent")}
	}
	if after != bs.head {
		return log.String(), &PushError{Kind: "verify", Err: fmt.Errorf("remote reports %s, want %s", after, bs.head)}
	}
	if err := e.fetchBackup(ctx, dir, bs.remote, bs.target); err != nil {
		return log.String(), classifyGitError("verify "+bs.target, err)
	}
	if !e.upstreamMatches(ctx, dir, bs.target, bs.head) {
		return log.String(), &PushError{Kind: "verify", Err: fmt.Errorf("%s does not track HEAD after the push", bs.target)}
	}
	return log.String(), nil
}

// repoRemote returns the remote that carries the checkout's upstream, or origin
// when there is no upstream.
func (e *Engine) repoRemote(ctx context.Context, dir string) (string, error) {
	out, err := e.run(ctx, dir, []string{"git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"})
	if err == nil {
		if upstream := strings.TrimSpace(string(out)); upstream != "" {
			if i := strings.Index(upstream, "/"); i > 0 {
				return upstream[:i], nil
			}
		}
	}
	out, err = e.run(ctx, dir, []string{"git", "remote"})
	if err != nil {
		return "", err
	}
	var remotes []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			remotes = append(remotes, name)
		}
	}
	if len(remotes) == 0 {
		return "", errors.New("no git remote")
	}
	for _, name := range remotes {
		if name == "origin" {
			return name, nil
		}
	}
	return remotes[0], nil
}

// remoteDefault resolves the remote's default branch with ls-remote --symref.
// It never compares against a literal branch name. An empty remote has no refs
// and no default branch to collide with, so it returns "" and the first backup
// may create its branch.
func (e *Engine) remoteDefault(ctx context.Context, dir, remote string) (string, error) {
	out, err := e.networkRun(ctx, dir, []string{"git", "ls-remote", "--symref", remote, "HEAD"})
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "ref:" && strings.HasPrefix(fields[1], "refs/heads/") {
			return strings.TrimPrefix(fields[1], "refs/heads/"), nil
		}
	}
	if strings.TrimSpace(string(out)) == "" {
		return "", nil
	}
	return "", errors.New("remote did not report a default branch")
}

// lsRemoteBranch reports the branch's remote SHA. A missing branch is not an
// error; an unreachable remote is.
func (e *Engine) lsRemoteBranch(ctx context.Context, dir, remote, branch string) (string, bool, error) {
	out, err := e.networkRun(ctx, dir, []string{"git", "ls-remote", "--exit-code", remote, "refs/heads/" + branch})
	if err != nil {
		var exit *exitError
		if errors.As(err, &exit) && exit.Code == 2 {
			return "", false, nil
		}
		return "", false, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false, nil
	}
	return fields[0], true, nil
}

// fetchBackup updates the remote-tracking ref for one backup branch.
func (e *Engine) fetchBackup(ctx context.Context, dir, remote, branch string) error {
	ref := "refs/heads/" + branch
	_, err := e.networkRun(ctx, dir, []string{"git", "fetch", "--quiet", remote, "+" + ref + ":refs/remotes/" + remote + "/" + branch})
	return err
}

// head returns the commit at HEAD.
func (e *Engine) head(ctx context.Context, dir string) (string, error) {
	out, err := e.run(ctx, dir, []string{"git", "rev-parse", "--verify", "HEAD"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// aheadBehind counts the commits on each side of left...right.
func (e *Engine) aheadBehind(ctx context.Context, dir, left, right string) (int, int, error) {
	out, err := e.run(ctx, dir, []string{"git", "rev-list", "--left-right", "--count", left + "..." + right})
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", string(out))
	}
	ahead, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, err
	}
	behind, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, err
	}
	return ahead, behind, nil
}

// upstreamMatches reports whether the local backup branch tracks head.
func (e *Engine) upstreamMatches(ctx context.Context, dir, branch, head string) bool {
	out, err := e.run(ctx, dir, []string{"git", "rev-parse", "--verify", "--quiet", branch + "@{u}"})
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == head
}

// ensureBackupBranch points a local branch at HEAD without touching the
// checkout. A branch that is already checked out is left alone. An existing
// branch moves only forward; refusing a backward move keeps the
// never-rewrite-history rule true on the local side too.
func (e *Engine) ensureBackupBranch(ctx context.Context, dir, branch string) error {
	out, err := e.run(ctx, dir, []string{"git", "symbolic-ref", "--quiet", "--short", "HEAD"})
	if err == nil && strings.TrimSpace(string(out)) == branch {
		return nil
	}
	local := "refs/heads/" + branch
	if _, err := e.run(ctx, dir, []string{"git", "rev-parse", "--verify", "--quiet", local}); err == nil {
		if _, err := e.run(ctx, dir, []string{"git", "merge-base", "--is-ancestor", local, "HEAD"}); err != nil {
			return &PushError{Kind: "precondition", Err: fmt.Errorf("local %s holds commits not in HEAD; refusing to rewrite it", branch)}
		}
		if _, err := e.run(ctx, dir, []string{"git", "branch", "-f", branch, "HEAD"}); err != nil {
			return classifyGitError("update local backup branch", err)
		}
		return nil
	}
	if _, err := e.run(ctx, dir, []string{"git", "branch", branch, "HEAD"}); err != nil {
		return classifyGitError("create local backup branch", err)
	}
	return nil
}

// networkRun bounds a git call that reaches the network.
func (e *Engine) networkRun(ctx context.Context, dir string, argv []string) ([]byte, error) {
	return e.runBounded(ctx, dir, argv, fetchTimeout)
}

// pushRun bounds the outbound write, which may copy the whole history.
func (e *Engine) pushRun(ctx context.Context, dir string, argv []string) ([]byte, error) {
	return e.runBounded(ctx, dir, argv, pushTimeout)
}

// runBounded runs argv under a timeout that the parent context can shorten.
func (e *Engine) runBounded(ctx context.Context, dir string, argv []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return e.run(ctx, dir, argv)
}

// pushState maps a classified git failure to a record state. A network fault is
// stale so the next pass may retry with backoff. A terminal failure is not-live:
// it needs a person. An unclassified failure is unknown, because lares cannot
// prove a backup state from it.
func pushState(stage string, err error) (contract.State, string, error) {
	pe := classifyGitError(stage, err)
	switch {
	case pe.Retryable():
		return contract.Stale, pe.Error(), nil
	case pe.Kind == "other":
		return contract.Unknown, pe.Error(), nil
	default:
		return contract.NotLive, pe.Error(), nil
	}
}

// classifyGitError names the failure so the queue can retry a network fault and
// stop on the rest.
func classifyGitError(stage string, err error) *PushError {
	var pe *PushError
	if errors.As(err, &pe) {
		return pe
	}
	text := strings.ToLower(err.Error())
	var exit *exitError
	if errors.As(err, &exit) {
		text += "\n" + strings.ToLower(exit.Stderr)
	}
	switch {
	case strings.Contains(text, "protected branch"),
		strings.Contains(text, "pre-receive hook declined"),
		strings.Contains(text, "gh006"),
		strings.Contains(text, "protected branch hook declined"):
		return &PushError{Kind: "protected", Err: fmt.Errorf("%s: %w", stage, err)}
	case strings.Contains(text, "authentication failed"),
		strings.Contains(text, "permission denied"),
		strings.Contains(text, "publickey"),
		strings.Contains(text, "could not read username"),
		strings.Contains(text, "could not read password"),
		strings.Contains(text, "access denied"),
		strings.Contains(text, "403"):
		return &PushError{Kind: "auth", Err: fmt.Errorf("%s: %w", stage, err)}
	case strings.Contains(text, "non-fast-forward"),
		strings.Contains(text, "fetch first"),
		strings.Contains(text, "rejected"):
		return &PushError{Kind: "diverged", Err: fmt.Errorf("%s: %w", stage, err)}
	case strings.Contains(text, "could not resolve host"),
		strings.Contains(text, "connection refused"),
		strings.Contains(text, "connection timed out"),
		strings.Contains(text, "operation timed out"),
		strings.Contains(text, "i/o timeout"),
		strings.Contains(text, "context deadline exceeded"),
		strings.Contains(text, "network is unreachable"),
		strings.Contains(text, "unable to access"),
		strings.Contains(text, "temporary failure in name resolution"):
		return &PushError{Kind: "network", Err: fmt.Errorf("%s: %w", stage, err)}
	default:
		return &PushError{Kind: "other", Err: fmt.Errorf("%s: %w", stage, err)}
	}
}
