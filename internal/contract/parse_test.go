package contract

import (
	"os"
	"strings"
	"testing"
)

// Fixtures in testdata/ were captured from `bash install/lib/hygiene-audit.sh
// --json` on the operator's Omarchy host on 2026-11-15. host-live.jsonl comes
// from a run with no findings (the offsite and branch records were live at the
// time); host-drift.jsonl is the captured run with one not-live record and one
// stale record.

func mustFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

func TestParseFixtures(t *testing.T) {
	tests := []struct {
		fixture      string
		wantRecords  int
		wantFindings int
		wantSummary  State
	}{
		{"host-live.jsonl", 11, 0, Live},
		{"host-drift.jsonl", 11, 2, NotLive},
		{"escaped.jsonl", 2, 0, Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			records, err := Parse(strings.NewReader(mustFixture(t, tt.fixture)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(records) != tt.wantRecords {
				t.Fatalf("got %d records, want %d", len(records), tt.wantRecords)
			}
			last := records[len(records)-1]
			if !last.IsSummary() {
				t.Fatalf("last record %q is not the summary", last.ID)
			}
			if last.State != tt.wantSummary {
				t.Errorf("summary state %q, want %q", last.State, tt.wantSummary)
			}
			findings := 0
			for _, rec := range records {
				if !rec.IsSummary() && rec.IsFinding() {
					findings++
				}
			}
			if findings != tt.wantFindings {
				t.Errorf("got %d findings, want %d", findings, tt.wantFindings)
			}
		})
	}
}

func TestParseRestoresEscapedDetail(t *testing.T) {
	records, err := Parse(strings.NewReader(mustFixture(t, "escaped.jsonl")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := "quote \" backslash \\ control \x01 newline \n tab \t"
	if got := records[0].Detail; got != want {
		t.Errorf("detail %q, want %q", got, want)
	}
}

func TestParseRejects(t *testing.T) {
	live := `{"id":"a","state":"live","detail":"","fix":""}` + "\n"
	summary := `{"id":"summary","state":"live","detail":"live=1 not-live=0 stale=0 unknown=0","fix":""}` + "\n"

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty stream", "", "empty stream"},
		{"blank lines only", "\n\t\n   \n", "empty stream"},
		{"line over the cap", strings.Repeat("x", MaxLineBytes+1) + "\n", "byte limit"},
		{"not an object", "plain text\n" + live + summary, "not one JSON object"},
		{"array", "[]\n" + summary, "not one JSON object"},
		{"missing id", `{"state":"live","detail":"","fix":""}` + "\n" + summary, `missing key "id"`},
		{"missing state", `{"id":"a","detail":"","fix":""}` + "\n" + summary, `missing key "state"`},
		{"missing detail", `{"id":"a","state":"live","fix":""}` + "\n" + summary, `missing key "detail"`},
		{"missing fix", `{"id":"a","state":"live","detail":""}` + "\n" + summary, `missing key "fix"`},
		{"unknown key", `{"id":"a","state":"live","detail":"","fix":"","extra":1}` + "\n" + summary, `unknown key "extra"`},
		{"wrong type", `{"id":1,"state":"live","detail":"","fix":""}` + "\n" + summary, "bad value"},
		{"trailing data", `{"id":"a","state":"live","detail":"","fix":""} {}` + "\n" + summary, "trailing data"},
		{"empty id", `{"id":"","state":"live","detail":"","fix":""}` + "\n" + summary, "id is empty"},
		{"unknown state", `{"id":"a","state":"broken","detail":"","fix":""}` + "\n" + summary, `unknown state "broken"`},
		{"no summary", live, "no summary"},
		{"two summaries", live + summary + summary, "not last"},
		{"summary first", summary + live, "not last"},
		{"summary only", summary, "besides the summary"},
		{"summary then blank then record", summary + "\n" + live, "not last"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, err := Parse(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("Parse accepted the stream and returned %d records", len(records))
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestParseCountsLinesFromOne(t *testing.T) {
	input := "\n" + `{"id":"a","state":"live","detail":"","fix":""}` + "\n" + `{"id":"b","state":"nope","detail":"","fix":""}` + "\n"
	_, err := Parse(strings.NewReader(input))
	if err == nil {
		t.Fatal("Parse accepted an unknown state")
	}
	if !strings.HasPrefix(err.Error(), "line 3:") {
		t.Fatalf("error %q does not name line 3", err)
	}
}

func TestParseSkipsBlankLines(t *testing.T) {
	input := "\n" + `{"id":"a","state":"live","detail":"","fix":""}` + "\n\n\t\n" +
		`{"id":"summary","state":"live","detail":"","fix":""}` + "\n\n"
	records, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
}

func TestStateValid(t *testing.T) {
	for _, s := range []State{Live, NotLive, Stale, Unknown} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if State("Live").Valid() {
		t.Error(`"Live" should not be valid`)
	}
}

func TestRecordPredicates(t *testing.T) {
	tests := []struct {
		rec     Record
		finding bool
		summary bool
	}{
		{Record{ID: "a", State: Live}, false, false},
		{Record{ID: "a", State: NotLive}, true, false},
		{Record{ID: "a", State: Stale}, true, false},
		{Record{ID: "a", State: Unknown}, false, false},
		{Record{ID: SummaryID, State: Unknown}, false, true},
	}
	for _, tt := range tests {
		if got := tt.rec.IsFinding(); got != tt.finding {
			t.Errorf("%+v IsFinding = %v, want %v", tt.rec, got, tt.finding)
		}
		if got := tt.rec.IsSummary(); got != tt.summary {
			t.Errorf("%+v IsSummary = %v, want %v", tt.rec, got, tt.summary)
		}
	}
}
