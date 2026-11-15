package reconcile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/state"
)

// RebootID is the queue item that carries a blocked reboot.
const RebootID = "reboot"

// Reboot terms, in the order the gate evaluates them.
const (
	TermWindow  = "window"
	TermHold    = "hold"
	TermSession = "session"
	TermOwnJob  = "own-job"
)

// RebootVerdict says whether a reboot may run now, and which term blocked it.
type RebootVerdict struct {
	Allowed bool
	Term    string
	HoldAge time.Duration
}

// RebootAllowed evaluates the four terms without changing anything.
func (e *Engine) RebootAllowed(ctx context.Context) (RebootVerdict, error) {
	if !e.cfg.Window.Contains(e.now()) {
		return RebootVerdict{Term: TermWindow}, nil
	}

	holds, err := e.store.Holds()
	if err != nil {
		return RebootVerdict{}, err
	}
	if len(holds) > 0 {
		age := e.now().Sub(holds[0].Start)
		for _, hold := range holds[1:] {
			if holdAge := e.now().Sub(hold.Start); holdAge > age {
				age = holdAge
			}
		}
		return RebootVerdict{Term: TermHold, HoldAge: age}, nil
	}

	active, err := e.session(ctx)
	if err != nil {
		return RebootVerdict{Term: TermSession}, nil
	}
	if active {
		return RebootVerdict{Term: TermSession}, nil
	}

	if e.busy.Load() {
		return RebootVerdict{Term: TermOwnJob}, nil
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
// blocked it. It returns the verdict it acted on.
func (e *Engine) Reboot(ctx context.Context) (RebootVerdict, error) {
	verdict, err := e.RebootAllowed(ctx)
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
	for _, existing := range queue.Items {
		if existing.ID == RebootID {
			item = existing
			item.Term = v.Term
			break
		}
	}
	replaced := false
	for i, existing := range queue.Items {
		if existing.ID != RebootID {
			continue
		}
		queue.Items[i] = item
		replaced = true
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

// defaultSessionActive reports whether a person is still using the machine.
//
// A locked screen counts: the session is still there to lose work. Omarchy's
// stay-awake marker counts, and so does an inhibitor a build left behind. The
// check fails closed: if loginctl cannot answer, the host is treated as in use.
func defaultSessionActive(ctx context.Context) (bool, error) {
	if home, err := os.UserHomeDir(); err == nil {
		marker := filepath.Join(home, ".local", "state", "omarchy", "indicators", "stay-awake")
		if _, err := os.Stat(marker); err == nil {
			return true, nil
		}
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

// defaultTrainRunning asks Omarchy whether an update train holds its lock.
func defaultTrainRunning(ctx context.Context, family adapter.Family) (bool, error) {
	if family != adapter.Arch {
		return false, nil
	}
	err := exec.CommandContext(ctx, "omarchy-update-lock", "held").Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, nil
}
