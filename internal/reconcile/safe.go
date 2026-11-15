package reconcile

// Term names a queue item's reason. A term is data, never prose, so the TUI and
// the CLI can group by it.
const (
	// termNeedsOperator marks work only a person may start.
	termNeedsOperator = "needs-operator"
	// termNeedsGrant marks work that needs a privilege the host has not granted.
	termNeedsGrant = "needs-grant"
	// termNeedsTTY marks work that opens /dev/tty for sudo.
	termNeedsTTY = "needs-tty"
	// termHold marks work held back by a live hold.
	termHold = "hold"
	// termUnverified marks a measurement that proves nothing either way.
	termUnverified = "unverified"
	// termDidNotHold marks a repair whose verification saw no movement.
	termDidNotHold = "fix-did-not-hold"
	// termBackoff marks a due item that is waiting out its retry delay.
	termBackoff = "backoff"
	// termDeferred marks an operator's deferral.
	termDeferred = "deferred"
)

// safeToConverge reports whether id may run without a person.
//
// Only the profile selection qualifies. It rewrites one file in the home
// directory and cannot prompt. Every other repair in host-setup.sh either opens
// /dev/tty for sudo or writes to a remote, so it goes to the queue instead of
// running unattended.
func safeToConverge(id string) bool {
	return id == "profile"
}

// repairTerm names the precondition that keeps id out of the safe set.
func repairTerm(id string) string {
	switch id {
	case "maintenance", "hermes":
		return termNeedsTTY
	case "patch-train":
		return termNeedsGrant
	default:
		return termNeedsOperator
	}
}
