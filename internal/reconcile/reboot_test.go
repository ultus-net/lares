package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
	previousCommands, previousTxn := runningCommands, rpmTransaction
	t.Cleanup(func() { runningCommands, rpmTransaction = previousCommands, previousTxn })
	// An idle daemon is the baseline for the process-only cases.
	rpmTransaction = func(context.Context) (bool, error) { return false, nil }

	runningCommands = func() ([]string, error) { return []string{"bash", "rpm-ostree"}, nil }
	if running, err := defaultTrainRunning(context.Background(), adapter.FedoraAtomic); err != nil || !running {
		t.Errorf("rpm-ostree running: got (%v, %v), want (true, nil)", running, err)
	}
	// A bootc host may still update through rpm-ostree, so that process must
	// hold the train there too.
	if running, err := defaultTrainRunning(context.Background(), adapter.Bootc); err != nil || !running {
		t.Errorf("bootc host with rpm-ostree running: got (%v, %v), want (true, nil)", running, err)
	}

	runningCommands = func() ([]string, error) { return []string{"bootc"}, nil }
	if running, err := defaultTrainRunning(context.Background(), adapter.Bootc); err != nil || !running {
		t.Errorf("bootc running: got (%v, %v), want (true, nil)", running, err)
	}
	if running, err := defaultTrainRunning(context.Background(), adapter.FedoraAtomic); err != nil || running {
		t.Errorf("fedora-atomic with only bootc running: got (%v, %v), want (false, nil)", running, err)
	}

	runningCommands = func() ([]string, error) { return []string{"bash", "sleep"}, nil }
	for _, family := range []adapter.Family{adapter.FedoraAtomic, adapter.Bootc} {
		if running, err := defaultTrainRunning(context.Background(), family); err != nil || running {
			t.Errorf("%s with no transaction: got (%v, %v), want (false, nil)", family, running, err)
		}
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

// TestTrainCheckSeesAHeldRpmOstreeTransaction proves the daemon's own process
// is not required: rpm-ostreed exits when idle and still reports a held
// transaction through its status document.
func TestTrainCheckSeesAHeldRpmOstreeTransaction(t *testing.T) {
	previousCommands, previousTxn := runningCommands, rpmTransaction
	t.Cleanup(func() { runningCommands, rpmTransaction = previousCommands, previousTxn })
	runningCommands = func() ([]string, error) { return []string{"bash", "sleep"}, nil }
	rpmTransaction = func(context.Context) (bool, error) { return true, nil }

	for _, family := range []adapter.Family{adapter.FedoraAtomic, adapter.Bootc} {
		if running, err := defaultTrainRunning(context.Background(), family); err != nil || !running {
			t.Errorf("%s with a held transaction: got (%v, %v), want (true, nil)", family, running, err)
		}
	}
}

// TestTrainCheckFailsClosedWhenTheTransactionCannotBeRead proves a failed status
// query blocks instead of reporting an idle train.
func TestTrainCheckFailsClosedWhenTheTransactionCannotBeRead(t *testing.T) {
	previousCommands, previousTxn := runningCommands, rpmTransaction
	t.Cleanup(func() { runningCommands, rpmTransaction = previousCommands, previousTxn })
	runningCommands = func() ([]string, error) { return nil, nil }
	rpmTransaction = func(context.Context) (bool, error) { return false, errors.New("no bus") }

	for _, family := range []adapter.Family{adapter.FedoraAtomic, adapter.Bootc} {
		if _, err := defaultTrainRunning(context.Background(), family); err == nil {
			t.Errorf("%s: a failed status query was treated as an idle train", family)
		}
	}
}

// TestTrainCheckAllowsABootcHostWithoutRpmOstree proves an absent rpm-ostree is
// not a blocker on bootc, where it is optional, and stays one on Fedora atomic,
// where the family is defined by it.
func TestTrainCheckAllowsABootcHostWithoutRpmOstree(t *testing.T) {
	previousCommands, previousTxn := runningCommands, rpmTransaction
	t.Cleanup(func() { runningCommands, rpmTransaction = previousCommands, previousTxn })
	runningCommands = func() ([]string, error) { return nil, nil }
	rpmTransaction = func(context.Context) (bool, error) {
		return false, fmt.Errorf("rpm-ostree status: %w", exec.ErrNotFound)
	}

	if running, err := defaultTrainRunning(context.Background(), adapter.Bootc); err != nil || running {
		t.Errorf("bootc without rpm-ostree: got (%v, %v), want (false, nil)", running, err)
	}
	if _, err := defaultTrainRunning(context.Background(), adapter.FedoraAtomic); err == nil {
		t.Error("fedora atomic without rpm-ostree was treated as an idle train")
	}
}

// TestAgentScanFailsClosedOnAnUnreadableProcess proves an unreadable command
// name blocks both scans instead of reporting a clear host.
func TestAgentScanFailsClosedOnAnUnreadableProcess(t *testing.T) {
	previous := procComm
	t.Cleanup(func() { procComm = previous })
	procComm = func(string) ([]byte, error) { return nil, fs.ErrPermission }

	if _, err := defaultAgents(); err == nil {
		t.Error("defaultAgents read an unreadable command name as clear")
	}
	if _, err := runningCommands(); err == nil {
		t.Error("runningCommands read an unreadable command name as idle")
	}
}

// TestAgentScanFailsClosedOnAnUnreadableStartTime proves a process whose start
// time cannot be read blocks the scan: without it, a hold for a reused pid can
// claim the process.
func TestAgentScanFailsClosedOnAnUnreadableStartTime(t *testing.T) {
	previousComm, previousTicks := procComm, procStartTicks
	t.Cleanup(func() { procComm, procStartTicks = previousComm, previousTicks })
	procComm = func(string) ([]byte, error) { return []byte("opencode\n"), nil }
	procStartTicks = func(int) (uint64, bool) { return 0, false }

	if _, err := defaultAgents(); err == nil {
		t.Error("defaultAgents read an unreadable start time as a clear host")
	}
}

// TestRPMTransactionReadsTheStatusDocument drives the real command path with a
// fake rpm-ostree, so the exec, the decode, and the transaction check are
// observed together.
func TestRPMTransactionReadsTheStatusDocument(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		held    bool
		wantErr bool
	}{
		{"held", `{"transaction":{"kind":"upgrade"}}`, true, false},
		{"idle", `{"deployments":[],"transaction":null}`, false, false},
		{"malformed", `{`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "rpm-ostree")
			body := "#!/bin/sh\nprintf '%s' '" + tt.body + "'\n"
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)

			held, err := rpmTransaction(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("error %v, want error %v", err, tt.wantErr)
			}
			if err == nil && held != tt.held {
				t.Errorf("held %v, want %v", held, tt.held)
			}
		})
	}
}

