package contract

import (
	"encoding/json"
)

// State is a verdict's state word.
type State string

const (
	Live    State = "live"
	NotLive State = "not-live"
	Stale   State = "stale"
	Unknown State = "unknown"
)

// SummaryID is the id of the single trailing record that counts the rest.
const SummaryID = "summary"

// Record is one verdict from the probe.
type Record struct {
	ID     string `json:"id"`
	State  State  `json:"state"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

// IsFinding reports whether the record is a finding: not-live or stale. The
// CLI uses this for its exit code, so an unknown record does not fail a measure.
func (r Record) IsFinding() bool {
	return r.State == NotLive || r.State == Stale
}

// NeedsAttention reports whether the controller must queue the record, which is
// every state except live. An unknown measurement is queued without a repair,
// because no check proved the item healthy.
func (r Record) NeedsAttention() bool {
	return r.State != Live
}

// IsSummary reports whether the record is the trailing summary.
func (r Record) IsSummary() bool {
	return r.ID == SummaryID
}

// Valid reports whether s is one of the four contract states.
func (s State) Valid() bool {
	switch s {
	case Live, NotLive, Stale, Unknown:
		return true
	}
	return false
}

// UnmarshalJSON keeps the compiler honest about the wire type: a state the
// contract does not name is rejected where it is decoded.
func (s *State) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = State(v)
	return nil
}
