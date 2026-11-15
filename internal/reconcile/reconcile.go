// Package reconcile runs one measurement, converges the repairs that are safe
// to repeat, verifies that the state moved, and queues the rest.
//
// It owns the policy, not the probes or the verbs. A probe is a command in the
// dotfiles repository and a repair verb is the `fix` field the probe prints, so
// a new check arrives as data.
//
// Every side effect is injectable: measure, run a command, sleep, read the
// session state, ask the update-train lock, and activate a reboot. Tests drive
// the whole loop without touching the host.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/state"
)

// VerifyDelays is the ladder that decides whether a repair held.
var VerifyDelays = []time.Duration{5 * time.Second, 30 * time.Second, 5 * time.Minute}

// maxBackoff caps the queue delay.
const maxBackoff = 24 * time.Hour

// termNeedsOperator marks a finding the safe set does not cover.
const termNeedsOperator = "needs-operator"

// termDidNotHold marks a repair whose verification saw no movement.
const termDidNotHold = "fix-did-not-hold"

// termBackoff marks a due item that is waiting out its retry delay.
const termBackoff = "backoff"

// Options are the dependencies of an Engine. New fills the zero values with the
// host defaults.
type Options struct {
	Config   config.Config
	Store    *state.Store
	Family   adapter.Family
	Adapters map[adapter.Family]adapter.Adapter

	// RepoDir is the dotfiles checkout that holds the probe and the repair
	// verbs. Relative audit paths resolve against it.
	RepoDir string

	Now      func() time.Time
	Measure  func(ctx context.Context) ([]contract.Record, error)
	Run      func(ctx context.Context, dir string, argv []string) (string, error)
	Sleep    func(ctx context.Context, d time.Duration) error
	Session  func(ctx context.Context) (bool, error)
	Train    func(ctx context.Context) (bool, error)
	Agents   func() []string
	Activate func(ctx context.Context, argv []string) error
}

// Engine reconciles one host.
type Engine struct {
	cfg      config.Config
	store    *state.Store
	family   adapter.Family
	adapters map[adapter.Family]adapter.Adapter
	repoDir  string

	now      func() time.Time
	measure  func(ctx context.Context) ([]contract.Record, error)
	run      func(ctx context.Context, dir string, argv []string) (string, error)
	sleep    func(ctx context.Context, d time.Duration) error
	session  func(ctx context.Context) (bool, error)
	train    func(ctx context.Context) (bool, error)
	agents   func() []string
	activate func(ctx context.Context, argv []string) error

	busy atomic.Bool
}

// Result reports what one pass did.
type Result struct {
	Measured  []contract.Record
	Converged []string
	Queued    []string
	Escalated []string
	Skipped   []string
}

// New builds an Engine, replacing the zero fields in opts with host defaults.
func New(opts Options) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("reconcile: store is required")
	}
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	e := &Engine{
		cfg:      cfg,
		store:    opts.Store,
		family:   opts.Family,
		adapters: opts.Adapters,
		repoDir:  opts.RepoDir,
		now:      opts.Now,
		measure:  opts.Measure,
		run:      opts.Run,
		sleep:    opts.Sleep,
		session:  opts.Session,
		train:    opts.Train,
		agents:   opts.Agents,
		activate: opts.Activate,
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.repoDir == "" {
		e.repoDir = "."
	}
	if e.measure == nil {
		e.measure = func(ctx context.Context) ([]contract.Record, error) {
			return contract.MeasureIn(ctx, e.repoDir, resolveProbe(cfg.AuditCommand, e.repoDir))
		}
	}
	if e.run == nil {
		e.run = runCommand
	}
	if e.sleep == nil {
		e.sleep = sleepContext
	}
	if e.session == nil {
		e.session = defaultSessionActive
	}
	if e.train == nil {
		e.train = func(ctx context.Context) (bool, error) { return defaultTrainRunning(ctx, e.family) }
	}
	if e.agents == nil {
		e.agents = defaultAgents
	}
	if e.activate == nil {
		e.activate = func(ctx context.Context, argv []string) error {
			return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
		}
	}
	return e, nil
}

// resolveProbe makes a relative script path absolute against repoDir.
func resolveProbe(argv []string, repoDir string) []string {
	if len(argv) < 2 || filepath.IsAbs(argv[1]) {
		return argv
	}
	out := append([]string(nil), argv...)
	out[1] = filepath.Join(repoDir, argv[1])
	return out
}