// TestRPMTransactionCanceledContextFailsClosed proves a pass that lost its
// deadline does not read the train as idle. Start returns the context error
// before it forks, so no rpm-ostree runs.
func TestRPMTransactionCanceledContextFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := rpmTransaction(ctx); err == nil {
		t.Error("a canceled context was treated as an idle train")
	}
}

// TestProcessScansSkipAProcessThatExited proves the one benign read error stays
// benign: a process that disappears between the directory read and the file
// read is not a failure.
func TestProcessScansSkipAProcessThatExited(t *testing.T) {
	previous := procComm
	t.Cleanup(func() { procComm = previous })
	procComm = func(string) ([]byte, error) { return nil, fs.ErrNotExist }

	if agents, err := defaultAgents(); err != nil || len(agents) != 0 {
		t.Errorf("defaultAgents: got (%v, %v), want no agents and no error", agents, err)
	}
	if names, err := runningCommands(); err != nil || len(names) != 0 {
		t.Errorf("runningCommands: got (%v, %v), want no names and no error", names, err)
	}
}

// TestSessionFailsClosedWhenTheMarkerCannotBeRead proves a marker that exists
// but cannot be statted keeps the host in use. The indicators path is a regular
// file, so the marker stat fails with ENOTDIR without depending on permissions
// and the test also runs as root.
func TestSessionFailsClosedWhenTheMarkerCannotBeRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	indicators := filepath.Join(home, ".local", "state", "omarchy", "indicators")
	if err := os.MkdirAll(filepath.Dir(indicators), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indicators, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	active, err := defaultSessionActive(context.Background())
	if !active || err == nil {
		t.Errorf("active %v, error %v, want the host in use and an error", active, err)
	}
}

// TestSessionFailsClosedWithoutAHomeDirectory proves lares cannot claim the host
// is idle when it cannot locate the stay-awake marker.
func TestSessionFailsClosedWithoutAHomeDirectory(t *testing.T) {
	t.Setenv("HOME", "")

	active, err := defaultSessionActive(context.Background())
	if !active || err == nil {
		t.Errorf("active %v, error %v, want the host in use and an error", active, err)
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
