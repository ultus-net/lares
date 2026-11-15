package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/state"
	"github.com/ultus-net/lares/internal/term"
)

// RebootID is the queue item that carries a blocked reboot.
const RebootID = "reboot"

// Reboot terms, in the order the gate evaluates them. Hold and Scan come from
// the shared term vocabulary so the measure engine and the gate never drift.
const (
	TermWindow = "window"
	TermHold   = term.Hold
	// TermScan means no process could be checked, so an unclaimed agent cannot
	// be ruled out. It is not a hold the operator wrote.
	TermScan    = term.Scan
	TermSession = "session"
	TermOwnJob  = "own-job"
)

// RebootVerdict says whether a reboot may run now, and which term blocked it.
type RebootVerdict struct {
	Allowed bool
	Term    string
	HoldAge time.Duration
	// Unclaimed names long-horizon processes that are running without a hold,
	// or why the process scan failed.
	Unclaimed []string
}

// RebootAllowed evaluates the gate without changing anything.
//
// It takes the state lock for the read, so a dry run never observes a queue or a
// hold set that a pass is halfway through rewriting.
func (e *Engine) RebootAllowed(ctx context.Context) (RebootVerdict, error) {
	if err := e.store.Lock(); err != nil {
		return RebootVerdict{}, err
	}
	defer e.store.Unlock()
	return e.rebootAllowedLocked(ctx)
}

// rebootAllowedLocked is the gate without the lock. A pass calls it while it
// already holds the lock.
func (e *Engine) rebootAllowedLocked(ctx context.Context) (RebootVerdict, error) {
	if !e.cfg.Window.Contains(e.now()) {
		return RebootVerdict{Term: TermWindow}, nil
	}

	holds, err := e.store.Holds()
	if err != nil {
		return RebootVerdict{}, err
	}
	unclaimed, err := e.unclaimedAgents(holds)
	if err != nil {
		return RebootVerdict{Term: TermScan, Unclaimed: []string{"process scan failed: " + err.Error()}}, nil
	}
	if len(holds) > 0 || len(unclaimed) > 0 {
		var age time.Duration
		for _, hold := range holds {
			if holdAge := e.now().Sub(hold.Start); holdAge > age {
				age = holdAge
			}
		}
		return RebootVerdict{Term: TermHold, HoldAge: age, Unclaimed: unclaimed}, nil
	}

	active, err := e.session(ctx)
	if err != nil {
		return RebootVerdict{Term: TermSession}, nil
	}
	if active {
		return RebootVerdict{Term: TermSession}, nil
	}

	running, err := e.train(ctx)
	if err != nil {
		return RebootVerdict{Term: TermOwnJob}, nil
	}
	if running {
		return RebootVerdict{Term: TermOwnJob}, nil
	}
	return RebootVerdict{Allowed: true}, nil
}

// Reboot either activates the family's reboot verb or queues the term that
// blocked it. The check and the activation share one lock hold, so nothing can
// start a repair in between.
func (e *Engine) Reboot(ctx context.Context) (RebootVerdict, error) {
	if err := e.store.Lock(); err != nil {
		return RebootVerdict{}, err
	}
	defer e.store.Unlock()
	return e.rebootLocked(ctx)
}

// rebootLocked is Reboot without the lock. A pass calls it last, while it still
// holds the lock.
func (e *Engine) rebootLocked(ctx context.Context) (RebootVerdict, error) {
	verdict, err := e.rebootAllowedLocked(ctx)
	if err != nil {
		return verdict, err
	}
	if !verdict.Allowed {
		if err := e.queueBlockedReboot(verdict); err != nil {
			return verdict, err
		}
		return verdict, nil
	}

	entry, err := adapter.Select(e.family, e.cfg.AdapterOverride, e.adapters)
	if err != nil {
		return verdict, err
	}
	if err := e.activate(ctx, entry.Activate); err != nil {
		return verdict, fmt.Errorf("activate reboot: %w", err)
	}
	if err := e.clearBlockedReboot(); err != nil {
		return verdict, err
	}
	return verdict, nil
}

// queueBlockedReboot records the term that blocked the reboot.
func (e *Engine) queueBlockedReboot(v RebootVerdict) error {
	queue, _, err := e.store.Queue()
	if err != nil && !errors.Is(err, state.ErrNoQueue) {
		return err
	}
	now := e.now()
	item := state.Item{ID: RebootID, Term: v.Term, FirstSeen: now}
	replaced := false
	for i, existing := range queue.Items {
		if existing.ID != RebootID {
			continue
		}
		item = existing
		item.Term = v.Term
		queue.Items[i] = item
		replaced = true
		break
	}
	if !replaced {
		queue.Items = append(queue.Items, item)
	}
	_, err = e.store.SaveQueue(queue)
	return err
}

// clearBlockedReboot drops the reboot item once one ran.
func (e *Engine) clearBlockedReboot() error {
	queue, _, err := e.store.Queue()
	if errors.Is(err, state.ErrNoQueue) {
		return nil
	}
	if err != nil {
		return err
	}
	kept := queue.Items[:0]
	for _, item := range queue.Items {
		if item.ID != RebootID {
			kept = append(kept, item)
		}
	}
	queue.Items = kept
	_, err = e.store.SaveQueue(queue)
	return err
}

