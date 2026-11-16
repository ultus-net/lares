package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newStore opens a store in a fresh temp directory.
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// stubAlive replaces processAlive for one test.
func stubAlive(t *testing.T, alive func(pid int) bool) {
	t.Helper()
	previous := processAlive
	processAlive = alive
	t.Cleanup(func() { processAlive = previous })
}

func TestOpenCreatesDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Dir() != dir {
		t.Errorf("Dir %q, want %q", s.Dir(), dir)
	}
	for _, name := range []string{"holds", "runs"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", name)
		}
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("second Open: %v", err)
	}
}

func TestAppendAndReadVerdicts(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"a", "b", "c"} {
		v := Verdict{ID: id, State: "live", Detail: "d", Fix: "f", At: base.Add(time.Duration(i) * time.Minute)}
		if err := s.AppendVerdict(v); err != nil {
			t.Fatalf("AppendVerdict: %v", err)
		}
	}
	got, err := s.Verdicts()
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d verdicts, want 3", len(got))
	}
	if got[0].ID != "a" || got[2].ID != "c" {
		t.Errorf("order lost: %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
	if !got[0].At.Equal(base) {
		t.Errorf("At %v, want %v", got[0].At, base)
	}
	info, err := os.Stat(s.verdictPath())
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("verdict log mode %o, want 600", mode)
	}
}

