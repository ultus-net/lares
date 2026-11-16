package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/state"
)

// fedoraAdapters holds the two families whose stage verb needs root.
func fedoraAdapters() map[adapter.Family]adapter.Adapter {
	return map[adapter.Family]adapter.Adapter{
		adapter.FedoraAtomic: {
			Family:   adapter.FedoraAtomic,
			Stage:    []string{"rpm-ostree", "upgrade"},
			Activate: []string{"systemctl", "reboot"},
		},
		adapter.Bootc: {
			Family:   adapter.Bootc,
			Stage:    []string{"bootc", "upgrade"},
			Activate: []string{"systemctl", "reboot"},
		},
	}
}

// patchTrainDrift is the probe's finding on a host with no patch-train timer.
func patchTrainDrift() []contract.Record {
	return []contract.Record{
		rec("patch-train", contract.NotLive, "install/host-setup.sh --only patch-train --apply"),
		summary(contract.NotLive),
	}
}

// newestVerdict returns the last verdict for id.
func newestVerdict(t *testing.T, store *state.Store, id string) state.Verdict {
	t.Helper()
	verdicts, err := store.Verdicts()
	if err != nil {
		t.Fatalf("Verdicts: %v", err)
	}
	var found state.Verdict
	seen := false
	for _, verdict := range verdicts {
		if verdict.ID == id && !verdict.IsDeferral() {
			found, seen = verdict, true
		}
	}
	if !seen {
		t.Fatalf("no verdict for %s", id)
	}
	return found
}

func TestPassQueuesTheStageCommandOnFedora(t *testing.T) {
	for _, tc := range []struct {
		family adapter.Family
		want   string
	}{
		{adapter.FedoraAtomic, "rpm-ostree upgrade"},
		{adapter.Bootc, "bootc upgrade"},
	} {
		t.Run(string(tc.family), func(t *testing.T) {
			h := newHarnessWith(t, harnessOptions{family: tc.family, adapters: fedoraAdapters()}, patchTrainDrift())

			result, err := h.engine.Pass(context.Background())
			if err != nil {
				t.Fatalf("Pass: %v", err)
			}
			if len(result.Queued) != 1 || result.Queued[0] != "patch-train" {
				t.Fatalf("queued %v, want patch-train", result.Queued)
			}
			if len(h.runs) != 0 {
				t.Errorf("ran %v, want no stage command from lares", h.runs)
			}
			item, ok := h.queued(t)["patch-train"]
			if !ok {
				t.Fatal("patch-train not queued")
			}
			if item.Term != termNeedsGrant {
				t.Errorf("term %q, want %q", item.Term, termNeedsGrant)
			}
			verdict := newestVerdict(t, h.store, "patch-train")
			if verdict.Fix != tc.want {
				t.Errorf("queued command %q, want %q", verdict.Fix, tc.want)
			}
		})
	}
}

func TestPassReportsAStagedDeploymentLive(t *testing.T) {
	for _, family := range []adapter.Family{adapter.FedoraAtomic, adapter.Bootc} {
		t.Run(string(family), func(t *testing.T) {
			h := newHarnessWith(t, harnessOptions{
				family:   family,
				adapters: fedoraAdapters(),
				staged:   func(context.Context) (bool, error) { return true, nil },
			}, patchTrainDrift())

			result, err := h.engine.Pass(context.Background())
			if err != nil {
				t.Fatalf("Pass: %v", err)
			}
			if len(result.Queued) != 0 {
				t.Errorf("queued %v, want nothing after a staged deployment", result.Queued)
			}
			if _, ok := h.queued(t)["patch-train"]; ok {
				t.Error("patch-train stayed queued with a staged deployment")
			}
			verdict := newestVerdict(t, h.store, "patch-train")
			if verdict.State != string(contract.Live) {
				t.Errorf("state %q, want live", verdict.State)
			}
			if verdict.Fix != "" {
				t.Errorf("fix %q, want empty for a live check", verdict.Fix)
			}
			if !strings.Contains(verdict.Detail, "staged") {
				t.Errorf("detail %q does not name the staged deployment", verdict.Detail)
			}
		})
	}
}

func TestPassKeepsTheItemQueuedWhenTheStagedCheckFails(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{
		family:   adapter.FedoraAtomic,
		adapters: fedoraAdapters(),
		staged:   func(context.Context) (bool, error) { return false, errors.New("rpm-ostree is missing") },
	}, patchTrainDrift())

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	item, ok := h.queued(t)["patch-train"]
	if !ok {
		t.Fatal("patch-train not queued after a failed staged check")
	}
	if item.Term != termNeedsGrant {
		t.Errorf("term %q, want %q", item.Term, termNeedsGrant)
	}
	if item.Attempts != 0 {
		t.Errorf("attempts %d, want 0: a failed check must not escalate", item.Attempts)
	}
	verdict := newestVerdict(t, h.store, "patch-train")
	if !strings.Contains(verdict.Detail, "staged check failed") {
		t.Errorf("detail %q does not name the failed check", verdict.Detail)
	}
	if verdict.Fix != "rpm-ostree upgrade" {
		t.Errorf("queued command %q, want the stage argv", verdict.Fix)
	}
}

