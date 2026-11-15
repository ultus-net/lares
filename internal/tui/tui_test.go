package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/reconcile"
	"github.com/ultus-net/lares/internal/state"
)

var base = time.Date(2026, 11, 15, 12, 0, 0, 0, time.Local)

// newModel builds a model over a temp store and a scripted engine.
func newModel(t *testing.T, records ...[]contract.Record) Model {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	cfg := config.Default()
	cfg.StateDir = store.Dir()
	cfg.ConfigDir = store.Dir()
	// The punch list is not the reboot gate, so a pass here never queues one.
	cfg.RebootFromPass = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	index := 0
	engine, err := reconcile.New(reconcile.Options{
		Config:  cfg,
		Store:   store,
		Family:  adapter.Arch,
		RepoDir: store.Dir(),
		Now:     func() time.Time { return base },
		Measure: func(context.Context) ([]contract.Record, error) {
			if len(records) == 0 {
				return []contract.Record{{ID: contract.SummaryID, State: contract.Live}}, nil
			}
			i := index
			if i >= len(records) {
				i = len(records) - 1
			}
			index++
			return records[i], nil
		},
		Run:      func(context.Context, string, []string) (string, error) { return "", nil },
		Sleep:    func(context.Context, time.Duration) error { return nil },
		Session:  func(context.Context) (bool, error) { return false, nil },
		Train:    func(context.Context) (bool, error) { return false, nil },
		Agents:   func() ([]reconcile.Agent, error) { return nil, nil },
		Activate: func(context.Context, []string) error { return nil },
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	model, err := New(Options{Engine: engine, Store: store, Now: func() time.Time { return base }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return model
}

func TestFormatAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h"},
		{30 * time.Hour, "1d"},
		{5 * 24 * time.Hour, "5d"},
		{30 * 24 * time.Hour, "4w"},
		{-time.Hour, "0m"},
	}
	for _, tt := range tests {
		if got := formatAge(tt.d); got != tt.want {
			t.Errorf("formatAge(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestRowsSortBySeverityThenAge(t *testing.T) {
	status := reconcile.Status{
		Queue: state.Queue{Items: []state.Item{
			{ID: "unknown-item", Term: "needs-operator", FirstSeen: base.Add(-48 * time.Hour)},
			{ID: "stale-item", Term: "needs-operator", FirstSeen: base.Add(-72 * time.Hour)},
		}},
		Verdicts: map[string]state.Verdict{
			"unknown-item": {ID: "unknown-item", State: "unknown", Detail: "cannot measure", At: base},
			"stale-item":   {ID: "stale-item", State: "stale", Detail: "obsolete", At: base},
			"live-item":    {ID: "live-item", State: "not-live", Detail: "broken", At: base.Add(-time.Hour)},
			"older-live":   {ID: "older-live", State: "not-live", Detail: "broken longer", At: base.Add(-24 * time.Hour)},
			"quiet":        {ID: "quiet", State: "live", Detail: "fine", At: base},
		},
	}
	rows := rowsFrom(status, base)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (live verdicts are not rows): %+v", len(rows), rows)
	}
	want := []string{"older-live", "live-item", "stale-item", "unknown-item"}
	for i, id := range want {
		if rows[i].ID != id {
			t.Errorf("row %d is %q, want %q", i, rows[i].ID, id)
		}
	}
}

func TestRowsShowQueueTermWithoutVerdict(t *testing.T) {
	status := reconcile.Status{Queue: state.Queue{Items: []state.Item{
		{ID: "mystery", Term: "needs-operator", FirstSeen: base.Add(-2 * time.Hour)},
	}}}
	rows := rowsFrom(status, base)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Detail != "needs-operator" {
		t.Errorf("detail %q, want the queue term", rows[0].Detail)
	}
	if rows[0].State != contract.Unknown {
		t.Errorf("state %q, want unknown", rows[0].State)
	}
}

func TestSelectionClamps(t *testing.T) {
	m := newModel(t, []contract.Record{
		{ID: "a", State: contract.NotLive, Detail: "a"},
		{ID: "b", State: contract.NotLive, Detail: "b"},
		{ID: contract.SummaryID, State: contract.NotLive},
	})
	if _, err := m.engine.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err != nil {
		t.Fatal(err)
	}
	if len(m.rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(m.rows))
	}

	updated, _ := m.Update(key('j'))
	m = updated.(Model)
	updated, _ = m.Update(key('j'))
	m = updated.(Model)
	if m.selected != len(m.rows)-1 {
		t.Errorf("selected %d after two downs, want %d", m.selected, len(m.rows)-1)
	}
	updated, _ = m.Update(key('k'))
	m = updated.(Model)
	updated, _ = m.Update(key('k'))
	m = updated.(Model)
	if m.selected != 0 {
		t.Errorf("selected %d after two ups, want 0", m.selected)
	}
}

func TestDeferMovesNextAttempt(t *testing.T) {
	m := newModel(t, []contract.Record{
		{ID: "offsite", State: contract.NotLive, Fix: "install/offsite-oauth-wizard.sh"},
		{ID: contract.SummaryID, State: contract.NotLive},
	})
	if _, err := m.engine.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err != nil {
		t.Fatal(err)
	}

	updated, _ := m.Update(key('d'))
	m = updated.(Model)

	queue, _, err := m.store.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(queue.Items) != 1 {
		t.Fatalf("queue %+v, want one item", queue.Items)
	}
	want := base.Add(time.Hour)
	if !queue.Items[0].NextAttemptAt.Equal(want) {
		t.Errorf("next attempt %v, want %v", queue.Items[0].NextAttemptAt, want)
	}
	if queue.Items[0].Term != "deferred" {
		t.Errorf("term %q, want deferred", queue.Items[0].Term)
	}
}

func TestDeferWritesAVerdict(t *testing.T) {
	m := newModel(t, []contract.Record{
		{ID: "offsite", State: contract.NotLive, Detail: "no repo"},
		{ID: contract.SummaryID, State: contract.NotLive},
	})
	if _, err := m.engine.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(key('d'))
	m = updated.(Model)

	deferred, err := m.store.Deferrals()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := deferred["offsite"]; !ok {
		t.Fatal("the deferral was not recorded in the verdict log")
	}
}

func TestActionForNamesOnlyImplementedKeys(t *testing.T) {
	if got := actionFor(reconcile.RebootID); got != "[r]eboot" {
		t.Errorf("reboot action %q", got)
	}
	for _, id := range []string{"offsite", "branch-x", "patch-train", "backup"} {
		if got := actionFor(id); got != "[d]efer" {
			t.Errorf("action for %q is %q, want [d]efer", id, got)
		}
	}
}

func TestUnknownVerdictIsARow(t *testing.T) {
	status := reconcile.Status{Verdicts: map[string]state.Verdict{
		"backup": {ID: "backup", State: "unknown", Detail: "cannot measure", At: base},
	}}
	rows := rowsFrom(status, base)
	if len(rows) != 1 || rows[0].ID != "backup" {
		t.Fatalf("rows %+v, want the unknown verdict shown", rows)
	}
	if rows[0].State != contract.Unknown {
		t.Errorf("state %q, want unknown", rows[0].State)
	}
}

func TestDetailToggle(t *testing.T) {
	m := newModel(t)
	if m.detail {
		t.Fatal("detail pane starts open")
	}
	updated, _ := m.Update(key('v'))
	m = updated.(Model)
	if !m.detail {
		t.Fatal("v did not open the detail pane")
	}
	updated, _ = m.Update(key('v'))
	m = updated.(Model)
	if m.detail {
		t.Fatal("v did not close the detail pane")
	}
}

func TestViewRendersRowsAndHolds(t *testing.T) {
	m := newModel(t, []contract.Record{
		{ID: "offsite", State: contract.NotLive, Detail: "BACKUP_OFFSITE_REPO is empty"},
		{ID: contract.SummaryID, State: contract.NotLive},
	})
	if _, err := m.engine.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	hold := state.Hold{Owner: "pid-1", Reason: "agent run", PID: os.Getpid(), Start: base.Add(-30 * time.Minute)}
	if err := m.store.AddHold(hold); err != nil {
		t.Fatal(err)
	}
	if err := m.reload(); err != nil {
		t.Fatal(err)
	}

	view := m.View()
	for _, want := range []string{"lares", "offsite", "BACKUP_OFFSITE_REPO is empty", "hold pid-1: agent run"} {
		if !strings.Contains(view, want) {
			t.Errorf("view does not contain %q:\n%s", want, view)
		}
	}
}

func TestErrorSurfacesInStatusLine(t *testing.T) {
	m := newModel(t)
	updated, _ := m.Update(passDoneMsg{err: errors.New("probe exploded")})
	m = updated.(Model)
	if !strings.Contains(m.status, "probe exploded") {
		t.Fatalf("status %q does not carry the error", m.status)
	}
	if _, cmd := m.Update(key('j')); cmd != nil {
		t.Error("model stopped accepting keys after an error")
	}
}

func TestQuitKey(t *testing.T) {
	m := newModel(t)
	_, cmd := m.Update(key('q'))
	if cmd == nil {
		t.Fatal("q returned no command")
	}
	if msg := cmd(); msg == nil {
		t.Error("q command produced no message")
	}
}

func TestNewRequiresEngineAndStore(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted empty options")
	}
}

// key builds a rune key message.
func key(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}
