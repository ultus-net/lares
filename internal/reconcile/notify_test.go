package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/term"
)

// needsOperator is a record only a person may repair.
func needsOperator() []contract.Record {
	return []contract.Record{
		rec("offsite", contract.NotLive, "scripts/offsite.sh"),
		summary(contract.NotLive),
	}
}

func TestPassNotifiesOncePerNewFinding(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyAuto}, needsOperator())
	h.session = true

	first, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Notices) != 1 || !strings.Contains(first.Notices[0], "offsite") {
		t.Fatalf("notices %v, want one for offsite", first.Notices)
	}
	if len(h.notified) != 1 {
		t.Fatalf("desktop notices %d, want 1", len(h.notified))
	}
	if h.notified[0].Title != "lares: offsite" {
		t.Errorf("title %q", h.notified[0].Title)
	}
	if !strings.Contains(h.notified[0].Body, "scripts/offsite.sh") {
		t.Errorf("body %q does not name the fix", h.notified[0].Body)
	}
	if h.notified[0].Urgent {
		t.Error("a queued item was marked urgent")
	}

	second, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Notices) != 0 {
		t.Errorf("second pass notices %v, want none for a repeated finding", second.Notices)
	}
	if len(h.notified) != 1 {
		t.Errorf("desktop notices %d after a repeat, want 1", len(h.notified))
	}
}

func TestPassPrintsWithoutASession(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyAuto}, needsOperator())

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Notices) != 1 {
		t.Errorf("notices %v, want the journal line", result.Notices)
	}
	if len(h.notified) != 0 {
		t.Errorf("desktop notices %v, want none without a session", h.notified)
	}
}

func TestPassStdoutModeNeverOpensADesktop(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyStdout}, needsOperator())
	h.session = true

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Notices) != 1 {
		t.Errorf("notices %v, want one", result.Notices)
	}
	if len(h.notified) != 0 {
		t.Errorf("desktop notices %v, want none in stdout mode", h.notified)
	}
}

func TestPassDesktopModeIsSilentOnTheJournal(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyDesktop}, needsOperator())
	h.session = true

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Notices) != 0 {
		t.Errorf("notices %v, want none in desktop mode", result.Notices)
	}
	if len(h.notified) != 1 {
		t.Errorf("desktop notices %v, want one", h.notified)
	}
}

func TestPassSurfacesNothingWhenNotifyIsOff(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyOff}, needsOperator())
	h.session = true

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Notices) != 0 || len(h.notified) != 0 {
		t.Errorf("off mode surfaced notices %v and %v", result.Notices, h.notified)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "notified.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("off mode wrote notified.json: %v", err)
	}
}

func TestPassNotifiesAgainWhenTheDetailChanges(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyStdout},
		[]contract.Record{{ID: "offsite", State: contract.NotLive, Detail: "a"}, summary(contract.NotLive)},
		[]contract.Record{{ID: "offsite", State: contract.NotLive, Detail: "b"}, summary(contract.NotLive)},
	)
	h.session = true

	first, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(first.Notices) != 1 || len(second.Notices) != 1 {
		t.Fatalf("notices %v then %v, want one each", first.Notices, second.Notices)
	}
	if first.Notices[0] == second.Notices[0] {
		t.Errorf("both passes printed %q, want the changed detail", first.Notices[0])
	}
}

func TestPassSurfacesAFindingAgainAfterItClears(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyStdout},
		needsOperator(),
		[]contract.Record{rec("offsite", contract.Live, ""), summary(contract.Live)},
		needsOperator(),
	)

	counts := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		result, err := h.engine.Pass(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
		counts = append(counts, len(result.Notices))
	}
	if counts[0] != 1 || counts[1] != 0 || counts[2] != 1 {
		t.Errorf("notices per pass %v, want 1, 0, 1", counts)
	}
}

func TestPassMarksADidNotHoldFindingUrgent(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyDesktop},
		[]contract.Record{rec("repo-demo", contract.NotLive, "git fetch origin"), summary(contract.NotLive)},
	)
	h.session = true

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(result.Escalated) != 1 || result.Escalated[0] != "repo-demo" {
		t.Fatalf("escalated %v, want repo-demo: the repair did not hold", result.Escalated)
	}
	if len(h.notified) != 1 {
		t.Fatalf("desktop notices %d, want 1", len(h.notified))
	}
	if !h.notified[0].Urgent {
		t.Error("a repair that did not hold was not marked urgent")
	}

	again, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(again.Notices) != 0 || len(h.notified) != 1 {
		t.Errorf("a repeated escalation was surfaced again: %v %v", again.Notices, h.notified)
	}
}

