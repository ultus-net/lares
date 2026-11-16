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

// fetchTimeout bounds the network read that makes a behind count meaningful.
const fetchTimeout = 60 * time.Second

// repoStatus is the state of one checkout, read from git.
type repoStatus struct {
	dirty    int
	head     string
	upstream string
	ahead    int
	behind   int
	prunable int
}

// repoChecks returns one check per configured or discovered checkout.
//
// When discovery cannot read repos_dir, it returns one unknown check instead of
// an empty set. An unreadable directory must never look like a host with no
// repositories to check.
func (e *Engine) repoChecks() []check {
	dirs, err := e.sortedRepos()
	if err != nil {
		return []check{{
			id:   "repo-discovery",
			term: term.NeedsOperator,
			run: func(context.Context) (contract.State, string, error) {
				return contract.Unknown, "", err
			},
		}}
	}
	used := make(map[string]bool, len(dirs))
	checks := make([]check, 0, len(dirs)*2)
	for _, dir := range dirs {
		slug := repoSlug(dir, used)
		checks = append(checks, e.repoCheck("repo-"+slug, dir))
		checks = append(checks, e.backupCheck("backup-"+slug, slug, dir))
	}
	return checks
}

// repoCheck measures one checkout. A clean checkout that is behind its upstream
// is the only stale state, and a fast-forward is its safe repair.
func (e *Engine) repoCheck(id, dir string) check {
	return check{
		id:   id,
		term: term.NeedsOperator,
		safe: true,
		verb: "fast-forward",
		run: func(ctx context.Context) (contract.State, string, error) {
			// A failed fetch leaves the last known remote refs in place, so the
			// behind count is at worst stale, never wrong in the unsafe
			// direction.
			fetchErr := e.fetchRepo(ctx, dir)
			st, err := e.readRepo(ctx, dir)
			if err != nil {
				return contract.Unknown, "", err
			}
			state, detail := classify(st)
			if fetchErr != nil {
				detail += " (fetch failed: " + fetchErr.Error() + ")"
			}
			return state, detail, nil
		},
		repair: func(ctx context.Context) (string, error) {
			return e.repoFastForward(ctx, dir)
		},
	}
}

// fetchRepo reads the upstream with a bounded timeout.
func (e *Engine) fetchRepo(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	_, err := e.run(ctx, dir, []string{"git", "fetch", "--prune", "--quiet"})
	return err
}

// readRepo reads porcelain status and the worktree list.
func (e *Engine) readRepo(ctx context.Context, dir string) (repoStatus, error) {
	out, err := e.run(ctx, dir, []string{"git", "status", "--porcelain=v2", "--branch", "--untracked-files=normal"})
	if err != nil {
		return repoStatus{}, fmt.Errorf("read %s: %w", dir, err)
	}
	st := parseRepoStatus(string(out))

	worktrees, err := e.run(ctx, dir, []string{"git", "worktree", "list", "--porcelain"})
	if err == nil {
		st.prunable = countPrunable(string(worktrees))
	}
	return st, nil
}

// parseRepoStatus reads the fields of `git status --porcelain=v2 --branch`.
func parseRepoStatus(out string) repoStatus {
	var st repoStatus
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "# ") {
			st.dirty++
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		switch fields[1] {
		case "branch.head":
			st.head = fields[2]
		case "branch.upstream":
			st.upstream = fields[2]
		case "branch.ab":
			if len(fields) < 4 {
				continue
			}
			st.ahead, _ = strconv.Atoi(strings.TrimPrefix(fields[2], "+"))
			st.behind, _ = strconv.Atoi(strings.TrimPrefix(fields[3], "-"))
		}
	}
	return st
}

// countPrunable counts worktrees whose directory is gone.
func countPrunable(out string) int {
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "prunable" || strings.HasPrefix(line, "prunable ") {
			count++
		}
	}
	return count
}

// classify turns a checkout's status into a record.
//
// Order matters: a dirty checkout that is also behind is not safe, so it
// reports the blocker that needs a person.
func classify(st repoStatus) (contract.State, string) {
	switch {
	case st.dirty > 0:
		return contract.NotLive, fmt.Sprintf("%d uncommitted paths", st.dirty)
	case st.head == "" || st.head == "(detached)":
		return contract.NotLive, "detached HEAD"
	case st.upstream == "":
		return contract.NotLive, "no upstream branch"
	case st.ahead > 0:
		return contract.NotLive, fmt.Sprintf("%d unpushed commits", st.ahead)
	case st.behind > 0 && st.prunable > 0:
		return contract.Stale, fmt.Sprintf("%d commits behind %s, %d prunable worktrees", st.behind, st.upstream, st.prunable)
	case st.behind > 0:
		return contract.Stale, fmt.Sprintf("%d commits behind %s", st.behind, st.upstream)
	case st.prunable > 0:
		return contract.Stale, fmt.Sprintf("%d prunable worktrees", st.prunable)
	default:
		return contract.Live, "clean, up to date with " + st.upstream
	}
}

// repoFastForward prunes dead worktrees and moves a clean checkout to its
// upstream. Both steps are idempotent.
//
// The safety checks run before the worktree prune and the merge, so a refused
// repair changes no worktree bookkeeping and moves no branch. A prune failure is
// recorded in the log and does not block the fast-forward that can still run.
func (e *Engine) repoFastForward(ctx context.Context, dir string) (string, error) {
	var log strings.Builder
	if err := e.fetchRepo(ctx, dir); err != nil {
		return log.String(), fmt.Errorf("fetch %s: %w", dir, err)
	}
	st, err := e.readRepo(ctx, dir)
	if err != nil {
		return log.String(), err
	}
	switch {
	case st.dirty > 0:
		return log.String(), errors.New("worktree is dirty; refusing to fast-forward")
	case st.ahead > 0:
		return log.String(), errors.New("local commits are ahead; refusing to fast-forward")
	case st.upstream == "":
		return log.String(), errors.New("no upstream branch; refusing to fast-forward")
	}
	if st.prunable > 0 {
		// git keeps its default prune expiry, so a worktree that is only
		// temporarily missing is not removed here.
		out, err := e.run(ctx, dir, []string{"git", "worktree", "prune", "--verbose"})
		log.Write(out)
		if err != nil {
			log.WriteString("worktree prune failed: " + err.Error() + "\n")
		}
	}
	if st.behind == 0 {
		return log.String(), nil
	}
	out, err := e.run(ctx, dir, []string{"git", "merge", "--ff-only", st.upstream})
	log.Write(out)
	if err != nil {
		return log.String(), fmt.Errorf("fast-forward %s: %w", dir, err)
	}
	return log.String(), nil
}