// runCommand runs argv in dir and returns combined output.
func runCommand(ctx context.Context, dir string, argv []string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("reconcile: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// sleepContext waits d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Pass measures once, converges what is due and safe, verifies, queues the
// rest, and records a verdict per record.
func (e *Engine) Pass(ctx context.Context) (Result, error) {
	if !e.busy.CompareAndSwap(false, true) {
		return Result{}, errors.New("reconcile: a pass is already running")
	}
	defer e.busy.Store(false)

	now := e.now()
	records, err := e.measure(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("measure: %w", err)
	}
	result := Result{Measured: records}
	for _, rec := range records {
		if rec.IsSummary() {
			continue
		}
		verdict := state.Verdict{ID: rec.ID, State: string(rec.State), Detail: rec.Detail, Fix: rec.Fix, At: now}
		if err := e.store.AppendVerdict(verdict); err != nil {
			return result, err
		}
	}

	previous, _, err := e.store.Queue()
	if err != nil && !errors.Is(err, state.ErrNoQueue) {
		return result, err
	}
	previousByID := make(map[string]state.Item, len(previous.Items))
	for _, item := range previous.Items {
		previousByID[item.ID] = item
	}

	next := state.Queue{Generation: previous.Generation}
	for _, rec := range records {
		if rec.IsSummary() || !rec.IsFinding() {
			continue
		}
		item, seen := previousByID[rec.ID]
		if !seen {
			item = state.Item{ID: rec.ID, FirstSeen: now}
		}
		item.Term = termNeedsOperator

		if !safeToConverge(rec.ID) || rec.Fix == "" {
			next.Items = append(next.Items, item)
			result.Queued = append(result.Queued, rec.ID)
			continue
		}
		if item.NextAttemptAt.After(now) {
			item.Term = termBackoff
			next.Items = append(next.Items, item)
			result.Skipped = append(result.Skipped, rec.ID)
			continue
		}

		converged, logPath, err := e.converge(ctx, rec)
		if err != nil {
			return result, err
		}
		if converged {
			result.Converged = append(result.Converged, rec.ID)
			continue
		}
		item.Term = termDidNotHold
		item.Attempts++
		item.NextAttemptAt = now.Add(backoff(e.cfg.Resync, item.Attempts))
		item.LastRunLog = logPath
		next.Items = append(next.Items, item)
		result.Escalated = append(result.Escalated, rec.ID)
	}

	if _, err := e.store.SaveQueue(next); err != nil {
		return result, err
	}
	return result, nil
}

// converge runs the record's repair verb, then re-measures on the verify ladder.
// It returns whether the item stopped being a finding, plus the run log path.
func (e *Engine) converge(ctx context.Context, rec contract.Record) (bool, string, error) {
	argv := strings.Fields(rec.Fix)
	output, runErr := e.run(ctx, e.repoDir, argv)

	logPath, logErr := e.store.RunLog(rec.ID, e.now(), []byte(formatRun(rec, argv, output, runErr)))
	if logErr != nil {
		return false, "", logErr
	}
	if runErr != nil {
		return false, logPath, nil
	}

	for _, delay := range VerifyDelays {
		if err := e.sleep(ctx, delay); err != nil {
			return false, logPath, err
		}
		records, err := e.measure(ctx)
		if err != nil {
			return false, logPath, fmt.Errorf("verify %s: %w", rec.ID, err)
		}
		if !stillFinding(records, rec.ID) {
			return true, logPath, nil
		}
	}
	return false, logPath, nil
}

// formatRun renders one converge run for the item's log.
func formatRun(rec contract.Record, argv []string, output string, runErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "item: %s\n", rec.ID)
	fmt.Fprintf(&b, "state: %s\n", rec.State)
	fmt.Fprintf(&b, "detail: %s\n", rec.Detail)
	fmt.Fprintf(&b, "command: %s\n", strings.Join(argv, " "))
	if runErr != nil {
		fmt.Fprintf(&b, "error: %v\n", runErr)
	}
	b.WriteString("output:\n")
	b.WriteString(output)
	if !strings.HasSuffix(output, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// stillFinding reports whether id is still a finding in records.
func stillFinding(records []contract.Record, id string) bool {
	for _, rec := range records {
		if rec.ID == id && !rec.IsSummary() {
			return rec.IsFinding()
		}
	}
	return false
}

// backoff doubles the resync interval per attempt and caps at one day.
func backoff(resync time.Duration, attempts int) time.Duration {
	delay := resync
	for i := 1; i < attempts; i++ {
		if delay >= maxBackoff {
			return maxBackoff
		}
		delay *= 2
	}
	if delay > maxBackoff {
		return maxBackoff
	}
	return delay
}

// Status is the queue, the live holds, and the newest verdict per item.
type Status struct {
	Queue    state.Queue
	Holds    []state.Hold
	Verdicts map[string]state.Verdict
}

// Status reads the current state for the CLI and the TUI.
func (e *Engine) Status() (Status, error) {
	queue, _, err := e.store.Queue()
	if err != nil && !errors.Is(err, state.ErrNoQueue) {
		return Status{}, err
	}
	holds, err := e.store.Holds()
	if err != nil {
		return Status{}, err
	}
	verdicts, err := e.store.Verdicts()
	if err != nil {
		return Status{}, err
	}
	newest := make(map[string]state.Verdict, len(verdicts))
	for _, v := range verdicts {
		newest[v.ID] = v
	}
	return Status{Queue: queue, Holds: holds, Verdicts: newest}, nil
}