func TestPassNotifiesABlockedRebootOnce(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyAuto, reboot: true}, needsOperator())
	h.session = true

	first, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Reboot.Allowed {
		t.Fatalf("the reboot was allowed with a live session")
	}
	found := false
	for _, notice := range first.Notices {
		if strings.Contains(notice, "reboot blocked: "+TermSession) {
			found = true
		}
	}
	if !found {
		t.Fatalf("notices %v do not name the blocked reboot", first.Notices)
	}
	urgent := false
	for _, sent := range h.notified {
		if sent.Title == "lares: reboot blocked" && sent.Urgent {
			urgent = true
		}
	}
	if !urgent {
		t.Errorf("desktop notices %v do not mark the blocked reboot urgent", h.notified)
	}

	before := len(h.notified)
	second, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	for _, notice := range second.Notices {
		if strings.Contains(notice, "reboot blocked") {
			t.Errorf("second pass repeated the blocked reboot: %q", notice)
		}
	}
	if len(h.notified) != before {
		t.Errorf("desktop notices %d after a repeat, want %d", len(h.notified), before)
	}
}

func TestPassReportsADesktopFailureWithoutFailing(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyAuto, notifyErr: errors.New("no daemon")}, needsOperator())
	h.session = true

	result, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("a desktop failure failed the pass: %v", err)
	}
	if len(result.NotifyFailed) != 1 || !strings.Contains(result.NotifyFailed[0], "no daemon") {
		t.Errorf("failures %v, want the desktop error", result.NotifyFailed)
	}
	if len(result.Notices) != 1 {
		t.Errorf("notices %v, want the journal line despite the failure", result.Notices)
	}
}

func TestPassDoesNotRepeatAfterADesktopFailureWhenTheLineWasPrinted(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyAuto, notifyErr: errors.New("no daemon")}, needsOperator())
	h.session = true

	first, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(first.Notices) != 1 || len(first.NotifyFailed) != 1 {
		t.Fatalf("first pass notices %v failures %v, want one of each", first.Notices, first.NotifyFailed)
	}

	second, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.Notices) != 0 {
		t.Errorf("second pass notices %v, want none: the journal already held the line", second.Notices)
	}
	if len(second.NotifyFailed) != 0 {
		t.Errorf("second pass failures %v, want none: the finding was recorded", second.NotifyFailed)
	}
	if len(h.notified) != 1 {
		t.Errorf("desktop attempts %d, want the one failed attempt", len(h.notified))
	}
}

func TestPassDesktopModeRetriesAFailedSend(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{notify: config.NotifyDesktop, notifyErr: errors.New("no daemon")}, needsOperator())
	h.session = true

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := h.engine.Pass(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.NotifyFailed) != 1 {
		t.Errorf("second pass failures %v, want a retry: nothing reached the person", second.NotifyFailed)
	}
	if len(h.notified) != 2 {
		t.Errorf("desktop attempts %d, want a retry", len(h.notified))
	}
}

func TestNoticeStaysOnOneLine(t *testing.T) {
	got := noticeFor(contract.Record{ID: "offsite", State: contract.NotLive, Detail: "first\nsecond"}, term.NeedsOperator)
	if strings.Contains(got.Line, "\n") {
		t.Errorf("notice line %q holds a newline", got.Line)
	}
	if !strings.Contains(got.Line, "first second") {
		t.Errorf("notice line %q lost the detail", got.Line)
	}
}

func TestDefaultNotifyRunsNotifySend(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "args")
	fakeCommand(t, bin, "notify-send", `printf '%s\n' "$@" >> `+log)
	t.Setenv("PATH", bin)

	if err := defaultNotify(context.Background(), "lares: offsite", "not-live", false); err != nil {
		t.Fatalf("defaultNotify: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"--app-name=lares", "--urgency=normal", "lares: offsite", "not-live"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args %v, want %v", args, want)
	}

	if err := defaultNotify(context.Background(), "lares: reboot blocked", "hold", true); err != nil {
		t.Fatalf("defaultNotify: %v", err)
	}
	data, err = os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--urgency=critical") {
		t.Errorf("urgent call did not pass --urgency=critical: %q", data)
	}
}

func TestDefaultNotifyFailsLoudly(t *testing.T) {
	bin := t.TempDir()
	fakeCommand(t, bin, "notify-send", "echo 'no display' >&2\nexit 1")
	t.Setenv("PATH", bin)

	err := defaultNotify(context.Background(), "lares: offsite", "not-live", false)
	if err == nil {
		t.Fatal("a failed notify-send returned no error")
	}
	if !strings.Contains(err.Error(), "notify-send") {
		t.Errorf("error %q does not name the command", err)
	}
}
