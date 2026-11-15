// Package state owns everything lares writes to disk.
//
// Four files live in one directory:
//
//	verdicts.jsonl         append-only, one record per measure
//	queue.json             items awaiting judgement, plus queue.json.bak
//	holds/<owner>.json     one file per live hold
//	runs/<item>-<date>.log one run log per item per day
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
	"syscall"
	"time"
)

// Verdict is one measurement of one item.
type Verdict struct {
	ID     string    `json:"id"`
	State  string    `json:"state"`
	Detail string    `json:"detail"`
	Fix    string    `json:"fix"`
	At     time.Time `json:"at"`
}

// Item is one queued repair awaiting judgement.
type Item struct {
	ID            string    `json:"id"`
	Term          string    `json:"term"`
	FirstSeen     time.Time `json:"first_seen"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastRunLog    string    `json:"last_run_log,omitempty"`
}

// Queue is the whole queue file.
type Queue struct {
	Generation int    `json:"generation"`
	Items      []Item `json:"items"`
}

// Hold is one process's declaration that a reboot or disruptive repair must
// wait. Stale is set on read and never written.
type Hold struct {
	Owner  string    `json:"owner"`
	Reason string    `json:"reason"`
	PID    int       `json:"pid"`
	Start  time.Time `json:"start"`
	Stale  bool      `json:"-"`
}

// ErrNoQueue reports that neither the queue nor its backup exists.
var ErrNoQueue = errors.New("no queue file")

// processAlive reports whether pid names a live process. Tests replace it.
var processAlive = func(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// runLogCap caps one day's log for one item.
var runLogCap int64 = 10 << 20

// ownerChars matches everything a hold owner may not contain.
var ownerChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// sanitizeOwner turns an owner into a filename stem. It never returns a path.
func sanitizeOwner(owner string) string {
	return ownerChars.ReplaceAllString(owner, "_")
}

// Store is an open state directory.
type Store struct {
	dir      string
	holdsDir string
	runsDir  string
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
