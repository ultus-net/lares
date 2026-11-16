// Package tui renders the punch list: every finding, oldest and worst first,
// with the one key that acts on it.
//
// The model reads state through the engine and the store. It writes no file
// directly, so the queue and the hold rules stay in one place.
package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/reconcile"
	"github.com/ultus-net/lares/internal/state"
	"github.com/ultus-net/lares/internal/term"
)

// Options wires the punch list to the engine and the store.
type Options struct {
	Engine *reconcile.Engine
	Store  *state.Store
	Now    func() time.Time
}

// row is one line of the punch list.
type row struct {
	ID        string
	State     contract.State
	Detail    string
	Fix       string
	Age       time.Duration
	Action    string
	Escalated bool
}

// Model is the Bubble Tea model.
type Model struct {
	engine   *reconcile.Engine
	store    *state.Store
	now      func() time.Time
	hostname string

	rows     []row
	holds    []state.Hold
	selected int
	detail   bool
	status   string
	width    int
	lastAt   time.Time
}

// passDoneMsg carries a finished pass back to the model.
type passDoneMsg struct {
	result reconcile.Result
	err    error
}

// rebootDoneMsg carries a finished reboot evaluation back to the model.
type rebootDoneMsg struct {
	verdict reconcile.RebootVerdict
	err     error
}

// New builds the model and loads the current state.
func New(opts Options) (Model, error) {
	if opts.Engine == nil {
		return Model{}, fmt.Errorf("tui: engine is required")
	}
	if opts.Store == nil {
		return Model{}, fmt.Errorf("tui: store is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	m := Model{engine: opts.Engine, store: opts.Store, now: now, hostname: host}
	if err := m.reload(); err != nil {
		return Model{}, err
	}
	return m, nil
}

// reload rebuilds the rows and holds from the engine.
func (m *Model) reload() error {
	status, err := m.engine.Status()
	if err != nil {
		return fmt.Errorf("load status: %w", err)
	}
	m.rows = rowsFrom(status, m.now())
	m.holds = status.Holds
	if len(m.rows) == 0 {
		m.selected = 0
	} else if m.selected >= len(m.rows) {
		m.selected = len(m.rows) - 1
	}
	for _, verdict := range status.Verdicts {
		if verdict.At.After(m.lastAt) {
			m.lastAt = verdict.At
		}
	}
	return nil
}

// rowsFrom turns a status into the sorted punch list.
func rowsFrom(status reconcile.Status, now time.Time) []row {
	seen := make(map[string]bool, len(status.Queue.Items))
	rows := make([]row, 0, len(status.Queue.Items)+len(status.Verdicts))

	for _, item := range status.Queue.Items {
		seen[item.ID] = true
		verdict, ok := status.Verdicts[item.ID]
		st := contract.Unknown
		detail := item.Term
		fix := ""
		if ok {
			st = contract.State(verdict.State)
			fix = verdict.Fix
			if verdict.Detail != "" {
				detail = verdict.Detail
			}
		}
		if detail == "" {
			detail = item.Term
		}
		rows = append(rows, row{
			ID:        item.ID,
			State:     st,
			Detail:    detail,
			Fix:       fix,
			Age:       now.Sub(item.FirstSeen),
			Action:    actionFor(item.ID),
			Escalated: item.Term == term.Escalated,
		})
	}

	for _, verdict := range status.Verdicts {
		if seen[verdict.ID] {
			continue
		}
		st := contract.State(verdict.State)
		if !isFinding(st) {
			continue
		}
		rows = append(rows, row{
			ID:     verdict.ID,
			State:  st,
			Detail: verdict.Detail,
			Fix:    verdict.Fix,
			Age:    now.Sub(verdict.At),
			Action: actionFor(verdict.ID),
		})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		// An escalated backup outranks severity: the checkout may be the only
		// copy on the host.
		if rows[i].Escalated != rows[j].Escalated {
			return rows[i].Escalated
		}
		if severity(rows[i].State) != severity(rows[j].State) {
			return severity(rows[i].State) < severity(rows[j].State)
		}
		return rows[i].Age > rows[j].Age
	})
	return rows
}

// isFinding reports whether a state needs attention. An unknown measurement is
// shown too: nothing proved the item healthy, so hiding it would lose the
// finding.
func isFinding(st contract.State) bool {
	return st == contract.NotLive || st == contract.Stale || st == contract.Unknown
}

// severity ranks states, worst first.
func severity(st contract.State) int {
	switch st {
	case contract.NotLive:
		return 0
	case contract.Stale:
		return 1
	case contract.Unknown:
		return 2
	default:
		return 3
	}
}

// actionFor names the key that acts on an item.
//
// Only the keys the model implements appear. A repair that needs a person is
// shown with its command in the detail pane, never with a key that does nothing.
func actionFor(id string) string {
	if id == reconcile.RebootID {
		return "[r]eboot"
	}
	return "[d]efer"
}

// formatAge renders a duration as minutes, hours, days, or weeks.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 21*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/(24*7)))
	}
}