func TestVerdictsMissingLogIsEmpty(t *testing.T) {
	s := newStore(t)
	got, err := s.Verdicts()
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestVerdictsSkipsTornLine(t *testing.T) {
	s := newStore(t)
	if err := s.AppendVerdict(Verdict{ID: "good", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.verdictPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"torn","sta`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := s.Verdicts()
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("got %+v, want only the good record", got)
	}
}

func TestVerdictsUnreadableLogIsAnError(t *testing.T) {
	s := newStore(t)
	if err := os.Mkdir(s.verdictPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verdicts(); err == nil {
		t.Fatal("Verdicts accepted a log it could not read")
	}
}

func TestQueueMissing(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Queue(); !errors.Is(err, ErrNoQueue) {
		t.Fatalf("got %v, want ErrNoQueue", err)
	}
}

func TestQueueRoundtripAndGeneration(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	q := Queue{Items: []Item{{ID: "offsite", Term: "not-live", FirstSeen: now, Attempts: 2}}}

	gen, err := s.SaveQueue(q)
	if err != nil {
		t.Fatalf("SaveQueue: %v", err)
	}
	if gen != 1 {
		t.Errorf("generation %d, want 1", gen)
	}
	loaded, recovered, err := s.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if recovered {
		t.Error("recovered from backup on a healthy queue")
	}
	if loaded.Generation != gen {
		t.Errorf("stored generation %d, want %d", loaded.Generation, gen)
	}
	if len(loaded.Items) != 1 || loaded.Items[0].ID != "offsite" {
		t.Fatalf("items %+v", loaded.Items)
	}

	if _, err := s.SaveQueue(q); err != nil {
		t.Fatalf("second SaveQueue: %v", err)
	}
	if _, err := s.SaveQueue(q); err != nil {
		t.Fatalf("third SaveQueue: %v", err)
	}
	loaded, _, err = s.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if loaded.Generation != 3 {
		t.Errorf("generation %d, want 3 (monotonic)", loaded.Generation)
	}
}

func TestSaveQueueCreatesBackup(t *testing.T) {
	s := newStore(t)
	if _, err := s.SaveQueue(Queue{Items: []Item{{ID: "first"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.backupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup written before a primary existed: %v", err)
	}
	if _, err := s.SaveQueue(Queue{Items: []Item{{ID: "second"}}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.backupPath())
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var backup Queue
	if err := json.Unmarshal(data, &backup); err != nil {
		t.Fatalf("parse backup: %v", err)
	}
	if len(backup.Items) != 1 || backup.Items[0].ID != "first" {
		t.Errorf("backup holds %+v, want the previous queue", backup.Items)
	}
}

func TestQueueRecoversFromBackup(t *testing.T) {
	s := newStore(t)
	if _, err := s.SaveQueue(Queue{Items: []Item{{ID: "old"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveQueue(Queue{Items: []Item{{ID: "new"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.queuePath(), []byte(`{"generation":`), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, recovered, err := s.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if !recovered {
		t.Error("recovered is false after a corrupt primary")
	}
	if len(loaded.Items) != 1 || loaded.Items[0].ID != "old" {
		t.Fatalf("loaded %+v, want the backup", loaded.Items)
	}
	matches, err := filepath.Glob(s.queuePath() + ".corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("got %d quarantine files, want 1", len(matches))
	}
}

func TestQueuePrimaryMissingBackupUsed(t *testing.T) {
	s := newStore(t)
	if _, err := s.SaveQueue(Queue{Items: []Item{{ID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveQueue(Queue{Items: []Item{{ID: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.queuePath()); err != nil {
		t.Fatal(err)
	}
	loaded, recovered, err := s.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if !recovered {
		t.Error("recovered is false when the primary is missing")
	}
	if len(loaded.Items) != 1 || loaded.Items[0].ID != "a" {
		t.Fatalf("loaded %+v", loaded.Items)
	}
}

func TestQueueBothCorrupt(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(s.queuePath(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.backupPath(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Queue(); err == nil {
		t.Fatal("Queue accepted two corrupt files")
	}
}

func TestQueueRejectsTrailingData(t *testing.T) {
	s := newStore(t)
	body := `{"generation":1,"items":[]}` + "\ntrailing"
	if err := os.WriteFile(s.queuePath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Queue(); err == nil || errors.Is(err, ErrNoQueue) {
		t.Fatalf("Queue accepted trailing data: %v", err)
	}
}

func TestHoldsRoundtrip(t *testing.T) {
	s := newStore(t)
	stubAlive(t, func(pid int) bool { return pid == 4242 })
	hold := Hold{Owner: "opencode-session", Reason: "agent run", PID: 4242, Start: time.Now().UTC().Truncate(time.Second)}
	if err := s.AddHold(hold); err != nil {
		t.Fatalf("AddHold: %v", err)
	}
	holds, err := s.Holds()
	if err != nil {
		t.Fatalf("Holds: %v", err)
	}
	if len(holds) != 1 {
		t.Fatalf("got %d holds, want 1", len(holds))
	}
	if holds[0].Owner != hold.Owner || holds[0].Reason != hold.Reason || holds[0].PID != hold.PID {
		t.Errorf("hold %+v, want %+v", holds[0], hold)
	}
	if holds[0].Stale {
		t.Error("live hold marked stale")
	}
}

func TestAddHoldReplacesSameOwner(t *testing.T) {
	s := newStore(t)
	stubAlive(t, func(int) bool { return true })
	if err := s.AddHold(Hold{Owner: "worker", Reason: "first", PID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{Owner: "worker", Reason: "second", PID: 2}); err != nil {
		t.Fatal(err)
	}
	holds, err := s.Holds()
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0].Reason != "second" || holds[0].PID != 2 {
		t.Fatalf("holds %+v, want the replacement only", holds)
	}
}

func TestAddHoldRejectsNonSlugOwner(t *testing.T) {
	s := newStore(t)
	stubAlive(t, func(int) bool { return true })
	for _, owner := range []string{"user/process", `user\process`, "user!@#process", "user process"} {
		if err := s.AddHold(Hold{Owner: owner, PID: 1}); err == nil {
			t.Errorf("AddHold accepted owner %q", owner)
		}
	}
	if _, err := os.Stat(filepath.Join(s.holdsDir, "user_process.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a rejected owner still produced a file")
	}
}

func TestHoldRejectsUnusableOwner(t *testing.T) {
	s := newStore(t)
	for _, owner := range []string{"", ".", ".."} {
		if err := s.AddHold(Hold{Owner: owner, PID: 1}); err == nil {
			t.Errorf("AddHold accepted owner %q", owner)
		}
	}
}

func TestHoldsRemovesStaleAndMarksStale(t *testing.T) {
	s := newStore(t)
	stubAlive(t, func(pid int) bool { return pid == 1 })
	if err := s.AddHold(Hold{Owner: "live", PID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHold(Hold{Owner: "dead", PID: 999999}); err != nil {
		t.Fatal(err)
	}

	all, err := s.HoldsIncludingStale()
	if err != nil {
		t.Fatalf("HoldsIncludingStale: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d holds, want 2", len(all))
	}
	stale := 0
	for _, h := range all {
		if h.Stale {
			stale++
		}
	}
	if stale != 1 {
		t.Errorf("got %d stale holds, want 1", stale)
	}

	live, err := s.Holds()
	if err != nil {
		t.Fatalf("Holds: %v", err)
	}
	if len(live) != 1 || live[0].Owner != "live" {
		t.Fatalf("live holds %+v, want only the live one", live)
	}
	if _, err := os.Stat(filepath.Join(s.holdsDir, "dead.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale hold file was not removed")
	}
}

func TestHoldsUnparsableFile(t *testing.T) {
	s := newStore(t)
	stubAlive(t, func(int) bool { return true })
	path := filepath.Join(s.holdsDir, "broken.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := s.HoldsIncludingStale()
	if err != nil {
		t.Fatalf("HoldsIncludingStale: %v", err)
	}
	if len(all) != 1 || !all[0].Stale || all[0].Owner != "broken" {
		t.Fatalf("holds %+v, want one stale broken hold", all)
	}

	live, err := s.Holds()
	if err != nil {
		t.Fatalf("Holds: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("got %d live holds, want 0", len(live))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("unparsable hold file was not removed")
	}
}

func TestRemoveHold(t *testing.T) {
	s := newStore(t)
	stubAlive(t, func(int) bool { return true })
	if err := s.AddHold(Hold{Owner: "worker", PID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveHold("worker"); err != nil {
		t.Fatalf("RemoveHold: %v", err)
	}
	holds, err := s.Holds()
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 0 {
		t.Fatalf("got %d holds, want 0", len(holds))
	}
	if err := s.RemoveHold("worker"); err != nil {
		t.Errorf("removing an absent hold failed: %v", err)
	}
}

func TestRunLogNamesAndAppends(t *testing.T) {
	s := newStore(t)
	at := time.Date(2026, 11, 15, 23, 30, 0, 0, time.UTC)
	path, err := s.RunLog("offsite-repair", at, []byte("first\n"))
	if err != nil {
		t.Fatalf("RunLog: %v", err)
	}
	if want := filepath.Join(s.runsDir, "offsite-repair-20261115.log"); path != want {
		t.Errorf("path %q, want %q", path, want)
	}
	if _, err := s.RunLog("offsite-repair", at.Add(time.Minute), []byte("second\n")); err != nil {
		t.Fatalf("second RunLog: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first\nsecond\n" {
		t.Errorf("log body %q", data)
	}
	if _, err := s.RunLog("offsite-repair", at.AddDate(0, 0, 1), []byte("x")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.runsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d run logs, want 2 (one per day)", len(entries))
	}
}

func TestRunLogCap(t *testing.T) {
	s := newStore(t)
	previous := runLogCap
	runLogCap = 16
	t.Cleanup(func() { runLogCap = previous })

	if _, err := s.RunLog("item", time.Now(), []byte(strings.Repeat("x", 16))); err != nil {
		t.Fatalf("first RunLog: %v", err)
	}
	_, err := s.RunLog("item", time.Now(), []byte("y"))
	if err == nil {
		t.Fatal("RunLog accepted a write past the cap")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error %q does not name the cap", err)
	}
}

func TestRunLogRejectsUnusableItem(t *testing.T) {
	s := newStore(t)
	for _, item := range []string{"", ".", "..", "a/b", "a b"} {
		if _, err := s.RunLog(item, time.Now(), []byte("x")); err == nil {
			t.Errorf("RunLog accepted item %q", item)
		}
	}
}

func TestPruneVerdicts(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	old := Verdict{ID: "old", At: now.AddDate(0, 0, -91)}
	fresh := Verdict{ID: "fresh", At: now.AddDate(0, 0, -2)}
	for _, v := range []Verdict{old, fresh} {
		if err := s.AppendVerdict(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(now); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got, err := s.Verdicts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("verdicts %+v, want only fresh", got)
	}
}

func TestPruneRunLogs(t *testing.T) {
	s := newStore(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	old := filepath.Join(s.runsDir, "item-20261001.log")
	fresh := filepath.Join(s.runsDir, "item-20261114.log")
	for _, path := range []string{old, fresh} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.runsDir, "not-a-log.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(now); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Error("old run log survived Prune")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh run log was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.runsDir, "not-a-log.txt")); err != nil {
		t.Errorf("unrelated file was removed: %v", err)
	}
}

func TestPruneWithoutVerdictLog(t *testing.T) {
	s := newStore(t)
	if err := s.Prune(time.Now()); err != nil {
		t.Errorf("Prune on an empty store: %v", err)
	}
}

func TestRunLogDay(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
		want string
	}{
		{"offsite-20261115.log", true, "2026-11-15"},
		{"item-with-dashes-20260101.log", true, "2026-01-01"},
		{"item-20261301.log", false, ""},
		{"item.log", false, ""},
		{"item-2026111.log", false, ""},
	}
	for _, tt := range tests {
		day, ok := runLogDay(tt.name)
		if ok != tt.ok {
			t.Errorf("%s: ok %v, want %v", tt.name, ok, tt.ok)
			continue
		}
		if ok && day.Format("2006-01-02") != tt.want {
			t.Errorf("%s: day %s, want %s", tt.name, day.Format("2006-01-02"), tt.want)
		}
	}
}

func TestValidSlug(t *testing.T) {
	valid := []string{"plain", "offsite", "branch-chore-land-prs", "pid-1234", "keep._-", "a..b"}
	for _, s := range valid {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	invalid := []string{"", ".", "..", "a/b", "a b", `a\b`, "a: b", "../escape", "-leading", "üñïcode"}
	for _, s := range invalid {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

func TestLockIsExclusiveBetweenStores(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(); err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	if err := b.Lock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock = %v, want ErrLocked", err)
	}
	if err := a.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := b.Lock(); err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	if err := b.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestLockIsNotReentrant(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Lock on one store = %v, want ErrLocked", err)
	}
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := a.Lock(); err != nil {
		t.Fatalf("Lock after Unlock: %v", err)
	}
	a.Unlock()
}

func TestUnlocksWhenNotHeld(t *testing.T) {
	s := newStore(t)
	if err := s.Unlock(); err != nil {
		t.Errorf("Unlock without Lock: %v", err)
	}
}

func TestHoldSurvivesEPERM(t *testing.T) {
	s := newStore(t)
	previous := processAlive
	processAlive = func(int) bool { return true }
	t.Cleanup(func() { processAlive = previous })
	if err := s.AddHold(Hold{Owner: "pid-1", PID: 1}); err != nil {
		t.Fatal(err)
	}
	holds, err := s.Holds()
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 {
		t.Fatalf("got %d holds, want the hold kept", len(holds))
	}
}

func TestHoldDiesWhenThePIDWasReused(t *testing.T) {
	s := newStore(t)
	previousAlive, previousStart := processAlive, processStart
	processAlive = func(int) bool { return true }
	processStart = func(int) (uint64, bool) { return 900, true }
	t.Cleanup(func() { processAlive, processStart = previousAlive, previousStart })

	if err := s.AddHold(Hold{Owner: "pid-2", PID: 2, StartTicks: 100}); err != nil {
		t.Fatal(err)
	}
	holds, err := s.Holds()
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 0 {
		t.Fatalf("holds %+v, want the reused pid treated as stale", holds)
	}
}

func TestHoldIsKeptWhenTheStartTimeCannotBeRead(t *testing.T) {
	s := newStore(t)
	previousAlive, previousStart := processAlive, processStart
	processAlive = func(int) bool { return true }
	processStart = func(int) (uint64, bool) { return 0, false }
	t.Cleanup(func() { processAlive, processStart = previousAlive, previousStart })

	if err := s.AddHold(Hold{Owner: "pid-3", PID: 3, StartTicks: 100}); err != nil {
		t.Fatal(err)
	}
	holds, err := s.Holds()
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 {
		t.Fatalf("holds %+v, want the hold kept when /proc cannot be read", holds)
	}
}

func TestDeferralsAreNewestPerItem(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, v := range []Verdict{
		{ID: "offsite", State: "not-live", At: base},
		{ID: "offsite", Kind: KindDefer, At: base, Until: base.Add(time.Hour)},
		{ID: "offsite", Kind: KindDefer, At: base.Add(time.Minute), Until: base.Add(3 * time.Hour)},
		{ID: "backup", State: "live", At: base},
	} {
		if err := s.AppendVerdict(v); err != nil {
			t.Fatal(err)
		}
	}
	deferred, err := s.Deferrals()
	if err != nil {
		t.Fatal(err)
	}
	if len(deferred) != 1 {
		t.Fatalf("got %d deferrals, want 1: %+v", len(deferred), deferred)
	}
	if got := deferred["offsite"].Until; !got.Equal(base.Add(3 * time.Hour)) {
		t.Errorf("until %v, want the newest deferral", got)
	}
	if _, ok := deferred["backup"]; ok {
		t.Error("a live measurement was treated as a deferral")
	}
}

func TestNotifiedRoundTrips(t *testing.T) {
	s := newStore(t)
	keys, err := s.Notified()
	if err != nil {
		t.Fatalf("Notified on a missing file: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("missing file gave %v, want empty", keys)
	}

	want := map[string]bool{"offsite/not-live/git push failed": true, "reboot/blocked/hold": true}
	if err := s.SaveNotified(want); err != nil {
		t.Fatalf("SaveNotified: %v", err)
	}
	got, err := s.Notified()
	if err != nil {
		t.Fatalf("Notified: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	for key := range want {
		if !got[key] {
			t.Errorf("key %q missing", key)
		}
	}

	data, err := os.ReadFile(filepath.Join(s.Dir(), "notified.json"))
	if err != nil {
		t.Fatalf("read notified.json: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("notified.json has no trailing newline")
	}
}

func TestNotifiedQuarantinesACorruptFile(t *testing.T) {
	s := newStore(t)
	path := filepath.Join(s.Dir(), "notified.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := s.Notified()
	if err != nil {
		t.Fatalf("Notified on a corrupt file: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("corrupt file gave %v, want empty", keys)
	}
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	quarantined := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "notified.json.corrupt-") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Error("a corrupt notified file was not quarantined")
	}
}

func TestParseStartTicks(t *testing.T) {
	// A comm field with spaces and parentheses must not shift the field index.
	stat := "4242 (a (weird) name) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 123456 20 21\n"
	ticks, ok := parseStartTicks([]byte(stat))
	if !ok {
		t.Fatal("parseStartTicks failed")
	}
	if ticks != 123456 {
		t.Errorf("ticks %d, want 123456", ticks)
	}
	if _, ok := parseStartTicks([]byte("garbage")); ok {
		t.Error("parseStartTicks accepted garbage")
	}
	if _, ok := parseStartTicks([]byte("1 (short) S 1 2\n")); ok {
		t.Error("parseStartTicks accepted a truncated stat line")
	}
}
