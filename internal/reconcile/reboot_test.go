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
	hold := state.Hold{Owner: "pid-opencode", Reason: "agent run", PID: os.Getpid(), Start: fixedNow.Add(-2 * time.Hour)}
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

func TestRebootFailsClosedWhenTheTrainIsUnknown(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.train = func(context.Context) (bool, error) {
		return false, errors.New("omarchy-update-lock is missing")
	}
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermOwnJob {
		t.Fatalf("verdict %+v, want blocked when the train state is unknown", verdict)
	}
}

func TestRebootRefusesWhenAnotherProcessHoldsTheLock(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	other, err := state.Open(h.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}
	defer other.Unlock()

	if _, err := h.engine.Reboot(context.Background()); !errors.Is(err, state.ErrLocked) {
		t.Fatalf("Reboot error %v, want ErrLocked", err)
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
	h.engine.agents = func() ([]Agent, error) { return []Agent{{PID: 4242, Name: "opencode"}}, nil }
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermHold {
		t.Fatalf("verdict %+v, want blocked on an unclaimed agent", verdict)
	}
	if len(verdict.Unclaimed) != 1 || verdict.Unclaimed[0] != "opencode[4242]" {
		t.Errorf("unclaimed %v, want [opencode[4242]]", verdict.Unclaimed)
	}
}

func TestUnclaimedAgentsMatchByPID(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.agents = func() ([]Agent, error) {
		return []Agent{{PID: 111, Name: "opencode"}, {PID: 222, Name: "orca"}}, nil
	}

	if got, err := h.engine.unclaimedAgents(nil); err != nil || len(got) != 2 {
		t.Errorf("unclaimed %v (%v), want both agents", got, err)
	}

	// A hold that only mentions the name must not claim the process.
	holds := []state.Hold{{Owner: "opencode-session", Reason: "opencode run", PID: 999}}
	got, err := h.engine.unclaimedAgents(holds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("unclaimed %v, want both agents: a name is not a pid", got)
	}

	// A hold with the pid claims exactly that process.
	holds = append(holds, state.Hold{Owner: "pid-111", Reason: "agent", PID: 111})
	got, err = h.engine.unclaimedAgents(holds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "orca[222]" {
		t.Errorf("unclaimed %v, want [orca[222]]", got)
	}
}

func TestRebootFailsClosedWhenTheAgentScanFails(t *testing.T) {
	h := newHarness(t, []contract.Record{})
	h.engine.agents = func() ([]Agent, error) { return nil, errors.New("cannot read /proc") }
	verdict, err := h.engine.RebootAllowed(context.Background())
	if err != nil {
		t.Fatalf("RebootAllowed: %v", err)
	}
	if verdict.Allowed || verdict.Term != TermScan {
		t.Fatalf("verdict %+v, want the scan term when no process could be checked", verdict)
	}
	if len(verdict.Unclaimed) != 1 || !strings.Contains(verdict.Unclaimed[0], "cannot read /proc") {
		t.Errorf("unclaimed %v does not name the failed scan", verdict.Unclaimed)
	}
}

func TestHoldClaimsRejectsAReusedPID(t *testing.T) {
	hold := state.Hold{PID: 10, StartTicks: 100}
	if holdClaims(hold, Agent{PID: 10, StartTicks: 200}) {
		t.Error("a hold claimed a process with a different start time")
	}
	if !holdClaims(hold, Agent{PID: 10, StartTicks: 100}) {
		t.Error("a hold failed to claim its own process")
	}
	if holdClaims(hold, Agent{PID: 11, StartTicks: 100}) {
		t.Error("a hold claimed a different pid")
	}
}

func TestTrainCheckForFedoraFamilies(t *testing.T) {
	previous := runningCommands
	t.Cleanup(func() { runningCommands = previous })

	runningCommands = func() ([]string, error) { return []string{"bash", "rpm-ostree"}, nil }
	if running, err := defaultTrainRunning(context.Background(), adapter.FedoraAtomic); err != nil || !running {
		t.Errorf("rpm-ostree running: got (%v, %v), want (true, nil)", running, err)
	}
	if running, err := defaultTrainRunning(context.Background(), adapter.Bootc); err != nil || running {
		t.Errorf("bootc with no bootc process: got (%v, %v), want (false, nil)", running, err)
	}

	runningCommands = func() ([]string, error) { return []string{"bootc"}, nil }
	if running, err := defaultTrainRunning(context.Background(), adapter.Bootc); err != nil || !running {
		t.Errorf("bootc running: got (%v, %v), want (true, nil)", running, err)
	}

	runningCommands = func() ([]string, error) { return nil, errors.New("no /proc") }
	if _, err := defaultTrainRunning(context.Background(), adapter.FedoraAtomic); err == nil {
		t.Error("an unreadable /proc was treated as an idle train")
	}
}

func TestTrainCheckOfAnUnknownFamilyFailsClosed(t *testing.T) {
	if running, err := defaultTrainRunning(context.Background(), adapter.Unknown); err == nil {
		t.Errorf("unknown family: got (%v, nil), want an error", running)
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