// Init starts the program with no background work.
func (m Model) Init() tea.Cmd {
	return nil
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case passDoneMsg:
		if msg.err != nil {
			m.status = "error: " + msg.err.Error()
			return m, nil
		}
		m.status = fmt.Sprintf("pass: %d converged, %d queued, %d escalated, %d waiting",
			len(msg.result.Converged), len(msg.result.Queued), len(msg.result.Escalated), len(msg.result.Skipped))
		if err := m.reload(); err != nil {
			m.status = "error: " + err.Error()
		}
		return m, nil
	case rebootDoneMsg:
		if msg.err != nil {
			m.status = "error: " + msg.err.Error()
			return m, nil
		}
		if msg.verdict.Allowed {
			m.status = "reboot: activated"
		} else {
			m.status = "reboot: blocked on " + msg.verdict.Term
			if len(msg.verdict.Unclaimed) > 0 {
				m.status += " (unclaimed: " + strings.Join(msg.verdict.Unclaimed, ", ") + ")"
			}
		}
		if err := m.reload(); err != nil {
			m.status = "error: " + err.Error()
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey applies one key press.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "j", "down":
		if m.selected < len(m.rows)-1 {
			m.selected++
		}
	case "k", "up":
		if m.selected > 0 {
			m.selected--
		}
	case "v":
		m.detail = !m.detail
	case "d":
		if err := m.deferSelected(); err != nil {
			m.status = "error: " + err.Error()
		} else {
			m.status = "deferred for 1h"
		}
	case "p":
		return m, m.runPass()
	case "r":
		return m, m.runReboot()
	}
	return m, nil
}

// deferSelected records a one-hour deferral through the engine, so the
// operator's judgement lands in the verdict log and survives a lost queue.
func (m *Model) deferSelected() error {
	if len(m.rows) == 0 {
		return nil
	}
	id := m.rows[m.selected].ID
	if _, err := m.engine.Defer(id, time.Hour, "deferred from the punch list"); err != nil {
		return err
	}
	return m.reload()
}

// runPass runs one reconcile pass off the UI goroutine.
func (m Model) runPass() tea.Cmd {
	engine := m.engine
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		result, err := engine.Pass(ctx)
		return passDoneMsg{result: result, err: err}
	}
}

// runReboot evaluates and, when allowed, activates a reboot.
func (m Model) runReboot() tea.Cmd {
	engine := m.engine
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		verdict, err := engine.Reboot(ctx)
		return rebootDoneMsg{verdict: verdict, err: err}
	}
}

// title style for the header and the selected row.
var (
	headerStyle   = lipgloss.NewStyle().Bold(true)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
)

// View renders the punch list.
func (m Model) View() string {
	var b strings.Builder

	last := "never"
	if !m.lastAt.IsZero() {
		last = formatAge(m.now().Sub(m.lastAt)) + " ago"
	}
	fmt.Fprintf(&b, "%s   %s   last measure %s\n", headerStyle.Render("lares"), m.hostname, last)

	for i, r := range m.rows {
		line := fmt.Sprintf("%4s  %-22s %-44s %s", formatAge(r.Age), truncate(r.ID, 22), truncate(r.Detail, 44), r.Action)
		if i == m.selected {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if len(m.rows) == 0 {
		b.WriteString(dimStyle.Render("  nothing queued") + "\n")
	}

	for _, hold := range m.holds {
		fmt.Fprintf(&b, "hold %s: %s (%s)\n", hold.Owner, hold.Reason, formatAge(m.now().Sub(hold.Start)))
	}

	if m.detail && len(m.rows) > 0 {
		r := m.rows[m.selected]
		fmt.Fprintf(&b, "\n%s\n", headerStyle.Render("detail: "+r.ID))
		fmt.Fprintf(&b, "state:  %s\n", r.State)
		fmt.Fprintf(&b, "detail: %s\n", r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(&b, "run:    %s\n", r.Fix)
		}
	}

	if m.status != "" {
		b.WriteString("\n" + m.status + "\n")
	}

	b.WriteString(dimStyle.Render("\n[j/k] move  [p] pass  [r] reboot  [d] defer 1h  [v] detail  [q] quit") + "\n")
	return b.String()
}

// truncate shortens s to n runes with an ellipsis.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

// Run starts the program.
func (m Model) Run() error {
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
