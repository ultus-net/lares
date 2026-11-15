package reconcile

// safeIDs is the v1 safe-converge set.
//
// These four workstreams converge through one idempotent command each, and none
// can prompt. `patch-train` is excluded on purpose: its timer needs a widened
// sudo grant first, so enabling it would buy a unit that fails every night.
// `offsite` needs a provider decision, `backup` needs a mount and a password,
// and branch cleanup needs a per-branch judgement. Those queue.
var safeIDs = map[string]bool{
	"hermes":      true,
	"maintenance": true,
	"notebook":    true,
	"profile":     true,
}

// safeToConverge reports whether id is in the v1 safe set.
func safeToConverge(id string) bool {
	return safeIDs[id]
}
