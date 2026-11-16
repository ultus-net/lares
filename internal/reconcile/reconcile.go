// Package reconcile runs one measurement, converges the repairs that are safe
// to repeat, verifies that the state moved, and queues the rest.
//
// It owns the policy, not the checks or the verbs. The measure engine supplies
// both: a check produces a record, and a check in the safe set carries an
// in-process repair. A record from an external probe is always queued, because
// lares does not run a repair it cannot prove.
//
// Every side effect is injectable: measure, repair, sleep, read the session
// state, ask the update-train lock, list agent processes, and activate a reboot.
// Tests drive the whole loop without touching the host.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/measure"
	"github.com/ultus-net/lares/internal/state"
	"github.com/ultus-net/lares/internal/term"
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

// Repairer decides which records are safe and runs their repairs.
//
// The measure engine implements it. A test injects a fake, and an external probe
// injects none, which queues every record.
type Repairer interface {
	Safe(contract.Record) bool
	Term(contract.Record) string
	Repair(ctx context.Context, rec contract.Record) (string, error)
}

// Options are the dependencies of an Engine. New fills the zero values with the
// host defaults.
type Options struct {
	Config   config.Config
	Store    *state.Store
	Family   adapter.Family
	Adapters map[adapter.Family]adapter.Adapter

	Now      func() time.Time
	Measure  func(ctx context.Context) ([]contract.Record, error)
	Repairer Repairer
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

	now      func() time.Time
	measure  func(ctx context.Context) ([]contract.Record, error)
	repairer Repairer
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
		now:      opts.Now,
		measure:  opts.Measure,
		repairer: opts.Repairer,
		sleep:    opts.Sleep,
		session:  opts.Session,
		train:    opts.Train,
		agents:   opts.Agents,
		activate: opts.Activate,
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.repairer == nil {
		if len(cfg.AuditCommand) > 0 {
			// An external probe proves nothing lares can repair, so every
			// record it prints queues.
			e.repairer = noRepairer{}
		} else {
			engine, err := measure.New(measure.Options{
				Config:  cfg,
				Family:  opts.Family,
				Adapter: opts.Adapters[opts.Family],
				Now:     e.now,
			})
			if err != nil {
				return nil, err
			}
			e.repairer = engine
			if e.measure == nil {
				e.measure = engine.Measure
			}
		}
	}
	if e.measure == nil {
		if len(cfg.AuditCommand) == 0 {
			return nil, errors.New("reconcile: measure is required when no audit_command is configured")
		}
		argv := append([]string(nil), cfg.AuditCommand...)
		e.measure = func(ctx context.Context) ([]contract.Record, error) {
			return contract.Measure(ctx, argv)
		}
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

// noRepairer queues every record. It is the policy for an external probe.
type noRepairer struct{}

// Safe always reports false.
func (noRepairer) Safe(contract.Record) bool { return false }

// Term names the operator, because lares cannot prove an external repair.
func (noRepairer) Term(contract.Record) string { return term.NeedsOperator }

// Repair refuses, because a check lares does not own is never run unattended.
func (noRepairer) Repair(context.Context, contract.Record) (string, error) {
	return "", errors.New("reconcile: an external record is never repaired unattended")
}

// Measure runs one measurement without taking the lock or touching the queue.
//
// The CLI uses it for `lares measure`, which records verdicts but derives no
// queue. A pass calls the same function under the lock.
func (e *Engine) Measure(ctx context.Context) ([]contract.Record, error) {
	return e.measure(ctx)
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
	heldTerm := term.Hold
	if scanErr != nil {
		heldTerm = term.Scan
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
			item.Term = term.Deferred
			item.NextAttemptAt = deferral.Until
			next.Items = append(next.Items, item)
			result.Skipped = append(result.Skipped, rec.ID)
			continue
		}
		if item.Blocked != "" {
			// A terminal push failure is not retried blind. The item leaves the
			// queue when a measurement proves it live, or the operator clears
			// the block with a deferral.
			item.Term = term.NeedsOperator
			next.Items = append(next.Items, item)
			result.Queued = append(result.Queued, rec.ID)
			continue
		}
		if rec.State == contract.Unknown {
			item.Term = term.Unverified
			next.Items = append(next.Items, item)
			result.Queued = append(result.Queued, rec.ID)
			continue
		}
		if !e.repairer.Safe(rec) {
			item.Term = e.repairer.Term(rec)
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
			item.Term = term.Backoff
			if isBackupItem(rec.ID) && item.Attempts >= EscalateAfterAttempts {
				item.Term = term.Escalated
			}
			next.Items = append(next.Items, item)
			result.Skipped = append(result.Skipped, rec.ID)
			continue
		}

		outcome, err := e.converge(ctx, rec)
		if err != nil {
			return result, err
		}
		if outcome.converged {
			result.Converged = append(result.Converged, rec.ID)
			continue
		}
		item.Term = term.DidNotHold
		item.Attempts++
		item.NextAttemptAt = now.Add(backoff(e.cfg.Resync, item.Attempts))
		item.LastRunLog = outcome.logPath
		if kind, terminal := terminalKind(outcome.repairErr); terminal {
			// A protected branch, a diverged remote, or a rejected credential
			// cannot be fixed by retrying. Block the item for a person; a
			// deferral clears it once the cause is fixed.
			item.Blocked = kind
			item.Term = term.NeedsOperator
			item.NextAttemptAt = time.Time{}
		} else if isBackupItem(rec.ID) && item.Attempts >= EscalateAfterAttempts {
			// On a single-disk host the checkout is the only copy, so a
			// retryable unpushed backlog outranks the rest of the punch list.
			// A terminal failure keeps needs-operator: retrying cannot help.
			item.Term = term.Escalated
		}
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

// convergeResult is one repair attempt and its verification.
type convergeResult struct {
	converged bool
	logPath   string
	repairErr error
}

// converge runs the record's repair, then re-measures on the verify ladder.
//
// It reports converged only when a verification measurement names the item and
// says live. A missing record and an unknown record both leave the repair
// unproven, so the item stays queued. A repair error is returned separately so
// the caller can tell a transient network fault from a terminal one.
func (e *Engine) converge(ctx context.Context, rec contract.Record) (convergeResult, error) {
	output, runErr := e.repairer.Repair(ctx, rec)

	logPath, logErr := e.store.RunLog(rec.ID, e.now(), []byte(formatRun(rec, output, runErr)))
	if logErr != nil {
		return convergeResult{}, logErr
	}
	if runErr != nil {
		return convergeResult{logPath: logPath, repairErr: runErr}, nil
	}

	for _, delay := range VerifyDelays {
		if err := e.sleep(ctx, delay); err != nil {
			return convergeResult{logPath: logPath}, err
		}
		records, err := e.measure(ctx)
		if err != nil {
			return convergeResult{logPath: logPath}, fmt.Errorf("verify %s: %w", rec.ID, err)
		}
		if err := e.record(records, e.now()); err != nil {
			return convergeResult{logPath: logPath}, err
		}
		if st, ok := stateOf(records, rec.ID); ok && st == contract.Live {
			return convergeResult{converged: true, logPath: logPath}, nil
		}
	}
	return convergeResult{logPath: logPath}, nil
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
func formatRun(rec contract.Record, output string, runErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "item: %s\n", rec.ID)
	fmt.Fprintf(&b, "state: %s\n", rec.State)
	fmt.Fprintf(&b, "detail: %s\n", rec.Detail)
	if rec.Fix != "" {
		fmt.Fprintf(&b, "verb: %s\n", rec.Fix)
	}
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

// EscalateAfterAttempts is the number of failed non-terminal pushes after which
// an unpushed backup outranks the rest of the punch list.
const EscalateAfterAttempts = 3

// isBackupItem reports whether id names an outbound backup check.
func isBackupItem(id string) bool {
	return strings.HasPrefix(id, "backup-")
}

// terminalKind returns the classified kind when a repair failure cannot be
// fixed by retrying. A network fault is the only retryable kind.
func terminalKind(err error) (string, bool) {
	var pe *measure.PushError
	if errors.As(err, &pe) && !pe.Retryable() {
		return pe.Kind, true
	}
	return "", false
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
		queue.Items[i].Term = term.Deferred
		queue.Items[i].NextAttemptAt = until
		// A deferral is the operator's action on a terminal push failure, so it
		// clears the block and lets the next attempt try again.
		queue.Items[i].Blocked = ""
		found = true
		break
	}
	if !found {
		queue.Items = append(queue.Items, state.Item{
			ID:            id,
			Term:          term.Deferred,
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