func TestPassLeavesTheArchPatchTrainRecordAlone(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{
		family: adapter.Arch,
		staged: func(context.Context) (bool, error) {
			t.Error("the staged check ran on Arch")
			return true, nil
		},
	}, patchTrainDrift())

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	verdict := newestVerdict(t, h.store, "patch-train")
	if verdict.Fix != "install/host-setup.sh --only patch-train --apply" {
		t.Errorf("fix %q, want the probe's repair", verdict.Fix)
	}
	if verdict.State != string(contract.NotLive) {
		t.Errorf("state %q, want not-live", verdict.State)
	}
}

func TestPassFailsWhenTheStageAdapterIsMissing(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{
		family:   adapter.FedoraAtomic,
		adapters: map[adapter.Family]adapter.Adapter{},
	}, patchTrainDrift())

	_, err := h.engine.Pass(context.Background())
	if err == nil {
		t.Fatal("a missing adapter section returned no error")
	}
	if !strings.Contains(err.Error(), "no section") {
		t.Errorf("error %q does not name the missing section", err)
	}
	if _, _, err := h.store.Queue(); !errors.Is(err, state.ErrNoQueue) {
		t.Errorf("failed pass wrote a queue: %v, want %v", err, state.ErrNoQueue)
	}
}

func TestPassNeedsNoStageAdapterWithoutAPatchTrainFinding(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{
		family:   adapter.FedoraAtomic,
		adapters: map[adapter.Family]adapter.Adapter{},
	}, []contract.Record{rec("profile", contract.Live, ""), summary(contract.Live)})

	if _, err := h.engine.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if items := h.queued(t); len(items) != 0 {
		t.Errorf("queue %v, want empty", items)
	}
}

func TestRpmOstreeStaged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		want    bool
		wantErr bool
	}{
		{"one staged deployment", `{"deployments":[{"booted":true,"staged":false},{"staged":true}]}`, true, false},
		{"no staged deployment", `{"deployments":[{"booted":true,"staged":false}]}`, false, false},
		{"no deployments", `{"deployments":[],"transaction":null}`, false, false},
		{"missing key", `{"transaction":null}`, false, true},
		{"invalid json", `not json`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rpmOstreeStaged([]byte(tc.data))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %v with no error, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("rpmOstreeStaged: %v", err)
			}
			if got != tc.want {
				t.Errorf("staged %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBootcStaged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		want    bool
		wantErr bool
	}{
		{"a staged image", `{"status":{"staged":{"image":{"version":"2"}},"booted":{"image":{}}}}`, true, false},
		{"an empty staged object", `{"status":{"staged":{},"booted":{"image":{}}}}`, true, false},
		{"null staged", `{"status":{"staged":null,"booted":{"image":{}}}}`, false, false},
		{"staged is a string", `{"status":{"staged":"yes"}}`, false, true},
		{"staged is a number", `{"status":{"staged":42}}`, false, true},
		{"staged is false", `{"status":{"staged":false}}`, false, true},
		{"staged is an array", `{"status":{"staged":[]}}`, false, true},
		{"missing status", `{"kind":"BootcHost"}`, false, true},
		{"missing staged key", `{"status":{"booted":{"image":{}}}}`, false, true},
		{"invalid json", `not json`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bootcStaged([]byte(tc.data))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %v with no error, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("bootcStaged: %v", err)
			}
			if got != tc.want {
				t.Errorf("staged %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDefaultStagedRunsTheFamilyCommand(t *testing.T) {
	bin := t.TempDir()
	fakeCommand(t, bin, "rpm-ostree", `echo '{"deployments":[{"staged":true}]}'`)
	fakeCommand(t, bin, "bootc", `echo '{"status":{"staged":{"image":{"version":"2"}}}}'`)
	t.Setenv("PATH", bin)

	for _, family := range []adapter.Family{adapter.FedoraAtomic, adapter.Bootc} {
		t.Run(string(family), func(t *testing.T) {
			staged, err := defaultStaged(context.Background(), family)
			if err != nil {
				t.Fatalf("defaultStaged: %v", err)
			}
			if !staged {
				t.Error("a staged deployment was not detected")
			}
		})
	}
}

func TestDefaultStagedFailsClosedWhenTheCommandFails(t *testing.T) {
	bin := t.TempDir()
	fakeCommand(t, bin, "rpm-ostree", "echo 'rpm-ostree is broken' >&2\nexit 1")
	t.Setenv("PATH", bin)

	staged, err := defaultStaged(context.Background(), adapter.FedoraAtomic)
	if err == nil {
		t.Fatal("a failed status command returned no error")
	}
	if staged {
		t.Error("a failed status command reported a staged deployment")
	}
	if !strings.Contains(err.Error(), "rpm-ostree status --json") {
		t.Errorf("error %q does not name the command", err)
	}
}

func TestDefaultStagedIsFalseForArch(t *testing.T) {
	// An empty PATH makes an accidental command run an error instead of a pass.
	t.Setenv("PATH", "")
	staged, err := defaultStaged(context.Background(), adapter.Arch)
	if err != nil {
		t.Fatalf("defaultStaged: %v", err)
	}
	if staged {
		t.Error("Arch reported a staged deployment")
	}
}
