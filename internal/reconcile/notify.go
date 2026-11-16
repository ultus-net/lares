package reconcile

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/state"
	"github.com/ultus-net/lares/internal/term"
)

// notice is one finding lares has not surfaced yet.
//
// Key is the dedupe key: the item id, its state, and its detail. A repeated
// identical failure keeps the same key, so it is surfaced once. A changed
// detail makes a new key, so the change is surfaced.
type notice struct {
	Key    string
	Title  string
	Body   string
	Line   string
	Urgent bool
}

// noticeFor turns a queued record into a notice.
func noticeFor(rec contract.Record, itemTerm string) notice {
	body := rec.Detail
	if body == "" {
		body = string(rec.State)
	}
	if rec.Fix != "" {
		body += "; fix: " + rec.Fix
	}
	return notice{
		Key:    rec.ID + "/" + string(rec.State) + "/" + rec.Detail,
		Title:  "lares: " + rec.ID,
		Body:   body,
		Line:   fmt.Sprintf("lares: %s %s: %s", rec.ID, rec.State, oneLine(body)),
		Urgent: itemTerm == term.DidNotHold,
	}
}

// oneLine keeps a journal notice on one line. A probe detail may carry a
// newline, which would split one finding across several journal lines.
func oneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ").Replace(s))
}

// rebootNotice turns a blocked reboot into a notice.
//
// The dedupe key names the term, so a reboot blocked by the same term is
// surfaced once. The unclaimed processes are part of the body, not the key: the
// term is the finding and the names are its evidence.
func rebootNotice(v RebootVerdict) notice {
	body := v.Term
	if len(v.Unclaimed) > 0 {
		body += "; unclaimed: " + strings.Join(v.Unclaimed, ", ")
	}
	return notice{
		Key:    RebootID + "/blocked/" + v.Term,
		Title:  "lares: reboot blocked",
		Body:   body,
		Line:   "lares: reboot blocked: " + body,
		Urgent: true,
	}
}

// notifyFindings surfaces each new finding once and records what it surfaced.
//
// It returns the lines to print and the desktop failures to report. A desktop
// failure never fails a pass: the finding was still recorded, and a missing
// notification daemon must not stop convergence. The notified set is replaced
// with the findings this pass surfaced, so a finding that clears and returns is
// surfaced again.
func (e *Engine) notifyFindings(ctx context.Context, records []contract.Record, items []state.Item, reboot RebootVerdict) ([]string, []string, error) {
	if e.cfg.Notify == config.NotifyOff {
		return nil, nil, nil
	}

	byID := make(map[string]contract.Record, len(records))
	for _, rec := range records {
		if !rec.IsSummary() {
			byID[rec.ID] = rec
		}
	}
	var findings []notice
	for _, item := range items {
		if item.ID == RebootID {
			// The reboot item is described by the verdict below, which carries
			// the unclaimed names.
			continue
		}
		rec, ok := byID[item.ID]
		if !ok {
			continue
		}
		findings = append(findings, noticeFor(rec, item.Term))
	}
	if !reboot.Allowed && reboot.Term != "" {
		findings = append(findings, rebootNotice(reboot))
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Key < findings[j].Key })

	previous, err := e.store.Notified()
	if err != nil {
		return nil, nil, err
	}

	wantDesktop := e.cfg.Notify == config.NotifyAuto || e.cfg.Notify == config.NotifyDesktop
	desktop := false
	var failed []string
	if wantDesktop {
		active, err := e.session(ctx)
		if err != nil {
			failed = append(failed, fmt.Sprintf("session: %v", err))
		}
		desktop = active
	}

	surfaced := make(map[string]bool, len(findings))
	var lines []string
	for _, f := range findings {
		if previous[f.Key] {
			surfaced[f.Key] = true
			continue
		}
		delivered := false
		if e.cfg.Notify == config.NotifyAuto || e.cfg.Notify == config.NotifyStdout {
			lines = append(lines, f.Line)
			delivered = true
		}
		if wantDesktop && desktop {
			if err := e.notify(ctx, f.Title, f.Body, f.Urgent); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", f.Key, err))
			} else {
				delivered = true
			}
		}
		// A key is recorded once anything reached the person, so a failed
		// desktop send never re-prints a line the journal already holds. In
		// desktop mode nothing reached them, so the key stays out and the
		// next pass retries the send.
		if delivered {
			surfaced[f.Key] = true
		}
	}
	if err := e.store.SaveNotified(surfaced); err != nil {
		return nil, failed, err
	}
	return lines, failed, nil
}

// defaultNotify sends one desktop notification with notify-send.
//
// A missing notify-send is an error the caller reports. It is not a pass
// failure: the same finding is still printed for the journal.
func defaultNotify(ctx context.Context, title, body string, urgent bool) error {
	urgency := "--urgency=normal"
	if urgent {
		urgency = "--urgency=critical"
	}
	argv := []string{"notify-send", "--app-name=lares", urgency, title, body}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("notify-send: %w: %s", err, msg)
		}
		return fmt.Errorf("notify-send: %w", err)
	}
	return nil
}