// longHorizon is the set of process names that must hold the host.
//
// An agent that forgets to write a hold is the failure this policy exists to
// remove, so a live process with no hold blocks the reboot and is reported.
var longHorizon = map[string]bool{
	"opencode": true,
	"orca":     true,
}

// unclaimedAgents names long-horizon processes with no hold that claims them.
//
// A hold claims one process by pid and start time. It never claims a process
// because the hold's reason happens to contain the program name, so two agents of
// the same kind need two holds.
func (e *Engine) unclaimedAgents(holds []state.Hold) ([]string, error) {
	agents, err := e.agents()
	if err != nil {
		return nil, err
	}
	var unclaimed []string
	for _, agent := range agents {
		claimed := false
		for _, hold := range holds {
			if holdClaims(hold, agent) {
				claimed = true
				break
			}
		}
		if !claimed {
			unclaimed = append(unclaimed, fmt.Sprintf("%s[%d]", agent.Name, agent.PID))
		}
	}
	return unclaimed, nil
}

// holdClaims reports whether hold names this exact process instance.
func holdClaims(hold state.Hold, agent Agent) bool {
	if hold.PID <= 0 || hold.PID != agent.PID {
		return false
	}
	if hold.StartTicks == 0 || agent.StartTicks == 0 {
		return true
	}
	return hold.StartTicks == agent.StartTicks
}

// defaultAgents scans /proc for long-horizon processes.
//
// A directory read that fails is an error, because returning no agents would
// claim the host is clear when nothing was checked.
func defaultAgents() ([]Agent, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var found []Agent
	for _, entry := range entries {
		if !entry.IsDir() || !isPID(entry.Name()) {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(data))
		if !longHorizon[name] {
			continue
		}
		ticks, _ := state.ProcessStartTicks(pid)
		found = append(found, Agent{PID: pid, Name: name, StartTicks: ticks})
	}
	return found, nil
}

// isPID reports whether name is all digits.
func isPID(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// defaultSessionActive reports whether a person or a program is still using the
// machine.
//
// A locked screen counts: the session is still there to lose work. Omarchy's
// stay-awake marker counts too, and so does a block inhibitor, which a build or
// a backup takes to keep the host awake. The check fails closed: an unreadable
// inhibitor list or an unanswerable loginctl treats the host as in use.
func defaultSessionActive(ctx context.Context) (bool, error) {
	if home, err := os.UserHomeDir(); err == nil {
		marker := filepath.Join(home, ".local", "state", "omarchy", "indicators", "stay-awake")
		if _, err := os.Stat(marker); err == nil {
			return true, nil
		}
	}

	locks, err := inhibitList(ctx)
	if err != nil {
		return true, err
	}
	blocked, err := blockedByInhibitor(locks)
	if err != nil {
		return true, err
	}
	if blocked {
		return true, nil
	}

	out, err := exec.CommandContext(ctx, "loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return true, fmt.Errorf("list sessions: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		class, err := exec.CommandContext(ctx, "loginctl", "show-session", fields[0], "-p", "Class", "--value").Output()
		if err != nil {
			return true, fmt.Errorf("session class: %w", err)
		}
		if strings.TrimSpace(string(class)) == "manager" {
			continue
		}
		return true, nil
	}
	return false, nil
}

// trainProcesses names the programs that hold an update lock on a family.
//
// Fedora has no lock file the way Omarchy does, so the check looks for the
// transaction program itself. A bootc host may still be updated by rpm-ostree,
// as the Universal Blue images are, so either process holds the train.
var trainProcesses = map[adapter.Family][]string{
	adapter.FedoraAtomic: {"rpm-ostree"},
	adapter.Bootc:        {"bootc", "rpm-ostree"},
}

// runningCommands lists the command names in /proc. Tests replace it.
var runningCommands = func() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || !isPID(entry.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			// The process exited between the directory read and this read.
			continue
		}
		names = append(names, strings.TrimSpace(string(data)))
	}
	return names, nil
}

// defaultTrainRunning reports whether an update transaction holds the host.
//
// Arch asks Omarchy: exit 0 means held and exit 1 means not held, and any other
// result is an error. Fedora families look for the transaction program. An
// unrecognised family has no check at all, which is an error, so the gate blocks
// instead of guessing that the train is idle.
func defaultTrainRunning(ctx context.Context, family adapter.Family) (bool, error) {
	if names, ok := trainProcesses[family]; ok {
		running, err := runningCommands()
		if err != nil {
			return false, err
		}
		for _, name := range running {
			for _, want := range names {
				if name == want {
					return true, nil
				}
			}
		}
		return false, nil
	}
	if family != adapter.Arch {
		return false, fmt.Errorf("no update-train check for family %q", family)
	}
	cmd := exec.CommandContext(ctx, "omarchy-update-lock", "held")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false, fmt.Errorf("omarchy-update-lock: %w", err)
	}
	if exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("omarchy-update-lock exited %d: %s", exit.ExitCode(), strings.TrimSpace(stderr.String()))
}
