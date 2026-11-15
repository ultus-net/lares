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

// IsFinding reports whether the record needs attention, which is every state
// except live.
func (r Record) IsFinding() bool {
	return r.State == NotLive || r.State == Stale
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
