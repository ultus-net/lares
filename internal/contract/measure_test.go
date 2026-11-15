package contract

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// probeScript is the shell program used as the probe. Measure appends --json,
// so the script receives it as $1 after the placeholder $0.
const probeOK = `[ "$1" = "--json" ] || { echo "missing --json" >&2; exit 9; }; cat testdata/host-live.jsonl`

func TestMeasureParsesProbeOutput(t *testing.T) {
	records, err := Measure(context.Background(), []string{"sh", "-c", probeOK, "sh"})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(records) != 11 {
		t.Fatalf("got %d records, want 11", len(records))
	}
	if !records[len(records)-1].IsSummary() {
		t.Fatal("last record is not the summary")
	}
}

func TestMeasureAppendsJSONFlag(t *testing.T) {
	script := `for a in "$@"; do [ "$a" = "--json" ] || { echo "unexpected arg $a" >&2; exit 9; }; done; cat testdata/host-live.jsonl`
	if _, err := Measure(context.Background(), []string{"sh", "-c", script, "sh"}); err != nil {
		t.Fatalf("Measure: %v", err)
	}
}

func TestMeasureRejectsEmptyArgv(t *testing.T) {
	if _, err := Measure(context.Background(), nil); err == nil {
		t.Fatal("Measure accepted a nil argv")
	}
	if _, err := Measure(context.Background(), []string{""}); err == nil {
		t.Fatal("Measure accepted an empty program")
	}
}

func TestMeasureReportsExitAndLastStderrLine(t *testing.T) {
	script := "echo 'first line' >&2; echo 'the real reason' >&2; echo >&2; exit 3"
	_, err := Measure(context.Background(), []string{"sh", "-c", script, "sh"})
	if err == nil {
		t.Fatal("Measure accepted a failing probe")
	}
	if !strings.Contains(err.Error(), "exit 3") {
		t.Errorf("error %q does not name the exit code", err)
	}
	if !strings.Contains(err.Error(), "the real reason") {
		t.Errorf("error %q does not carry the last stderr line", err)
	}
}

func TestMeasureReportsParseFailure(t *testing.T) {
	script := "echo 'not json'"
	_, err := Measure(context.Background(), []string{"sh", "-c", script, "sh"})
	if err == nil {
		t.Fatal("Measure accepted an unparsable stream")
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("error %q does not name the line", err)
	}
}

func TestMeasureSurvivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Measure(ctx, []string{"sh", "-c", "cat testdata/host-live.jsonl", "sh"})
	if err == nil {
		t.Fatal("Measure ignored a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %q does not match context.Canceled", err)
	}
}

func TestMeasureReportsMissingProgram(t *testing.T) {
	_, err := Measure(context.Background(), []string{"definitely-not-a-real-program-xyz"})
	if err == nil {
		t.Fatal("Measure accepted a missing program")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Logf("error %q does not match exec.ErrNotFound; it is still an error", err)
	}
}
