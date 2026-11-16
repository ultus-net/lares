package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// verdictRetention is how long a verdict survives Prune.
const verdictRetention = 90 * 24 * time.Hour

// runLogRetention is how long a run log survives Prune.
const runLogRetention = 30 * 24 * time.Hour

// AppendVerdict adds v to the end of the verdict log and syncs it.
func (s *Store) AppendVerdict(v Verdict) error {
	f, err := os.OpenFile(s.verdictPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open verdict log: %w", err)
	}
	defer f.Close()

	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode verdict: %w", err)
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("append verdict: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync verdict log: %w", err)
	}
	return nil
}

// Verdicts returns the log in file order. A line that does not parse is
// skipped, because a torn final line is the normal crash case. A log that
// exists but cannot be read is an error.
func (s *Store) Verdicts() ([]Verdict, error) {
	f, err := os.Open(s.verdictPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open verdict log: %w", err)
	}
	defer f.Close()

	var verdicts []Verdict
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var v Verdict
		if err := json.Unmarshal(line, &v); err != nil {
			continue
		}
		verdicts = append(verdicts, v)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read verdict log: %w", err)
	}
	return verdicts, nil
}

// Deferrals returns the newest deferral per item, in log order.
func (s *Store) Deferrals() (map[string]Verdict, error) {
	verdicts, err := s.Verdicts()
	if err != nil {
		return nil, err
	}
	deferred := make(map[string]Verdict)
	for _, v := range verdicts {
		if v.IsDeferral() {
			deferred[v.ID] = v
		}
	}
	return deferred, nil
}

// Queue returns the queue and whether it came from the backup after the
// primary was found unparsable. ErrNoQueue means neither file exists.
func (s *Store) Queue() (Queue, bool, error) {
	q, err := readQueue(s.queuePath())
	switch {
	case err == nil:
		return q, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return s.recoverQueue(err)
	}

	backup, backupErr := readQueue(s.backupPath())
	switch {
	case backupErr == nil:
		return backup, true, nil
	case errors.Is(backupErr, fs.ErrNotExist):
		return Queue{}, false, ErrNoQueue
	default:
		return Queue{}, false, fmt.Errorf("read queue backup: %w", backupErr)
	}
}

// recoverQueue quarantines an unparsable primary and loads the backup.
func (s *Store) recoverQueue(primaryErr error) (Queue, bool, error) {
	quarantine := s.queuePath() + ".corrupt-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(s.queuePath(), quarantine); err != nil {
		return Queue{}, false, fmt.Errorf("quarantine corrupt queue: %w", err)
	}
	backup, err := readQueue(s.backupPath())
	if err != nil {
		return Queue{}, false, fmt.Errorf("queue %w; backup: %w", primaryErr, err)
	}
	return backup, true, nil
}

// readQueue parses one queue file. An empty file, trailing garbage, or a wrong
// JSON type is an error.
func readQueue(path string) (Queue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Queue{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Queue{}, fmt.Errorf("%s is empty", path)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var q Queue
	if err := dec.Decode(&q); err != nil {
		return Queue{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Queue{}, fmt.Errorf("parse %s: trailing data", path)
	}
	return q, nil
}

// SaveQueue replaces the queue atomically and returns the new generation.
//
// The generation is monotonic: it is one more than the larger of the stored
// generation and q.Generation, so a caller that passes a stale value cannot
// move it backwards.
func (s *Store) SaveQueue(q Queue) (int, error) {
	generation := q.Generation
	if current, err := readQueue(s.queuePath()); err == nil && current.Generation > generation {
		generation = current.Generation
	}
	generation++
	q.Generation = generation

	data, err := json.Marshal(q)
	if err != nil {
		return 0, fmt.Errorf("encode queue: %w", err)
	}
	data = append(data, '\n')

	if err := replaceQueueFile(s.dir, s.queuePath(), s.backupPath(), data); err != nil {
		return 0, err
	}
	if err := syncdir(s.dir); err != nil {
		return 0, err
	}
	return generation, nil
}

// replaceQueueFile writes the temp file, syncs it, copies the current primary
// to the backup when one exists, and renames the temp over the primary.
func replaceQueueFile(dir, primary, backup string, data []byte) error {
	tmp := filepath.Join(dir, "queue.json.tmp")
	if err := writeFileSync(tmp, data); err != nil {
		return err
	}
	if current, err := os.ReadFile(primary); err == nil {
		if err := writeFileSync(backup, current); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, primary); err != nil {
		return fmt.Errorf("replace queue: %w", err)
	}
	return nil
}

// writeFileSync writes data to path with mode 0o600 and syncs it.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// Notified returns the keys lares has already surfaced.
//
// A missing file is an empty set. A file that does not parse is quarantined and
// read as empty, so a damaged cache costs one repeated notice instead of a
// failed pass. The quarantine keeps the evidence of the damage.
func (s *Store) Notified() (map[string]bool, error) {
	keys := make(map[string]bool)
	data, err := os.ReadFile(s.notifiedPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return keys, nil
		}
		return nil, fmt.Errorf("read notified set: %w", err)
	}
	var notified Notified
	if err := json.Unmarshal(data, &notified); err != nil {
		quarantine := s.notifiedPath() + ".corrupt-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if renameErr := os.Rename(s.notifiedPath(), quarantine); renameErr != nil {
			return nil, fmt.Errorf("quarantine notified set: %w", renameErr)
		}
		return keys, nil
	}
	for _, key := range notified.Keys {
		keys[key] = true
	}
	return keys, nil
}

