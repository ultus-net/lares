// Package contract decodes a verdict stream from an external probe.
//
// The built-in checks produce records in-process and never use this package's
// parser. The stream exists for the optional audit_command escape hatch: a
// script measures the host and prints one JSON object per line, and this
// package reads it. lares never repairs a record an external probe produced.
//
// The stream is one object per line with exactly the keys `id`, `state`,
// `detail` and `fix`, and a single `summary` record last. `state` is one of
// `live`, `not-live`, `stale` or `unknown`.
package contract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// MaxLineBytes caps one record line. A probe detail that needs more than a
// mebibyte is a probe bug, not a reason to grow memory without limit.
const MaxLineBytes = 1 << 20

var (
	errEmptyStream       = errors.New("empty stream")
	errLineTooLong       = fmt.Errorf("line exceeds the %d byte limit", MaxLineBytes)
	errNoSummary         = errors.New("no summary record")
	errMultipleSummaries = errors.New("more than one summary record")
	errSummaryNotAtEnd   = errors.New("summary record is not last")
	errNoRecords         = errors.New("no record besides the summary")
)

// keys required on every record, in the order errors name them.
var requiredKeys = []string{"id", "state", "detail", "fix"}

// Parse decodes the probe's stream.
//
// It returns the records in the probe's order, including the summary. It is an
// error to send an empty stream, a line over MaxLineBytes, a line that is not
// one object with the four keys, an unknown state, or a stream whose summary is
// missing, duplicated, or not last.
func Parse(r io.Reader) ([]Record, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)

	var records []Record
	for line := 1; scanner.Scan(); line++ {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		rec, err := decodeLine(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, errLineTooLong
		}
		return nil, fmt.Errorf("read stream: %w", err)
	}

	if len(records) == 0 {
		return nil, errEmptyStream
	}
	if err := checkSummary(records); err != nil {
		return nil, err
	}
	return records, nil
}

// decodeLine turns one line into a Record, rejecting anything the contract does
// not allow.
func decodeLine(raw []byte) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return Record{}, fmt.Errorf("not one JSON object: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Record{}, errors.New("trailing data after the object")
	}
	for _, key := range requiredKeys {
		if _, ok := fields[key]; !ok {
			return Record{}, fmt.Errorf("missing key %q", key)
		}
	}
	unknown := make([]string, 0, len(fields))
	for key := range fields {
		if !isRequiredKey(key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return Record{}, fmt.Errorf("unknown key %q", unknown[0])
	}

	rec := Record{}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("bad value: %w", err)
	}

	if rec.ID == "" {
		return Record{}, errors.New("id is empty")
	}
	if !rec.State.Valid() {
		return Record{}, fmt.Errorf("unknown state %q", rec.State)
	}
	return rec, nil
}

func isRequiredKey(key string) bool {
	for _, want := range requiredKeys {
		if key == want {
			return true
		}
	}
	return false
}

// checkSummary enforces one summary record, last, with at least one record
// before it.
func checkSummary(records []Record) error {
	summaries := 0
	for i, rec := range records {
		if !rec.IsSummary() {
			continue
		}
		summaries++
		if i != len(records)-1 {
			return errSummaryNotAtEnd
		}
	}
	switch {
	case summaries == 0:
		return errNoSummary
	case summaries > 1:
		return errMultipleSummaries
	case len(records) == 1:
		return errNoRecords
	}
	return nil
}
