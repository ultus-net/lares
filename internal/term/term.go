// Package term names the reason an item waits.
//
// A term is data, never prose, so the CLI and the TUI group by it and a
// measurement can name a precondition without importing the queue. The
// reconciler and the measure engine share these constants, so a new
// precondition is one edit.
package term

const (
	// NeedsOperator marks work only a person may start.
	NeedsOperator = "needs-operator"
	// NeedsGrant marks work that needs a privilege the host has not granted.
	NeedsGrant = "needs-grant"
	// NeedsTTY marks work that opens /dev/tty for sudo.
	NeedsTTY = "needs-tty"
	// Hold marks work held back by a live hold.
	Hold = "hold"
	// Scan marks work held back because no process could be checked, so an
	// agent may be running unclaimed. It is distinct from a hold the operator
	// wrote, because the fix is a different one.
	Scan = "scan"
	// Unverified marks a measurement that proves nothing either way.
	Unverified = "unverified"
	// DidNotHold marks a repair whose verification saw no movement.
	DidNotHold = "fix-did-not-hold"
	// Backoff marks a due item that is waiting out its retry delay.
	Backoff = "backoff"
	// Deferred marks an operator's deferral.
	Deferred = "deferred"
	// Escalated marks an unpushed backup that survived the retry threshold. It
	// outranks the rest of the queue because a single-disk host has no other
	// copy of the checkout.
	Escalated = "escalated"
)
