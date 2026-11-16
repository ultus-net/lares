package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/measure"
	"github.com/ultus-net/lares/internal/state"
	"github.com/ultus-net/lares/internal/term"
)

// fixedNow is the clock every test starts from.
var fixedNow = time.Date(2026, 11, 15, 3, 0, 0, 0, time.Local)

// harness wires an Engine to scripted measurements and records its calls.
type harness struct {
	engine   *Engine
	store    *state.Store
	stateDir string

	measures    [][]contract.Record
	runs        [][]string
	sleeps      []time.Duration
	activations [][]string
	notified    []sentNotice
	session     bool
	train       bool
}

// sentNotice is one desktop notification the engine asked for.
type sentNotice struct {
	Title  string
	Body   string
	Urgent bool
}

// fakeRepairer stands in for the measure engine. It repairs the repository
// fast-forward and records the verb it would have run.
type fakeRepairer struct {
	h *harness
}

// Safe reports whether rec is the repository item with a verb.
func (f fakeRepairer) Safe(rec contract.Record) bool {
	return rec.ID == "repo-demo" && rec.Fix != ""
}

// Term names the precondition for the arbitrary items the tests queue, so the
// tests prove the reconciler asks the repairer instead of hard-coding a term.
func (f fakeRepairer) Term(rec contract.Record) string {
	switch rec.ID {
	case "maintenance", "hermes":
		return term.NeedsTTY
	case "patch-train":
		return term.NeedsGrant
	default:
		return term.NeedsOperator
	}
}

// Repair records the verb and reports a converged run.
func (f fakeRepairer) Repair(_ context.Context, rec contract.Record) (string, error) {
	f.h.runs = append(f.h.runs, strings.Fields(rec.Fix))
	return "converged\n", nil
}

// harnessOptions names what a test overrides in the default harness.
type harnessOptions struct {
	family    adapter.Family
	adapters  map[adapter.Family]adapter.Adapter
	staged    func(context.Context) (bool, error)
	runLog    func(item string, at time.Time, body []byte) (string, error)
	notify    string
	notifyErr error
	reboot    bool
}

// newHarness builds an engine whose probe returns the scripted measurements in
// order, repeating the last one.
//
// The reboot gate is off by default so a pass test asserts the pass alone. A
// test that wants the gate sets cfg.RebootFromPass and an adapter.
func newHarness(t *testing.T, script ...[]contract.Record) *harness {
	t.Helper()
	return newHarnessWith(t, harnessOptions{}, script...)
}

