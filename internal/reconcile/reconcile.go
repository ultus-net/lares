// Package reconcile runs one measurement, converges the repairs that are safe
// to repeat, verifies that the state moved, and queues the rest.
//
// It owns the policy, not the probes or the verbs. A probe is a command in the
// dotfiles repository and a repair verb is the `fix` field the probe prints, so
// a new check arrives as data.
//
// Every side effect is injectable: measure, run a command, sleep, read the
// session state, ask the update-train lock, list agent processes, and activate a
// reboot. Tests drive the whole loop without touching the host.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
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

// Agent is one long-horizon process. StartTicks pins the process instance, so a
// hold claims one process and never every process with the same name.
type Agent struct {
	PID        int
	Name       string
	StartTicks uint64
}

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
	Agents   func() ([]Agent, error)
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
	agents   func() ([]Agent, error)
	activate func(ctx context.Context, argv []string) error
}

// Result reports what one pass did.
type Result struct {
	Measured  []contract.Record
	Converged []string
	Queued    []string
	Escalated []string
	Skipped   []string
	Reboot    RebootVerdict
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
// rest, records a verdict per record, and evaluates the reboot gate.
//
// The whole pass runs under the state lock, so a second lares process cannot
// repair the same item or overwrite the queue this pass derived.
func (e *Engine) Pass(ctx context.Context) (Result, error) {
	if err := e.store.Lock(); err != nil {
		return Result{}, err
	}
	defer e.store.Unlock()

	now := e.now()
	records, err := e.measure(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("measure: %w", err)
	}
	result := Result{Measured: records}
	if err := e.record(records, now); err != nil {
		return result, err
	}
	if err := e.store.Prune(now); err != nil {
		return result, err
	}

	previous, _, err := e.store.Queue()
	if err != nil && !errors.Is(err, state.ErrNoQueue) {
		return result, err
	}
	previousByID := make(map[string]state.Item, len(previous.Items))
	for _, item := range previous.Items {
		previousByID[item.ID] = item
	}
	deferrals, err := e.store.Deferrals()
	if err != nil {
		return result, err
	}
	holds, err := e.store.Holds()
	if err != nil {
		return result, err
	}
	// A failed process scan cannot prove that no agent is running, so it holds
	// the work just as a live hold does, under its own term.
	unclaimed, scanErr := e.unclaimedAgents(holds)
	heldTerm := termHold
	if scanErr != nil {
		heldTerm = termScan
	}
	held := len(holds) > 0 || scanErr != nil || len(unclaimed) > 0

	next := state.Queue{Generation: previous.Generation}
	// A queued reboot survives the pass; the gate below rewrites its term or
	// clears it once it runs.
	if item, ok := previousByID[RebootID]; ok {
		next.Items = append(next.Items, item)
	}

	for _, rec := range records {
		if rec.IsSummary() || !rec.NeedsAttention() {
			continue
		}
		item, seen := previousByID[rec.ID]
		if !seen {
			item = state.Item{ID: rec.ID, FirstSeen: now}
		}

		if deferral, ok := deferrals[rec.ID]; ok && deferral.Until.After(now) {
			item.Term = termDeferred
			item.NextAttemptAt = deferral.Until
			next.Items = append(next.Items, item)
			result.Skipped = append(result.Skipped, rec.ID)
			continue
		}
		if rec.State == contract.Unknown {
			item.Term = termUnverified
			next.Items = append(next.Items, item)
			result.Queued = append(result.Queued, rec.ID)
			continue
		}
		if !safeToConverge(rec.ID) || rec.Fix == "" {
			item.Term = repairTerm(rec.ID)
			next.Items = append(next.Items, item)
			result.Queued = append(result.Queued, rec.ID)
			continue
		}
		if held {
			item.Term = heldTerm
			next.Items = append(next.Items, item)
			result.Skipped = append(result.Skipped, rec.ID)
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
		// Keep the attempt even if the pass hits its deadline later.
		if err := e.persistItem(item); err != nil {
			return result, err
		}
	}

	if _, err := e.store.SaveQueue(next); err != nil {
		return result, err
	}
	if e.cfg.RebootFromPass {
		verdict, err := e.rebootLocked(ctx)
		if err != nil {
			return result, err
		}
		result.Reboot = verdict
	}
	return result, nil
}

// converge runs the record's repair verb, then re-measures on the verify ladder.
//
// It returns true only when a verification measurement names the item and says
// live. A missing record and an unknown record both leave the repair unproven,
// so the item stays queued.
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
		if err := e.record(records, e.now()); err != nil {
			return false, logPath, err
		}
		if st, ok := stateOf(records, rec.ID); ok && st == contract.Live {
			return true, logPath, nil
		}
	}
	return false, logPath, nil
}

