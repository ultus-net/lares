package reconcile

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/state"
)

func TestRebootBlockedByWindow(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.cfg.WindowStart = "02:00"
	h.engine.cfg.WindowEnd = "05:00"
	if err := h.engine.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h.engine.now = func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local) }

	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed {
		t.Fatal("reboot allowed outside the window")
	}
	if verdict.Term != TermWindow {
		t.Errorf("term %q, want %q", verdict.Term, TermWindow)
	}
}

func TestRebootBlockedByHold(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	hold := state.Hold{Owner: "opencode", Reason: "agent run", PID: os.Getpid(), Start: fixedNow.Add(-2 * time.Hour)}
	if err := h.store.AddHold(hold); err != nil {
		t.Fatal(err)
	}
	h.engine.session = func(context.Context) (bool, error) { return false, nil }
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermHold {
		t.Fatalf("verdict %+v, want blocked on hold", verdict)
	}
	if verdict.HoldAge != 2*time.Hour {
		t.Errorf("hold age %v, want 2h", verdict.HoldAge)
	}
}

func TestRebootIgnoresStaleHold(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	// A hold with no live pid is stale by definition.
	hold := state.Hold{Owner: "dead-agent", Reason: "agent run", Start: fixedNow.Add(-time.Hour)}
	if err := h.store.AddHold(hold); err != nil {
		t.Fatal(err)
	}
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("stale hold blocked the reboot: %+v", verdict)
	}
	holds, err := h.store.HoldsIncludingStale()
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 0 {
		t.Errorf("stale hold survived: %+v", holds)
	}
}

func TestRebootBlockedBySession(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.session = true
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermSession {
		t.Fatalf("verdict %+v, want blocked on session", verdict)
	}
}

func TestRebootBlockedByTrain(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.train = true
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermOwnJob {
		t.Fatalf("verdict %+v, want blocked on the train", verdict)
	}
}

func TestRebootBlockedWhileBusy(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.busy.Store(true)
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermOwnJob {
		t.Fatalf("verdict %+v, want blocked while the engine is busy", verdict)
	}
}

func TestRebootAllowedActivates(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.adapters = map[adapter.Family]adapter.Adapter{
		adapter.Arch: {Family: adapter.Arch, Stage: []string{"patch-train-update"}, Activate: []string{"omarchy-system", "reboot"}},
	}
	if _, err := h.store.SaveQueue(state.Queue{Items: []state.Item{{ID: RebootID, Term: TermHold}}}); err != nil {
		t.Fatal(err)
	}

	verdict, err := h.engine.Reboot(context.Background())
	if err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if !verdict.Allowed {
		t.Fatalf("reboot blocked: %+v", verdict)
	}
	if len(h.activations) != 1 || strings.Join(h.activations[0], " ") != "omarchy-system reboot" {
		t.Fatalf("activations %v, want omarchy-system reboot", h.activations)
	}
	if items := h.queued(t); len(items) != 0 {
		t.Errorf("queue %v, want the reboot item cleared", items)
	}
}

func TestRebootBlockedQueuesTerm(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.session = true
	verdict, err := h.engine.Reboot(context.Background())
	if err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	if verdict.Allowed {
		t.Fatal("reboot ran while a session was active")
	}
	items := h.queued(t)
	item, ok := items[RebootID]
	if !ok {
		t.Fatal("blocked reboot was not queued")
	}
	if item.Term != TermSession {
		t.Errorf("term %q, want %q", item.Term, TermSession)
	}
	if len(h.activations) != 0 {
		t.Errorf("activations %v, want none", h.activations)
	}
}

func TestRebootUnknownFamilyIsAnError(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.family = adapter.Unknown
	h.engine.adapters = map[adapter.Family]adapter.Adapter{}
	if _, err := h.engine.Reboot(context.Background()); !errors.Is(err, adapter.ErrUnknownFamily) {
		t.Fatalf("error %v does not wrap ErrUnknownFamily", err)
	}
}

func TestRebootBlockedByUnclaimedAgent(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.agents = func() []string { return []string{"opencode"} }
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermHold {
		t.Fatalf("verdict %+v, want blocked on an unclaimed agent", verdict)
	}
	if len(verdict.Unclaimed) != 1 || verdict.Unclaimed[0] != "opencode" {
		t.Errorf("unclaimed %v, want [opencode]", verdict.Unclaimed)
	}
}

func TestUnclaimedAgents(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.agents = func() []string { return []string{"opencode", "orca"} }

	if got := h.engine.unclaimedAgents(nil); len(got) != 2 {
		t.Errorf("unclaimed %v, want both agents", got)
	}
	holds := []state.Hold{{Owner: "pid-123", Reason: "opencode session"}}
	got := h.engine.unclaimedAgents(holds)
	if len(got) != 1 || got[0] != "orca" {
		t.Errorf("unclaimed %v, want [orca]", got)
	}
}

func TestSessionErrorFailsClosed(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.session = func(context.Context) (bool, error) {
		return false, errors.New("loginctl missing")
	}
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermSession {
		t.Fatalf("verdict %+v, want blocked when the session state is unknown", verdict)
	}
}
