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

// Holds returns the live holds and deletes the rest.
func (s *Store) Holds() ([]Hold, error) {
	return s.holds(false)
}

// HoldsIncludingStale returns every hold, marking the dead ones. A hold whose
// file does not parse is stale too.
func (s *Store) HoldsIncludingStale() ([]Hold, error) {
	return s.holds(true)
}

// holds lists holds, optionally keeping stale ones.
func (s *Store) holds(includeStale bool) ([]Hold, error) {
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
			if includeStale {
				holds = append(holds, Hold{Owner: strings.TrimSuffix(entry.Name(), ".json"), Stale: true})
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("remove unparsable hold %s: %w", path, err)
			}
			continue
		}
		if hold.PID <= 0 || !processAlive(hold.PID) {
			if includeStale {
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
// The owner is sanitised to a filename stem, and the stored Owner is that stem,
// so listing holds reports the identity that was written.
func (s *Store) AddHold(h Hold) error {
	owner, err := holdStem(h.Owner)
	if err != nil {
		return err
	}
	h.Owner = owner

	data, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("encode hold: %w", err)
	}
	data = append(data, '\n')
	return writeFileSync(filepath.Join(s.holdsDir, owner+".json"), data)
}

// RemoveHold deletes owner's hold. Removing an absent hold is not an error.
func (s *Store) RemoveHold(owner string) error {
	stem, err := holdStem(owner)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.holdsDir, stem+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove hold %s: %w", stem, err)
	}
	return nil
}

// holdStem sanitises an owner and rejects the names that cannot be files.
func holdStem(owner string) (string, error) {
	stem := sanitizeOwner(owner)
	if stem == "" || stem == "." || stem == ".." {
		return "", fmt.Errorf("invalid hold owner %q", owner)
	}
	return stem, nil
}

// RunLog appends body to the item's log for at's UTC day and returns the path.
// It refuses a write that would take the file past runLogCap.
func (s *Store) RunLog(item string, at time.Time, body []byte) (string, error) {
	stem := sanitizeOwner(item)
	if stem == "" || stem == "." || stem == ".." {
		return "", fmt.Errorf("invalid run log item %q", item)
	}
	path := filepath.Join(s.runsDir, fmt.Sprintf("%s-%s.log", stem, at.UTC().Format("20060102")))

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
	for _, v := range verdicts {
		if v.At.Before(cutoff) {
			continue
		}
		line, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("encode verdict: %w", err)
		}
		kept.Write(line)
		kept.WriteByte('\n')
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
