package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/state"
)

// fixedNow is the clock every test starts from.
var fixedNow = time.Date(2026, 11, 15, 3, 0, 0, 0, time.Local)

// harness wires an Engine to scripted measurements and records its calls.
type harness struct {
	engine   *Engine
	store    *state.Store
	stateDir string
	repoDir  string

	measures    [][]contract.Record
	runs        [][]string
	sleeps      []time.Duration
	activations [][]string
	session     bool
	train       bool
}

// newHarness builds an engine whose probe returns the scripted measurements in
// order, repeating the last one.
func newHarness(t *testing.T, script ...[]contract.Record) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	cfg := config.Default()
	cfg.StateDir = dir
	cfg.ConfigDir = dir
	cfg.WindowStart = "00:00"
	cfg.WindowEnd = "23:59"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.Validate: %v", err)
	}

	h := &harness{store: store, stateDir: dir, repoDir: dir, measures: script}
	index := 0
	engine, err := New(Options{
		Config:  cfg,
		Store:   store,
		Family:  adapter.Arch,
		RepoDir: dir,
		Now:     func() time.Time { return fixedNow },
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
		Run: func(_ context.Context, _ string, argv []string) (string, error) {
			h.runs = append(h.runs, argv)
			return "converged\n", nil
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			return nil
		},
		Session: func(context.Context) (bool, error) { return h.session, nil },
		Train:   func(context.Context) (bool, error) { return h.train, nil },
		Activate: func(_ context.Context, argv []string) error {
			h.activations = append(h.activations, argv)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.engine = engine
	return h
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

func TestPassConvergesSafeItem(t *testing.T) {
	withVerifyDelays(t, time.Second)
	h := newHarness(t,
		[]contract.Record{rec("maintenance", contract.NotLive, "install/host-setup.sh --only maintenance --apply"), summary(contract.NotLive)},
		[]contract.Record{rec("maintenance", contract.Live, ""), summary(contract.Live)},
	)

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Converged) != 1 || result.Converged[0] != "maintenance" {
		t.Fatalf("converged %v", result.Converged)
	}
	if len(h.runs) != 1 {
		t.Fatalf("ran %d commands, want 1", len(h.runs))
	}
	want := "install/host-setup.sh --only maintenance --apply"
	if got := strings.Join(h.runs[0], " "); got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
	if items := h.queued(t); len(items) != 0 {
		t.Errorf("queue %v, want empty", items)
	}
	logs, err := filepath.Glob(filepath.Join(h.stateDir, "runs", "maintenance-*.log"))
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
	if len(verdicts) != 1 {
		t.Errorf("got %d verdicts, want 1 (the summary is not a verdict)", len(verdicts))
	}
}

func TestPassEscalatesWhenStateDoesNotMove(t *testing.T) {
	withVerifyDelays(t, time.Second, 2*time.Second, 3*time.Second)
	drift := []contract.Record{
		rec("hermes", contract.NotLive, "install/host-setup.sh --only hermes --apply"),
		summary(contract.NotLive),
	}
	h := newHarness(t, drift)

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Escalated) != 1 || result.Escalated[0] != "hermes" {
		t.Fatalf("escalated %v", result.Escalated)
	}
	if len(h.sleeps) != 3 {
		t.Errorf("verified %d times, want 3", len(h.sleeps))
	}
	items := h.queued(t)
	item, ok := items["hermes"]
	if !ok {
		t.Fatal("hermes not queued after an unchanged measurement")
	}
	if item.Attempts != 1 {
		t.Errorf("attempts %d, want 1", item.Attempts)
	}
	if item.Term != termDidNotHold {
		t.Errorf("term %q, want %q", item.Term, termDidNotHold)
	}
	if !item.NextAttemptAt.Equal(fixedNow.Add(15 * time.Minute)) {
		t.Errorf("next attempt %v, want %v", item.NextAttemptAt, fixedNow.Add(15*time.Minute))
	}
	if item.LastRunLog == "" {
		t.Error("run log path not recorded")
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
	if item.Term != termNeedsOperator {
		t.Errorf("term %q, want %q", item.Term, termNeedsOperator)
	}
	if !item.FirstSeen.Equal(fixedNow) {
		t.Errorf("first seen %v, want %v", item.FirstSeen, fixedNow)
	}
}

func TestPassRespectsBackoff(t *testing.T) {
	withVerifyDelays(t, time.Second)
	h := newHarness(t, []contract.Record{
		rec("hermes", contract.NotLive, "install/host-setup.sh --only hermes --apply"),
		summary(contract.NotLive),
	})
	future := fixedNow.Add(time.Hour)
	queue := state.Queue{Items: []state.Item{{ID: "hermes", Term: termDidNotHold, FirstSeen: fixedNow.Add(-time.Hour), Attempts: 2, NextAttemptAt: future}}}
	if _, err := h.store.SaveQueue(queue); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "hermes" {
		t.Fatalf("skipped %v", result.Skipped)
	}
	if len(h.runs) != 0 {
		t.Errorf("ran %v during backoff", h.runs)
	}
	items := h.queued(t)
	if items["hermes"].Term != termBackoff {
		t.Errorf("term %q, want %q", items["hermes"].Term, termBackoff)
	}
	if items["hermes"].Attempts != 2 {
		t.Errorf("attempts changed to %d during backoff", items["hermes"].Attempts)
	}
}

func TestPassKeepsAttemptsAcrossFailures(t *testing.T) {
	withVerifyDelays(t, time.Second)
	drift := []contract.Record{
		rec("hermes", contract.NotLive, "install/host-setup.sh --only hermes --apply"),
		summary(contract.NotLive),
	}
	h := newHarness(t, drift, drift)
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("first Pass: %v", err)
	}

	// Move the clock past the backoff by rewriting the item's due time.
	items := h.queued(t)
	item := items["hermes"]
	item.NextAttemptAt = fixedNow.Add(-time.Minute)
	if _, err := h.store.SaveQueue(state.Queue{Items: []state.Item{item}}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	items = h.queued(t)
	if got := items["hermes"].Attempts; got != 2 {
		t.Errorf("attempts %d, want 2", got)
	}
	wantNext := fixedNow.Add(30 * time.Minute)
	if got := items["hermes"].NextAttemptAt; !got.Equal(wantNext) {
		t.Errorf("next attempt %v, want %v", got, wantNext)
	}
}

func TestPassDropsItemWhenItLeavesTheQueue(t *testing.T) {
	h := newHarness(t, []contract.Record{
		rec("offsite", contract.Live, ""),
		summary(contract.Live),
	})
	queue := state.Queue{Items: []state.Item{{ID: "offsite", Term: termNeedsOperator, FirstSeen: fixedNow.Add(-time.Hour), Attempts: 5}}}
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

func TestPassIgnoresUnknown(t *testing.T) {
	h := newHarness(t, []contract.Record{
		rec("backup", contract.Unknown, ""),
		summary(contract.Unknown),
	})
	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Queued) != 0 || len(result.Escalated) != 0 {
		t.Errorf("unknown record queued: %+v", result)
	}
	if items := h.queued(t); len(items) != 0 {
		t.Errorf("queue %v, want empty", items)
	}
}

func TestPassFailsWhenMeasureFails(t *testing.T) {
	h := newHarness(t)
	if _, err := h.engine.Pass(context.Background()); err == nil {
		t.Fatal("Pass succeeded with no measurement")
	}
}

func TestPassRefusesConcurrentRun(t *testing.T) {
	h := newHarness(t, []contract.Record{summary(contract.Live)})
	h.engine.busy.Store(true)
	if _, err := h.engine.Pass(context.Background()); err == nil {
		t.Fatal("Pass ran while another pass held the engine")
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

func TestSafeSet(t *testing.T) {
	for _, id := range []string{"hermes", "maintenance", "notebook", "profile"} {
		if !safeToConverge(id) {
			t.Errorf("%s should be safe", id)
		}
	}
	for _, id := range []string{"patch-train", "offsite", "backup", "branch-x", "reboot", ""} {
		if safeToConverge(id) {
			t.Errorf("%s should not be safe", id)
		}
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
	h := newHarness(t,
		[]contract.Record{rec("hermes", contract.NotLive, "install/host-setup.sh --only hermes --apply"), summary(contract.NotLive)},
		[]contract.Record{rec("hermes", contract.Live, ""), summary(contract.Live)},
	)
	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	logs, err := filepath.Glob(filepath.Join(h.stateDir, "runs", "hermes-*.log"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("run logs %v (%v)", logs, err)
	}
	body, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"item: hermes", "command: install/host-setup.sh --only hermes --apply", "converged"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("run log does not contain %q:\n%s", want, body)
		}
	}
}