// SaveNotified replaces the dedupe set atomically. Keys are sorted, so the same
// set always renders the same file.
func (s *Store) SaveNotified(keys map[string]bool) error {
	list := make([]string, 0, len(keys))
	for key := range keys {
		list = append(list, key)
	}
	sort.Strings(list)

	data, err := json.Marshal(Notified{Keys: list})
	if err != nil {
		return fmt.Errorf("encode notified set: %w", err)
	}
	data = append(data, '\n')

	return writeFileAtomic(s.dir, s.notifiedPath(), data)
}

// Holds returns the live holds. It deletes a hold whose process is gone, so a
// crashed tool cannot pin the host. A hold that cannot be read or parsed is an
// error: lares cannot prove the barrier is dead, so it fails closed.
func (s *Store) Holds() ([]Hold, error) {
	return s.holds(true)
}

// HoldsIncludingStale returns every hold and marks the dead ones. It never
// writes, so a status read cannot remove a hold that a writer is publishing.
func (s *Store) HoldsIncludingStale() ([]Hold, error) {
	return s.holds(false)
}

// holds lists holds. prune removes the dead ones; only a caller that holds the
// state lock passes true.
func (s *Store) holds(prune bool) ([]Hold, error) {
	entries, err := os.ReadDir(s.holdsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read holds: %w", err)
	}

	holds := make([]Hold, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(s.holdsDir, entry.Name())
		hold, err := readHold(path)
		if err != nil {
			return nil, fmt.Errorf("read hold: %w", err)
		}
		if !holdLive(hold) {
			if !prune {
				hold.Stale = true
				holds = append(holds, hold)
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("remove stale hold %s: %w", path, err)
			}
			continue
		}
		holds = append(holds, hold)
	}
	return holds, nil
}

// readHold parses one hold file.
func readHold(path string) (Hold, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Hold{}, err
	}
	var hold Hold
	if err := json.Unmarshal(data, &hold); err != nil {
		return Hold{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return hold, nil
}

// AddHold writes h, replacing any hold with the same owner.
//
// The owner must already be a slug, and the stored Owner is that slug, so
// listing holds reports the identity that was written and no two owners share a
// file. The write is atomic, so a reader sees the old hold or the complete new
// one and a crash can never leave half a barrier behind.
func (s *Store) AddHold(h Hold) error {
	if !ValidSlug(h.Owner) {
		return fmt.Errorf("invalid hold owner %q", h.Owner)
	}
	data, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encode hold: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(s.dir, filepath.Join(s.holdsDir, h.Owner+".json"), data)
}

// writeFileAtomic writes data to a temp file in dir, syncs it, renames it over
// path, and syncs the directory that receives it. The temp lives outside the
// destination directory, so a reader never lists a partial file.
func writeFileAtomic(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return syncdir(filepath.Dir(path))
}

// RemoveHold deletes owner's hold. Removing an absent hold is not an error.
func (s *Store) RemoveHold(owner string) error {
	if !ValidSlug(owner) {
		return fmt.Errorf("invalid hold owner %q", owner)
	}
	if err := os.Remove(filepath.Join(s.holdsDir, owner+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove hold %s: %w", owner, err)
	}
	return nil
}

// RunLog appends body to the item's log for at's UTC day and returns the path.
// It refuses a write that would take the file past runLogCap.
func (s *Store) RunLog(item string, at time.Time, body []byte) (string, error) {
	if !ValidSlug(item) {
		return "", fmt.Errorf("invalid run log item %q", item)
	}
	path := filepath.Join(s.runsDir, fmt.Sprintf("%s-%s.log", item, at.UTC().Format("20060102")))

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open run log %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat run log %s: %w", path, err)
	}
	if info.Size()+int64(len(body)) > runLogCap {
		return "", fmt.Errorf("run log %s would pass the %d byte cap", path, runLogCap)
	}
	if _, err := f.Write(body); err != nil {
		return "", fmt.Errorf("append run log %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync run log %s: %w", path, err)
	}
	return path, nil
}

// Prune drops verdicts older than 90 days and run logs older than 30 days.
func (s *Store) Prune(now time.Time) error {
	if err := s.pruneVerdicts(now.Add(-verdictRetention)); err != nil {
		return err
	}
	return s.pruneRunLogs(now.Add(-runLogRetention))
}

// pruneVerdicts rewrites the verdict log without records older than cutoff.
func (s *Store) pruneVerdicts(cutoff time.Time) error {
	verdicts, err := s.Verdicts()
	if err != nil {
		return err
	}
	if verdicts == nil {
		return nil
	}

	var kept bytes.Buffer
	stale := false
	for _, v := range verdicts {
		if v.At.Before(cutoff) {
			stale = true
			continue
		}
		line, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode verdict: %w", err)
		}
		kept.Write(line)
		kept.WriteByte('\n')
	}
	if !stale {
		return nil
	}

	tmp := s.verdictPath() + ".tmp"
	if err := writeFileSync(tmp, kept.Bytes()); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.verdictPath()); err != nil {
		return fmt.Errorf("replace verdict log: %w", err)
	}
	return syncdir(s.dir)
}

// pruneRunLogs deletes run logs whose date suffix is older than cutoff.
func (s *Store) pruneRunLogs(cutoff time.Time) error {
	entries, err := os.ReadDir(s.runsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read run logs: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		day, ok := runLogDay(entry.Name())
		if !ok || !day.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.runsDir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove run log %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// runLogDay reads the UTC day out of <item>-YYYYMMDD.log.
func runLogDay(name string) (time.Time, bool) {
	base := strings.TrimSuffix(name, ".log")
	dash := strings.LastIndex(base, "-")
	if dash < 0 || len(base)-dash-1 != 8 {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation("20060102", base[dash+1:], time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}