// newHarnessWith is newHarness with the family, adapters, and staged check
// replaced, for tests that exercise the Fedora staging policy.
func newHarnessWith(t *testing.T, opts harnessOptions, script ...[]contract.Record) *harness {
	t.Helper()
	if opts.family == "" {
		opts.family = adapter.Arch
	}
	if opts.staged == nil {
		opts.staged = func(context.Context) (bool, error) { return false, nil }
	}
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	h := &harness{store: store, stateDir: dir, measures: script}
	index := 0
	cfg := testConfig(dir, opts.reboot)
	if opts.notify != "" {
		cfg.Notify = opts.notify
		if err := cfg.Validate(); err != nil {
			t.Fatalf("config: %v", err)
		}
	}
	engine, err := New(Options{
		Config:   cfg,
		Store:    store,
		Family:   opts.family,
		Adapters: opts.adapters,
		Now:      func() time.Time { return fixedNow },
		Repairer: fakeRepairer{h: h},
		Measure: func(context.Context) ([]contract.Record, error) {
			if len(h.measures) == 0 {
				return nil, errors.New("no measurement scripted")
			}
			i := index
			if i >= len(h.measures) {
				i = len(h.measures) - 1
			}
			index++
			return h.measures[i], nil
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			return nil
		},
		RunLog:  opts.runLog,
		Session: func(context.Context) (bool, error) { return h.session, nil },
		Train:   func(context.Context) (bool, error) { return h.train, nil },
		Staged:  opts.staged,
		Agents:  func() ([]Agent, error) { return nil, nil },
		Activate: func(_ context.Context, argv []string) error {
			h.activations = append(h.activations, argv)
			return nil
		},
		Notify: func(_ context.Context, title, body string, urgent bool) error {
			h.notified = append(h.notified, sentNotice{Title: title, Body: body, Urgent: urgent})
			return opts.notifyErr
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.engine = engine
	return h
}

// testConfig builds a valid config over dir.
func testConfig(dir string, rebootFromPass bool) config.Config {
	cfg := config.Default()
	cfg.StateDir = dir
	cfg.ConfigDir = dir
	cfg.WindowStart = "00:00"
	cfg.WindowEnd = "23:59"
	cfg.RebootFromPass = rebootFromPass
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return cfg
}

// secondEngine opens a second engine over an existing state directory, which is
// what a second lares process sees.
func secondEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	engine, err := New(Options{
		Config:   testConfig(dir, false),
		Store:    store,
		Family:   adapter.Arch,
		Now:      func() time.Time { return fixedNow },
		Repairer: noRepairer{},
		Measure: func(context.Context) ([]contract.Record, error) {
			return []contract.Record{summary(contract.Live)}, nil
		},
		Agents: func() ([]Agent, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return engine
}

func TestNewUsesNoRepairerForAnExternalProbe(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	cfg := testConfig(dir, false)
	cfg.AuditCommand = []string{"bash", "-c", "true"}
	engine, err := New(Options{
		Config:  cfg,
		Store:   store,
		Family:  adapter.Arch,
		Measure: func(context.Context) ([]contract.Record, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := engine.repairer.(noRepairer); !ok {
		t.Fatalf("repairer %T, want noRepairer for an external probe", engine.repairer)
	}
	if engine.repairer.Safe(rec("external", contract.Stale, "fix")) {
		t.Error("an external probe record must never be safe to repair")
	}
}

// rec builds a non-summary record.
func rec(id string, st contract.State, fix string) contract.Record {
	return contract.Record{ID: id, State: st, Detail: string(st), Fix: fix}
}

// summary builds the trailing summary record.
func summary(st contract.State) contract.Record {
	return contract.Record{ID: contract.SummaryID, State: st, Detail: "counts"}
}

// withVerifyDelays shrinks the verification ladder for the duration of a test.
func withVerifyDelays(t *testing.T, delays ...time.Duration) {
	t.Helper()
	previous := VerifyDelays
	VerifyDelays = delays
	t.Cleanup(func() { VerifyDelays = previous })
}

// queued reads the queue and indexes it by id.
func (h *harness) queued(t *testing.T) map[string]state.Item {
	t.Helper()
	queue, _, err := h.store.Queue()
	if err != nil {
		if errors.Is(err, state.ErrNoQueue) {
			return map[string]state.Item{}
		}
		t.Fatalf("Queue: %v", err)
	}
	items := make(map[string]state.Item, len(queue.Items))
	for _, item := range queue.Items {
		items[item.ID] = item
	}
	return items
}

// repoDrift is one safe finding, paired with its summary.
func repoDrift() []contract.Record {
	return []contract.Record{
		rec("repo-demo", contract.NotLive, "fast-forward"),
		summary(contract.NotLive),
	}
}

// repoLive is the same item after the repair landed.
func repoLive() []contract.Record {
	return []contract.Record{rec("repo-demo", contract.Live, ""), summary(contract.Live)}
}

func TestPassConvergesSafeItem(t *testing.T) {
	withVerifyDelays(t, time.Second)
	h := newHarness(t, repoDrift(), repoLive())

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Converged) != 1 || result.Converged[0] != "repo-demo" {
		t.Fatalf("converged %v", result.Converged)
	}
	if len(h.runs) != 1 {
		t.Fatalf("ran %d commands, want 1", len(h.runs))
	}
	want := "fast-forward"
	if got := strings.Join(h.runs[0], " "); got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
	if items := h.queued(t); len(items) != 0 {
		t.Errorf("queue %v, want empty", items)
	}
	logs, err := filepath.Glob(filepath.Join(h.stateDir, "runs", "repo-demo-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("got %d run logs, want 1", len(logs))
	}
	verdicts, err := h.store.Verdicts()
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 2 {
		t.Errorf("got %d verdicts, want 2 (measure and verify)", len(verdicts))
	}
	if last := verdicts[len(verdicts)-1]; last.State != "live" {
		t.Errorf("newest verdict is %q, want live", last.State)
	}
}

// backupStale is one safe backup finding: the local checkout is ahead, so a
// push is the repair.
func backupStale() []contract.Record {
	return []contract.Record{
		rec("backup-demo", contract.Stale, "push"),
		summary(contract.Stale),
	}
}

// pushRepairer fails every repair with a classified push error and counts the
// calls, so a test can prove a terminal failure is not retried.
type pushRepairer struct {
	err   error
	calls *int
}

func (pushRepairer) Safe(contract.Record) bool   { return true }
func (pushRepairer) Term(contract.Record) string { return term.NeedsOperator }
func (p pushRepairer) Repair(context.Context, contract.Record) (string, error) {
	if p.calls != nil {
		*p.calls = *p.calls + 1
	}
	return "push failed\n", p.err
}

func TestPassEscalatesAnUnpushedBackup(t *testing.T) {
	h := newHarness(t, backupStale())
	if _, err := h.store.SaveQueue(state.Queue{Items: []state.Item{{
		ID:        "backup-demo",
		Term:      term.DidNotHold,
		FirstSeen: fixedNow,
		Attempts:  EscalateAfterAttempts - 1,
	}}}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}
	// A network fault is retryable, so the failed attempt may escalate.
	h.engine.repairer = pushRepairer{err: &measure.PushError{Kind: "network", Err: errors.New("connection refused")}}

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	item := h.queued(t)["backup-demo"]
	if item.Term != term.Escalated {
		t.Errorf("term %q after %d attempts, want %q", item.Term, EscalateAfterAttempts, term.Escalated)
	}
	if item.Attempts != EscalateAfterAttempts {
		t.Errorf("attempts %d, want %d", item.Attempts, EscalateAfterAttempts)
	}
}

func TestPassNeverEscalatesATerminalPush(t *testing.T) {
	h := newHarness(t, backupStale())
	if _, err := h.store.SaveQueue(state.Queue{Items: []state.Item{{
		ID:        "backup-demo",
		Term:      term.DidNotHold,
		FirstSeen: fixedNow,
		Attempts:  EscalateAfterAttempts - 1,
	}}}); err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}
	h.engine.repairer = pushRepairer{err: &measure.PushError{Kind: "diverged", Err: errors.New("non-fast-forward")}}

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	item := h.queued(t)["backup-demo"]
	if item.Term != term.NeedsOperator {
		t.Errorf("term %q, want %q: a terminal push must not escalate", item.Term, term.NeedsOperator)
	}
	if item.Blocked != "diverged" {
		t.Errorf("blocked %q, want diverged", item.Blocked)
	}
}

func TestPassQueuesAnUnsafeBackupWithoutEscalating(t *testing.T) {
	h := newHarness(t, backupStale())
	// fakeRepairer marks backup-demo unsafe, so no push runs. An unsafe
	// finding must not accrue attempts or escalate: only a failed repair can.
	for i := 0; i < EscalateAfterAttempts+1; i++ {
		if _, err := h.engine.Pass(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	item := h.queued(t)["backup-demo"]
	if item.Term != term.NeedsOperator {
		t.Errorf("term %q, want %q", item.Term, term.NeedsOperator)
	}
	if item.Attempts != 0 {
		t.Errorf("attempts %d, want 0: an unsafe finding is never retried", item.Attempts)
	}
}

func TestPassStopsRetryingATerminalPush(t *testing.T) {
	h := newHarness(t, backupStale())
	calls := 0
	h.engine.repairer = pushRepairer{
		err:   &measure.PushError{Kind: "protected", Err: errors.New("protected branch")},
		calls: &calls,
	}

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	item := h.queued(t)["backup-demo"]
	if item.Term != term.NeedsOperator {
		t.Errorf("term %q, want %q", item.Term, term.NeedsOperator)
	}
	if item.Blocked != "protected" {
		t.Errorf("blocked %q, want protected", item.Blocked)
	}
	if !item.NextAttemptAt.IsZero() {
		t.Errorf("next attempt %v, want no retry scheduled", item.NextAttemptAt)
	}

	// A second pass must not run the terminal repair again, even though the
	// measurement still reports a stale checkout.
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	if calls != 1 {
		t.Errorf("repair ran %d times, want 1: a terminal failure was retried", calls)
	}
}

func TestDeferClearsATerminalPush(t *testing.T) {
	h := newHarness(t, backupStale())
	h.engine.repairer = pushRepairer{err: &measure.PushError{Kind: "auth", Err: errors.New("permission denied")}}
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, err := h.engine.Defer("backup-demo", 2*time.Minute, "rotated the deploy key"); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	item := h.queued(t)["backup-demo"]
	if item.Blocked != "" {
		t.Errorf("blocked %q after a deferral, want cleared", item.Blocked)
	}
	if item.Term != term.Deferred {
		t.Errorf("term %q, want %q", item.Term, term.Deferred)
	}
}

func TestPassRetriesANetworkPushWithBackoff(t *testing.T) {
	h := newHarness(t, backupStale())
	h.engine.repairer = pushRepairer{err: &measure.PushError{Kind: "network", Err: errors.New("connection refused")}}

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	item := h.queued(t)["backup-demo"]
	if item.Term != term.DidNotHold {
		t.Errorf("term %q, want %q", item.Term, term.DidNotHold)
	}
	if want := fixedNow.Add(15 * time.Minute); !item.NextAttemptAt.Equal(want) {
		t.Errorf("next attempt %v, want %v", item.NextAttemptAt, want)
	}
}

func TestPassEscalatesWhenStateDoesNotMove(t *testing.T) {
	withVerifyDelays(t, time.Second, 2*time.Second, 3*time.Second)
	h := newHarness(t, repoDrift())

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Escalated) != 1 || result.Escalated[0] != "repo-demo" {
		t.Fatalf("escalated %v", result.Escalated)
	}
	if len(h.sleeps) != 3 {
		t.Errorf("verified %d times, want 3", len(h.sleeps))
	}
	items := h.queued(t)
	item, ok := items["repo-demo"]
	if !ok {
		t.Fatal("profile not queued after an unchanged measurement")
	}
	if item.Attempts != 1 {
		t.Errorf("attempts %d, want 1", item.Attempts)
	}
	if item.Term != term.DidNotHold {
		t.Errorf("term %q, want %q", item.Term, term.DidNotHold)
	}
	if !item.NextAttemptAt.Equal(fixedNow.Add(15 * time.Minute)) {
		t.Errorf("next attempt %v, want %v", item.NextAttemptAt, fixedNow.Add(15*time.Minute))
	}
	if item.LastRunLog == "" {
		t.Error("run log path not recorded")
	}
}

// TestPassChargesAnAttemptWhenTheRunLogFails proves a repair that ran still
// earns an attempt and a backoff when the pass aborts on the run log. Without
// the charge, the next pass retries the same repair at the normal cadence.
//
// The run-log seam fails deterministically, so the case does not depend on the
// uid or on filesystem permissions.
func TestPassChargesAnAttemptWhenTheRunLogFails(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{
		runLog: func(string, time.Time, []byte) (string, error) {
			return "", errors.New("run log unavailable")
		},
	}, repoDrift())

	if _, err := h.engine.Pass(context.Background()); err == nil {
		t.Fatal("Pass succeeded with a failing run log")
	}
	if len(h.runs) != 1 {
		t.Fatalf("ran %d commands, want 1: the attempt must be charged after the repair", len(h.runs))
	}
	item, ok := h.queued(t)["repo-demo"]
	if !ok {
		t.Fatal("the attempted item is missing from the queue")
	}
	if item.Attempts != 1 {
		t.Errorf("attempts %d, want 1", item.Attempts)
	}
	if item.Term != term.DidNotHold {
		t.Errorf("term %q, want %q", item.Term, term.DidNotHold)
	}
	if want := fixedNow.Add(15 * time.Minute); !item.NextAttemptAt.Equal(want) {
		t.Errorf("next attempt %v, want %v", item.NextAttemptAt, want)
	}
}

func TestPassQueuesUnsafeItemWithoutRunning(t *testing.T) {
	h := newHarness(t, []contract.Record{
		rec("offsite", contract.NotLive, "install/offsite-oauth-wizard.sh"),
		summary(contract.NotLive),
	})
	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Queued) != 1 || result.Queued[0] != "offsite" {
		t.Fatalf("queued %v", result.Queued)
	}
	if len(h.runs) != 0 {
		t.Errorf("ran %v, want no command for an unsafe item", h.runs)
	}
	items := h.queued(t)
	item, ok := items["offsite"]
	if !ok {
		t.Fatal("offsite not queued")
	}
	if item.Term != term.NeedsOperator {
		t.Errorf("term %q, want %q", item.Term, term.NeedsOperator)
	}
	if !item.FirstSeen.Equal(fixedNow) {
		t.Errorf("first seen %v, want %v", item.FirstSeen, fixedNow)
	}
}

func TestPassQueuesTheBlockingPrecondition(t *testing.T) {
	h := newHarness(t, []contract.Record{
		rec("hermes", contract.NotLive, "install/host-setup.sh --only hermes --apply"),
		rec("patch-train", contract.NotLive, "install/host-setup.sh --only patch-train --apply"),
		rec("notebook", contract.NotLive, "install/host-setup.sh --only notebook --apply"),
		summary(contract.NotLive),
	})
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(h.runs) != 0 {
		t.Errorf("ran %v, want no command outside the safe set", h.runs)
	}
	want := map[string]string{
		"hermes":      term.NeedsTTY,
		"patch-train": term.NeedsGrant,
		"notebook":    term.NeedsOperator,
	}
	items := h.queued(t)
	for id, term := range want {
		if got := items[id].Term; got != term {
			t.Errorf("%s term %q, want %q", id, got, term)
		}
	}
}

func TestPassRespectsBackoff(t *testing.T) {
	withVerifyDelays(t, time.Second)
	h := newHarness(t, repoDrift())
	future := fixedNow.Add(time.Hour)
	queue := state.Queue{Items: []state.Item{{ID: "repo-demo", Term: term.DidNotHold, FirstSeen: fixedNow.Add(-time.Hour), Attempts: 2, NextAttemptAt: future}}}
	if _, err := h.store.SaveQueue(queue); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "repo-demo" {
		t.Fatalf("skipped %v", result.Skipped)
	}
	if len(h.runs) != 0 {
		t.Errorf("ran %v during backoff", h.runs)
	}
	items := h.queued(t)
	if items["repo-demo"].Term != term.Backoff {
		t.Errorf("term %q, want %q", items["repo-demo"].Term, term.Backoff)
	}
	if items["repo-demo"].Attempts != 2 {
		t.Errorf("attempts changed to %d during backoff", items["repo-demo"].Attempts)
	}
}

func TestPassKeepsAttemptsAcrossFailures(t *testing.T) {
	withVerifyDelays(t, time.Second)
	h := newHarness(t, repoDrift(), repoDrift())
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("first Pass: %v", err)
	}

	// Move the clock past the backoff by rewriting the item's due time.
	items := h.queued(t)
	item := items["repo-demo"]
	item.NextAttemptAt = fixedNow.Add(-time.Minute)
	if _, err := h.store.SaveQueue(state.Queue{Items: []state.Item{item}}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	items = h.queued(t)
	if got := items["repo-demo"].Attempts; got != 2 {
		t.Errorf("attempts %d, want 2", got)
	}
	wantNext := fixedNow.Add(30 * time.Minute)
	if got := items["repo-demo"].NextAttemptAt; !got.Equal(wantNext) {
		t.Errorf("next attempt %v, want %v", got, wantNext)
	}
}

func TestPassDropsItemWhenItLeavesTheQueue(t *testing.T) {
	h := newHarness(t, []contract.Record{
		rec("offsite", contract.Live, ""),
		summary(contract.Live),
	})
	queue := state.Queue{Items: []state.Item{{ID: "offsite", Term: term.NeedsOperator, FirstSeen: fixedNow.Add(-time.Hour), Attempts: 5}}}
	if _, err := h.store.SaveQueue(queue); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if items := h.queued(t); len(items) != 0 {
		t.Errorf("queue %v, want empty after the finding cleared", items)
	}
}

func TestPassQueuesUnknownWithoutRunning(t *testing.T) {
	h := newHarness(t, []contract.Record{
		rec("backup", contract.Unknown, "install/host-setup.sh --only backup --apply"),
		summary(contract.Unknown),
	})
	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Queued) != 1 || result.Queued[0] != "backup" {
		t.Fatalf("queued %v, want the unknown item", result.Queued)
	}
	if len(h.runs) != 0 {
		t.Errorf("ran %v, want no repair on an unknown measurement", h.runs)
	}
	item, ok := h.queued(t)["backup"]
	if !ok {
		t.Fatal("an unknown measurement vanished from the queue")
	}
	if item.Term != term.Unverified {
		t.Errorf("term %q, want %q", item.Term, term.Unverified)
	}
}

func TestPassRequiresALiveVerification(t *testing.T) {
	withVerifyDelays(t, time.Second, 2*time.Second)
	h := newHarness(t,
		repoDrift(),
		[]contract.Record{rec("repo-demo", contract.Unknown, ""), summary(contract.Unknown)},
	)
	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Converged) != 0 {
		t.Fatalf("converged %v on an unknown verification", result.Converged)
	}
	if len(result.Escalated) != 1 || result.Escalated[0] != "repo-demo" {
		t.Fatalf("escalated %v", result.Escalated)
	}
	if got := h.queued(t)["repo-demo"].Term; got != term.DidNotHold {
		t.Errorf("term %q, want %q", got, term.DidNotHold)
	}
}

func TestPassRequiresTheRecordToBePresent(t *testing.T) {
	withVerifyDelays(t, time.Second, 2*time.Second)
	h := newHarness(t,
		repoDrift(),
		[]contract.Record{summary(contract.Live)},
	)
	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Converged) != 0 {
		t.Fatalf("converged %v with the item absent from the verification", result.Converged)
	}
	if len(result.Escalated) != 1 {
		t.Fatalf("escalated %v", result.Escalated)
	}
	if got := h.queued(t)["repo-demo"].Term; got != term.DidNotHold {
		t.Errorf("term %q, want %q: an absent verification must not converge", got, term.DidNotHold)
	}
}

func TestPassNamesAFailedScan(t *testing.T) {
	h := newHarness(t, repoDrift())
	h.engine.agents = func() ([]Agent, error) { return nil, errors.New("cannot read /proc") }

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(h.runs) != 0 {
		t.Errorf("ran %v when no process could be checked", h.runs)
	}
	item, ok := h.queued(t)["repo-demo"]
	if !ok {
		t.Fatal("the item vanished when the process scan failed")
	}
	if item.Term != term.Scan {
		t.Errorf("term %q, want %q", item.Term, term.Scan)
	}
}

func TestPassWaitsWhileAHoldIsLive(t *testing.T) {
	h := newHarness(t, repoDrift())
	hold := state.Hold{Owner: "pid-agent", Reason: "agent run", PID: os.Getpid(), Start: fixedNow.Add(-time.Minute)}
	if err := h.store.AddHold(hold); err != nil {
		t.Fatal(err)
	}
	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(h.runs) != 0 {
		t.Errorf("ran %v while a hold was live", h.runs)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "repo-demo" {
		t.Fatalf("skipped %v", result.Skipped)
	}
	if got := h.queued(t)["repo-demo"].Term; got != term.Hold {
		t.Errorf("term %q, want %q", got, term.Hold)
	}
}

func TestPassWaitsWhileAnAgentHasNoHold(t *testing.T) {
	h := newHarness(t, repoDrift())
	h.engine.agents = func() ([]Agent, error) { return []Agent{{PID: 4242, Name: "opencode"}}, nil }

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(h.runs) != 0 {
		t.Errorf("ran %v over an unclaimed agent", h.runs)
	}
	if len(result.Skipped) != 1 {
		t.Fatalf("skipped %v, want the item held", result.Skipped)
	}
	if got := h.queued(t)["repo-demo"].Term; got != term.Hold {
		t.Errorf("term %q, want %q", got, term.Hold)
	}
}

func TestPassFailsWhenMeasureFails(t *testing.T) {
	h := newHarness(t)
	if _, err := h.engine.Pass(context.Background()); err == nil {
		t.Fatal("Pass succeeded with no measurement")
	}
}

func TestSecondEngineCannotPassWhileTheFirstHoldsTheLock(t *testing.T) {
	withVerifyDelays(t, time.Millisecond)
	h := newHarness(t, repoDrift())
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.engine.measure = func(context.Context) ([]contract.Record, error) {
		once.Do(func() { close(entered) })
		<-release
		return repoDrift(), nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Pass(context.Background())
		done <- err
	}()
	<-entered // Pass holds the lock before it measures.

	second := secondEngine(t, h.stateDir)
	if _, err := second.Pass(context.Background()); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("second pass error %v, want ErrLocked", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first Pass: %v", err)
	}
}

func TestPassAppendsVerificationVerdicts(t *testing.T) {
	withVerifyDelays(t, time.Second)
	h := newHarness(t, repoDrift(), repoLive())
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	verdicts, err := h.store.Verdicts()
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, v := range verdicts {
		if v.ID == "repo-demo" {
			states = append(states, v.State)
		}
	}
	if len(states) != 2 || states[0] != "not-live" || states[1] != "live" {
		t.Fatalf("profile verdicts %v, want [not-live live]", states)
	}
}

func TestDeferWritesAVerdictAndMovesTheItem(t *testing.T) {
	h := newHarness(t, []contract.Record{
		rec("offsite", contract.NotLive, "install/offsite-oauth-wizard.sh"),
		summary(contract.NotLive),
	})
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}

	until, err := h.engine.Defer("offsite", 2*time.Hour, "waiting on the provider")
	if err != nil {
		t.Fatalf("Defer: %v", err)
	}
	if want := fixedNow.Add(2 * time.Hour); !until.Equal(want) {
		t.Errorf("until %v, want %v", until, want)
	}
	item := h.queued(t)["offsite"]
	if item.Term != term.Deferred {
		t.Errorf("term %q, want %q", item.Term, term.Deferred)
	}
	if !item.NextAttemptAt.Equal(fixedNow.Add(2 * time.Hour)) {
		t.Errorf("next attempt %v", item.NextAttemptAt)
	}

	verdicts, err := h.store.Verdicts()
	if err != nil {
		t.Fatal(err)
	}
	var record *state.Verdict
	for i := range verdicts {
		if verdicts[i].IsDeferral() {
			record = &verdicts[i]
		}
	}
	if record == nil {
		t.Fatal("no defer record was appended to the verdict log")
	}
	if record.ID != "offsite" || record.Kind != state.KindDefer {
		t.Errorf("defer record %+v, want id offsite and kind %q", record, state.KindDefer)
	}
	if record.Detail != "waiting on the provider" {
		t.Errorf("defer record detail %q, want the reason", record.Detail)
	}
	if !record.Until.Equal(fixedNow.Add(2 * time.Hour)) {
		t.Errorf("defer record until %v, want %v", record.Until, fixedNow.Add(2*time.Hour))
	}

	deferred, err := h.store.Deferrals()
	if err != nil {
		t.Fatal(err)
	}
	if deferred["offsite"].Detail != "waiting on the provider" {
		t.Errorf("deferral %+v, want the reason recorded", deferred["offsite"])
	}

	// A later pass replays the deferral even though the probe still reports it.
	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "offsite" {
		t.Fatalf("skipped %v, want the deferred item", result.Skipped)
	}
	if got := h.queued(t)["offsite"].Term; got != term.Deferred {
		t.Errorf("term %q after replay, want %q", got, term.Deferred)
	}
}

func TestDeferRejectsBadInput(t *testing.T) {
	h := newHarness(t, []contract.Record{summary(contract.Live)})
	if _, err := h.engine.Defer("a/b", time.Hour, "reason"); err == nil {
		t.Error("Defer accepted a non-slug id")
	}
	if _, err := h.engine.Defer("offsite", time.Second, "reason"); err == nil {
		t.Error("Defer accepted a sub-minute duration")
	}
}

func TestPassEvaluatesRebootFromPass(t *testing.T) {
	h := newHarness(t, []contract.Record{summary(contract.Live)})
	h.engine.cfg.RebootFromPass = true
	h.engine.adapters = map[adapter.Family]adapter.Adapter{
		adapter.Arch: {Family: adapter.Arch, Stage: []string{"patch-train-update"}, Activate: []string{"omarchy-system", "reboot"}},
	}

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if !result.Reboot.Allowed {
		t.Fatalf("pass verdict %+v, want allowed", result.Reboot)
	}
	if len(h.activations) != 1 || strings.Join(h.activations[0], " ") != "omarchy-system reboot" {
		t.Fatalf("activations %v, want omarchy-system reboot", h.activations)
	}
}

func TestPassQueuesABlockedReboot(t *testing.T) {
	h := newHarness(t, []contract.Record{summary(contract.Live)})
	h.engine.cfg.RebootFromPass = true
	h.engine.adapters = map[adapter.Family]adapter.Adapter{
		adapter.Arch: {Family: adapter.Arch, Stage: []string{"patch-train-update"}, Activate: []string{"omarchy-system", "reboot"}},
	}
	h.session = true

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if result.Reboot.Allowed || result.Reboot.Term != TermSession {
		t.Fatalf("verdict %+v, want blocked on session", result.Reboot)
	}
	item, ok := h.queued(t)[RebootID]
	if !ok {
		t.Fatal("the blocked reboot was not queued")
	}
	if item.Term != TermSession {
		t.Errorf("term %q, want %q", item.Term, TermSession)
	}
	if len(h.activations) != 0 {
		t.Errorf("activations %v, want none", h.activations)
	}
}

func TestPassKeepsAQueuedRebootItem(t *testing.T) {
	h := newHarness(t, []contract.Record{summary(contract.Live)})
	seed := state.Item{ID: RebootID, Term: TermWindow, FirstSeen: fixedNow.Add(-time.Hour)}
	if _, err := h.store.SaveQueue(state.Queue{Items: []state.Item{seed}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	item, ok := h.queued(t)[RebootID]
	if !ok {
		t.Fatal("the queued reboot was dropped by the pass")
	}
	if !item.FirstSeen.Equal(seed.FirstSeen) {
		t.Errorf("first seen %v, want %v", item.FirstSeen, seed.FirstSeen)
	}
}

func TestBackoff(t *testing.T) {
	base := 15 * time.Minute
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 15 * time.Minute},
		{2, 30 * time.Minute},
		{3, time.Hour},
		{6, 8 * time.Hour},
		{7, 16 * time.Hour},
		{8, 24 * time.Hour},
		{20, 24 * time.Hour},
	}
	for _, tt := range tests {
		if got := backoff(base, tt.attempts); got != tt.want {
			t.Errorf("backoff(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

func TestNoRepairerQueuesEverything(t *testing.T) {
	r := noRepairer{}
	record := rec("anything", contract.Stale, "some-verb")
	if r.Safe(record) {
		t.Error("an external record must never be safe")
	}
	if got := r.Term(record); got != term.NeedsOperator {
		t.Errorf("term %q, want %q", got, term.NeedsOperator)
	}
	if _, err := r.Repair(context.Background(), record); err == nil {
		t.Error("an external record was repaired unattended")
	}
}

func TestStatusReadsQueueHoldsAndVerdicts(t *testing.T) {
	h := newHarness(t, []contract.Record{summary(contract.Live)})
	if err := h.store.AppendVerdict(state.Verdict{ID: "hermes", State: "live", At: fixedNow}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	status, err := h.engine.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, ok := status.Verdicts["hermes"]; !ok {
		t.Error("hermes verdict missing")
	}
	if len(status.Holds) != 0 {
		t.Errorf("holds %v, want none", status.Holds)
	}
}

// TestStatusDoesNotPruneHolds proves a status read leaves a dead hold on disk:
// only a pass holds the lock and owns the cleanup.
func TestStatusDoesNotPruneHolds(t *testing.T) {
	h := newHarness(t)
	if err := h.store.AddHold(state.Hold{Owner: "dead", PID: 1 << 30, Start: fixedNow}); err != nil {
		t.Fatal(err)
	}
	status, err := h.engine.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(status.Holds) != 0 {
		t.Errorf("holds %v, want only live holds", status.Holds)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "holds", "dead.json")); err != nil {
		t.Errorf("Status removed a hold: %v", err)
	}
}

func TestStatusHidesDeferralsFromVerdicts(t *testing.T) {
	h := newHarness(t, []contract.Record{summary(contract.Live)})
	if err := h.store.AppendVerdict(state.Verdict{ID: "offsite", State: "not-live", At: fixedNow}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Defer("offsite", time.Hour, "later"); err != nil {
		t.Fatal(err)
	}
	status, err := h.engine.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Verdicts["offsite"].State; got != "not-live" {
		t.Errorf("state %q, want the measurement to survive the deferral", got)
	}
}

func TestNewRequiresStore(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted a nil store")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ResyncText = "1s"
	cfg.Resync = 0
	cfg.StateDir = "/tmp"
	cfg.ConfigDir = "/tmp"
	if _, err := New(Options{Config: cfg, Store: store}); err == nil {
		t.Fatal("New accepted an invalid config")
	}
}

func TestPassWritesRunLogContent(t *testing.T) {
	withVerifyDelays(t, time.Second)
	h := newHarness(t, repoDrift(), repoLive())
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	logs, err := filepath.Glob(filepath.Join(h.stateDir, "runs", "repo-demo-*.log"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("run logs %v (%v)", logs, err)
	}
	body, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"item: repo-demo", "verb: fast-forward", "converged"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("run log does not contain %q:\n%s", want, body)
		}
	}
}
