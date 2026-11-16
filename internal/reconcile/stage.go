package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/contract"
)

// stageRecords rewrites the patch-train record on a family whose update is
// staged by an operator command rather than by the Arch patch-train timer.
//
// On Fedora atomic and bootc nothing stages an update on its own: the stage verb
// needs root, and lares holds no grant. The queue therefore names the adapter's
// stage argv, and the operator runs it. When a deployment is already staged, the
// record reports live with its detail replaced, which clears the queue item.
//
// A failed staged check keeps the record queued and names the error in the
// detail. The item is safe to leave queued; claiming a staged deployment that
// cannot be seen would be the unsafe direction.
//
// A family that resolves no adapter section is an error, so the pass fails
// loudly instead of queueing a repair with no command. The section is resolved
// only when a patch-train finding exists, so a converged Fedora host does not
// depend on the adapter table.
func (e *Engine) stageRecords(ctx context.Context, records []contract.Record) ([]contract.Record, error) {
	if e.family != adapter.FedoraAtomic && e.family != adapter.Bootc {
		return records, nil
	}
	wantsStage := false
	for _, rec := range records {
		if rec.ID == "patch-train" && rec.NeedsAttention() {
			wantsStage = true
			break
		}
	}
	if !wantsStage {
		return records, nil
	}
	entry, err := adapter.Select(e.family, e.cfg.AdapterOverride, e.adapters)
	if err != nil {
		return nil, err
	}
	out := make([]contract.Record, len(records))
	copy(out, records)

	var staged bool
	var stagedErr error
	checked := false
	for i, rec := range out {
		if rec.ID != "patch-train" || !rec.NeedsAttention() {
			continue
		}
		rec.Fix = strings.Join(entry.Stage, " ")
		if !checked {
			staged, stagedErr = e.staged(ctx)
			checked = true
		}
		if stagedErr != nil {
			rec.Detail = fmt.Sprintf("%s (staged check failed: %v)", rec.Detail, stagedErr)
			out[i] = rec
			continue
		}
		if staged {
			rec.State = contract.Live
			rec.Detail = "staged deployment waits for reboot"
			rec.Fix = ""
		}
		out[i] = rec
	}
	return out, nil
}

// defaultStaged reports whether a deployment is staged for the next boot.
//
// The check is family-specific and read-only. Arch returns false because its
// patch-train timer owns staging.
func defaultStaged(ctx context.Context, family adapter.Family) (bool, error) {
	var argv []string
	switch family {
	case adapter.FedoraAtomic:
		argv = []string{"rpm-ostree", "status", "--json"}
	case adapter.Bootc:
		argv = []string{"bootc", "status", "--json"}
	default:
		return false, nil
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return false, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, msg)
		}
		return false, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	if family == adapter.Bootc {
		return bootcStaged(out)
	}
	return rpmOstreeStaged(out)
}

// rpmOstreeStaged reads the deployments array and reports one marked staged.
//
// A document without a deployments key is a layout this parser does not know, so
// it is an error rather than an idle answer.
func rpmOstreeStaged(data []byte) (bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("rpm-ostree status: %w", err)
	}
	raw, ok := doc["deployments"]
	if !ok {
		return false, fmt.Errorf("rpm-ostree status: no deployments key")
	}
	var deployments []struct {
		Staged bool `json:"staged"`
	}
	if err := json.Unmarshal(raw, &deployments); err != nil {
		return false, fmt.Errorf("rpm-ostree status: deployments: %w", err)
	}
	for _, deployment := range deployments {
		if deployment.Staged {
			return true, nil
		}
	}
	return false, nil
}

// bootcStaged reads status.staged. A non-null value names a deployment waiting
// for the next boot. A document without the key is an error, not an idle answer.
func bootcStaged(data []byte) (bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("bootc status: %w", err)
	}
	raw, ok := doc["status"]
	if !ok {
		return false, fmt.Errorf("bootc status: no status key")
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(raw, &status); err != nil {
		return false, fmt.Errorf("bootc status: status: %w", err)
	}
	staged, ok := status["staged"]
	if !ok {
		return false, fmt.Errorf("bootc status: no staged key")
	}
	value := bytes.TrimSpace(staged)
	if bytes.Equal(value, []byte("null")) {
		return false, nil
	}
	if len(value) == 0 || value[0] != '{' {
		return false, fmt.Errorf("bootc status: staged is not an object: %s", value)
	}
	return true, nil
}