// record appends one verdict per non-summary record.
func (e *Engine) record(records []contract.Record, at time.Time) error {
	for _, rec := range records {
		if rec.IsSummary() {
			continue
		}
		verdict := state.Verdict{ID: rec.ID, State: string(rec.State), Detail: rec.Detail, Fix: rec.Fix, At: at}
		if err := e.store.AppendVerdict(verdict); err != nil {
			return err
		}
	}
	return nil
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

// stateOf returns id's state and whether records named it.
func stateOf(records []contract.Record, id string) (contract.State, bool) {
	for _, rec := range records {
		if rec.ID == id && !rec.IsSummary() {
			return rec.State, true
		}
	}
	return "", false
}

// persistItem writes one item immediately, so a pass that hits its deadline
// still keeps the attempts and backoff it earned.
func (e *Engine) persistItem(item state.Item) error {
	queue, _, err := e.store.Queue()
	if err != nil && !errors.Is(err, state.ErrNoQueue) {
		return err
	}
	replaced := false
	for i, existing := range queue.Items {
		if existing.ID == item.ID {
			queue.Items[i] = item
			replaced = true
			break
		}
	}
	if !replaced {
		queue.Items = append(queue.Items, item)
	}
	_, err = e.store.SaveQueue(queue)
	return err
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

// Defer records an operator deferral and moves the item's next attempt out.
//
// The deferral is appended to the verdict log before the queue changes, so a
// lost queue is rebuilt with the operator's judgement intact.
func (e *Engine) Defer(id string, d time.Duration, reason string) (time.Time, error) {
	if !state.ValidSlug(id) {
		return time.Time{}, fmt.Errorf("defer: %q is not an item id", id)
	}
	if d < time.Minute {
		return time.Time{}, fmt.Errorf("defer: %v is shorter than a minute", d)
	}
	if err := e.store.Lock(); err != nil {
		return time.Time{}, err
	}
	defer e.store.Unlock()

	now := e.now()
	until := now.Add(d)
	if err := e.store.AppendVerdict(state.Verdict{
		ID: id, Kind: state.KindDefer, Detail: reason, At: now, Until: until,
	}); err != nil {
		return time.Time{}, err
	}

	queue, _, err := e.store.Queue()
	if err != nil && !errors.Is(err, state.ErrNoQueue) {
		return time.Time{}, err
	}
	found := false
	for i := range queue.Items {
		if queue.Items[i].ID != id {
			continue
		}
		queue.Items[i].Term = termDeferred
		queue.Items[i].NextAttemptAt = until
		found = true
		break
	}
	if !found {
		queue.Items = append(queue.Items, state.Item{
			ID:            id,
			Term:          termDeferred,
			FirstSeen:     now,
			NextAttemptAt: until,
		})
	}
	if _, err := e.store.SaveQueue(queue); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

// Status is the queue, the live holds, and the newest verdict per item.
type Status struct {
	Queue    state.Queue
	Holds    []state.Hold
	Verdicts map[string]state.Verdict
}

// Status reads the current state for the CLI and the TUI. A deferral is not a
// measurement, so it never replaces the newest verdict.
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
		if v.IsDeferral() {
			continue
		}
		newest[v.ID] = v
	}
	return Status{Queue: queue, Holds: holds, Verdicts: newest}, nil
}
