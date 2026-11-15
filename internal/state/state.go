// Package state owns everything lares writes to disk.
//
// Four files live in one directory:
//
//	verdicts.jsonl         append-only, one record per measure or deferral
//	queue.json             items awaiting judgement, plus queue.json.bak
//	holds/<owner>.json     one file per live hold
//	runs/<item>-<date>.log one run log per item per day
//	lock                   the interprocess lock a pass and a reboot share
//
// Two durability rules hold everywhere. The verdict log is append-only, so a
// torn final line is the normal crash case and is skipped on read. The queue is
// replaced, never edited: write a temp file in the same directory, fsync it,
// copy the current file to the backup, rename, then fsync the directory. A
// reader that finds a short or corrupt queue falls back to the backup and
// quarantines the bad file.
//
// Verdict.State carries the contract's four words: live, not-live, stale, and
// unknown. This package treats it as an opaque string so it compiles without
// the contract package.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Verdict is one record in the append-only log.
//
// A measurement leaves Kind and Until zero. A deferral sets Kind to KindDefer
// and Until to the instant the item becomes due again, so rebuilding the queue
// replays the operator's judgement.
type Verdict struct {
	ID     string    `json:"id"`
	State  string    `json:"state"`
	Detail string    `json:"detail"`
	Fix    string    `json:"fix"`
	At     time.Time `json:"at"`
	Kind   string    `json:"kind,omitempty"`
	Until  time.Time `json:"until,omitempty"`
}

// KindDefer marks an operator deferral in the verdict log.
const KindDefer = "defer"

// IsDeferral reports whether v records an operator deferral rather than a
// measurement.
func (v Verdict) IsDeferral() bool { return v.Kind == KindDefer }

// Item is one queued repair awaiting judgement.
type Item struct {
	ID            string    `json:"id"`
	Term          string    `json:"term"`
	FirstSeen     time.Time `json:"first_seen"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastRunLog    string    `json:"last_run_log,omitempty"`
	// Blocked names a terminal failure kind. A blocked item is queued without
	// a retry until it leaves the queue or an operator deferral clears it.
	Blocked string `json:"blocked,omitempty"`
}

// Queue is the whole queue file.
type Queue struct {
	Generation int    `json:"generation"`
	Items      []Item `json:"items"`
}

// Hold is one process's declaration that a reboot or disruptive repair must
// wait. StartTicks tells this process from a later one that reused its pid.
// Stale is set on read and never written.
type Hold struct {
	Owner      string    `json:"owner"`
	Reason     string    `json:"reason"`
	PID        int       `json:"pid"`
	StartTicks uint64    `json:"start_ticks,omitempty"`
	Start      time.Time `json:"start"`
	Stale      bool      `json:"-"`
}

// ErrNoQueue reports that neither the queue nor its backup exists.
var ErrNoQueue = errors.New("no queue file")

// ErrLocked reports that another process holds the state directory lock.
var ErrLocked = errors.New("state directory is locked")

// processAlive reports whether pid names a live process. Tests replace it.
//
// A permission error means the process exists under another user, so only ESRCH
// is dead. Treating EPERM as dead would drop a live hold.
var processAlive = func(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processStart reads pid's start time in clock ticks since boot. Tests replace
// it.
var processStart = func(pid int) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return parseStartTicks(data)
}

// ProcessAlive reports whether pid names a live process.
func ProcessAlive(pid int) bool { return processAlive(pid) }

// ProcessStartTicks returns pid's start time, so a hold can name one process
// instance rather than whichever process holds the pid later.
func ProcessStartTicks(pid int) (uint64, bool) { return processStart(pid) }

// parseStartTicks reads field 22 from /proc/<pid>/stat. The comm field may
// contain spaces and parentheses, so parsing starts after the last ')'.
func parseStartTicks(data []byte) (uint64, bool) {
	text := string(data)
	close := strings.LastIndex(text, ")")
	if close < 0 || close+2 > len(text) {
		return 0, false
	}
	fields := strings.Fields(text[close+1:])
	// fields[0] is field 3 (state), so starttime (field 22) is fields[19].
	const index = 22 - 3
	if len(fields) <= index {
		return 0, false
	}
	ticks, err := strconv.ParseUint(fields[index], 10, 64)
	if err != nil {
		return 0, false
	}
	return ticks, true
}

// holdLive reports whether h's process is still the process that wrote it.
//
// A start-time mismatch means the pid was reused. An unreadable /proc keeps the
// hold, because dropping it is the dangerous direction.
func holdLive(h Hold) bool {
	if h.PID <= 0 || !processAlive(h.PID) {
		return false
	}
	if h.StartTicks == 0 {
		return true
	}
	ticks, ok := processStart(h.PID)
	if !ok {
		return true
	}
	return ticks == h.StartTicks
}

// runLogCap caps one day's log for one item.
var runLogCap int64 = 10 << 20

// slugPattern matches an identity that is also a safe filename stem. It starts
// with alphanumeric so a name can never be "." or "..".
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidSlug reports whether s is safe to use as a file stem. A name that would
// need rewriting is rejected instead, so two identities never collide on one
// file.
func ValidSlug(s string) bool {
	return slugPattern.MatchString(s)
}

// Store is an open state directory.
type Store struct {
	dir      string
	holdsDir string
	runsDir  string

	lockMu   sync.Mutex
	lockFile *os.File
	locked   bool
}

// Open creates dir, dir/holds, and dir/runs with mode 0o700. It is idempotent.
func Open(dir string) (*Store, error) {
	s := &Store{
		dir:      dir,
		holdsDir: filepath.Join(dir, "holds"),
		runsDir:  filepath.Join(dir, "runs"),
	}
	for _, path := range []string{s.dir, s.holdsDir, s.runsDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
	}
	return s, nil
}

// Dir returns the state directory.
func (s *Store) Dir() string {
	return s.dir
}

// verdictPath returns the append-only verdict log path.
func (s *Store) verdictPath() string {
	return filepath.Join(s.dir, "verdicts.jsonl")
}

// queuePath returns the primary queue path.
func (s *Store) queuePath() string {
	return filepath.Join(s.dir, "queue.json")
}

// backupPath returns the queue backup path.
func (s *Store) backupPath() string {
	return filepath.Join(s.dir, "queue.json.bak")
}

// syncdir makes a rename durable by syncing the directory that holds it.
func syncdir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", dir, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
