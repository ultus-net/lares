// Package measure owns lares' checks and its safe repairs.
//
// A check reads host state and returns one contract record. A check in the safe
// set also carries a repair: lares runs it unattended and proves it by
// re-measuring. The checks are Go, so a host needs no checkout and no script of
// lares' own. The only external programs are the host tools a check must read:
// git, systemctl, and the family's update status command.
//
// An error from one check never fails the measurement. It becomes an `unknown`
// record, which the reconciler queues without a repair, because no check proved
// the item healthy.
package measure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/term"
)

// Options are the host reads a check may make. New fills every zero value with
// the real host, so a test injects only what it exercises.
type Options struct {
	Config  config.Config
	Family  adapter.Family
	Adapter adapter.Adapter
	Home    string

	Now         func() time.Time
	Run         func(ctx context.Context, dir string, argv []string) ([]byte, error)
	Statfs      func(path string) (fsStat, error)
	ReadFile    func(path string) ([]byte, error)
	ReadDir     func(path string) ([]os.DirEntry, error)
	LookupShell func() (string, error)
}

// check is one measurement and, when safe, its repair.
type check struct {
	id     string
	term   string
	safe   bool
	verb   string
	run    func(ctx context.Context) (contract.State, string, error)
	repair func(ctx context.Context) (string, error)
}

// Engine measures one host and repairs the items in its safe set.
type Engine struct {
	cfg     config.Config
	family  adapter.Family
	adapter adapter.Adapter
	home    string
	now     func() time.Time
	run     func(ctx context.Context, dir string, argv []string) ([]byte, error)
	statfs  func(path string) (fsStat, error)
	read    func(path string) ([]byte, error)
	readDir func(path string) ([]os.DirEntry, error)
	shell   func() (string, error)

	checks []check
	byID   map[string]*check
}

// New builds the engine over the configured host.
func New(opts Options) (*Engine, error) {
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("measure: %w", err)
	}
	home := opts.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("measure: locate home: %w", err)
		}
	}
	e := &Engine{
		cfg:     cfg,
		family:  opts.Family,
		adapter: opts.Adapter,
		home:    home,
		now:     opts.Now,
		run:     opts.Run,
		statfs:  opts.Statfs,
		read:    opts.ReadFile,
		readDir: opts.ReadDir,
		shell:   opts.LookupShell,
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.run == nil {
		e.run = runCommand
	}
	if e.statfs == nil {
		e.statfs = statfs
	}
	if e.read == nil {
		e.read = os.ReadFile
	}
	if e.readDir == nil {
		e.readDir = os.ReadDir
	}
	if e.shell == nil {
		e.shell = loginShell
	}
	if e.family == "" {
		e.family = adapter.Unknown
	}
	if e.adapter.Family == "" {
		e.adapter = adapter.Defaults()[e.family]
	}

	e.checks = append(e.checks, e.hostChecks()...)
	e.checks = append(e.checks, e.repoChecks()...)
	e.byID = make(map[string]*check, len(e.checks))
	for i := range e.checks {
		e.byID[e.checks[i].id] = &e.checks[i]
	}
	return e, nil
}

// Measure runs every check and appends the summary record.
//
// A check that errors becomes `unknown` with the error as its detail. The
// records keep a stable order: the host checks in declaration order, then the
// repositories by path.
func (e *Engine) Measure(ctx context.Context) ([]contract.Record, error) {
	records := make([]contract.Record, 0, len(e.checks)+1)
	findings := 0
	for _, c := range e.checks {
		state, detail, err := c.run(ctx)
		if err != nil {
			state, detail = contract.Unknown, err.Error()
		}
		if state != contract.Live {
			findings++
		}
		rec := contract.Record{ID: c.id, State: state, Detail: detail}
		if c.safe && state == contract.Stale {
			rec.Fix = c.verb
		}
		records = append(records, rec)
	}

	state := contract.Live
	if findings > 0 {
		state = contract.NotLive
	}
	records = append(records, contract.Record{
		ID:     contract.SummaryID,
		State:  state,
		Detail: fmt.Sprintf("%d checks, %d need attention", len(e.checks), findings),
	})
	return records, nil
}

// Safe reports whether rec may run unattended.
//
// Only a stale repository is safe: it is clean, it is behind its upstream, and
// a fast-forward moves it without changing a local commit. A dirty, detached,
// or ahead repository queues instead.
func (e *Engine) Safe(rec contract.Record) bool {
	c, ok := e.byID[rec.ID]
	return ok && c.safe && rec.State == contract.Stale
}

// Term names the precondition that keeps rec out of the safe set.
func (e *Engine) Term(rec contract.Record) string {
	if c, ok := e.byID[rec.ID]; ok && c.term != "" {
		return c.term
	}
	return term.NeedsOperator
}

// Repair runs the safe repair for rec and returns its log output.
func (e *Engine) Repair(ctx context.Context, rec contract.Record) (string, error) {
	c, ok := e.byID[rec.ID]
	if !ok {
		return "", fmt.Errorf("measure: unknown item %q", rec.ID)
	}
	if !c.safe || c.repair == nil {
		return "", fmt.Errorf("measure: %q is not safe to repair unattended", rec.ID)
	}
	return c.repair(ctx)
}

// hostChecks returns the checks that read the machine itself.
func (e *Engine) hostChecks() []check {
	return []check{
		e.shellCheck(),
		e.patchTrainCheck(),
		e.systemdCheck(),
		e.diskCheck(),
	}
}

// runCommand runs argv in dir and returns stdout. A non-zero exit is an error
// naming the program and the last non-empty stderr line.
func runCommand(ctx context.Context, dir string, argv []string) ([]byte, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("empty command")
	}
	out, err := runOutput(ctx, dir, argv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	return out, nil
}

// repoID turns a repository path into a slug, with a suffix when two checkout
// names collide.
func repoID(path string, used map[string]bool) string {
	base := filepath.Base(path)
	slug := slugify(base)
	if slug == "" {
		slug = "repo"
	}
	id := "repo-" + slug
	for i := 2; used[id]; i++ {
		id = fmt.Sprintf("repo-%s-%d", slug, i)
	}
	used[id] = true
	return id
}

// slugify lowercases and replaces every run of non-alphanumerics with one dash.
func slugify(s string) string {
	var b []rune
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, r)
			lastDash = false
		case r >= 'A' && r <= 'Z':
			b = append(b, r+'a'-'A')
			lastDash = false
		default:
			if len(b) > 0 && !lastDash {
				b = append(b, '-')
				lastDash = true
			}
		}
	}
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	return string(b)
}

// sortedRepos returns the configured checkouts, or the bounded immediate
// children of repos_dir that hold a .git entry.
func (e *Engine) sortedRepos() ([]string, error) {
	if len(e.cfg.Repos) > 0 {
		out := make([]string, 0, len(e.cfg.Repos))
		for _, dir := range e.cfg.Repos {
			if dir == "" {
				continue
			}
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(e.home, dir)
			}
			out = append(out, filepath.Clean(dir))
		}
		sort.Strings(out)
		return out, nil
	}
	if e.cfg.ReposDir == "" {
		return nil, nil
	}
	entries, err := e.readDir(e.cfg.ReposDir)
	if err != nil {
		return nil, fmt.Errorf("read repos_dir %s: %w", e.cfg.ReposDir, err)
	}
	const maxRepos = 200
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if len(out) >= maxRepos {
			break
		}
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(e.cfg.ReposDir, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue
		}
		out = append(out, dir)
	}
	sort.Strings(out)
	return out, nil
}
