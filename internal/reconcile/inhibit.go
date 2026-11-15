package reconcile

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// inhibitList reads the active inhibitor locks.
//
// --no-pager keeps systemd from opening a pager, and --no-legend drops the
// header and the footer. The output carries one line per lock.
func inhibitList(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "systemd-inhibit", "--list", "--no-legend", "--no-pager")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("systemd-inhibit: %w: %s", err, msg)
		}
		return "", fmt.Errorf("systemd-inhibit: %w", err)
	}
	return string(out), nil
}

// blockedByInhibitor reports whether a lock asks the host to stay up.
//
// The columns are WHO UID USER PID COMM WHAT WHY MODE. WHY may be empty or hold
// spaces, so the parser reads MODE from the end and checks the UID and PID
// columns to catch a layout it does not understand. A block-weak lock counts as
// well: it is still a program asking not to be interrupted. An unparsable line
// is an error, so the gate blocks instead of guessing that the host is idle.
func blockedByInhibitor(output string) (bool, error) {
	for i, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			return false, fmt.Errorf("inhibitor line %d: %d fields, want at least 7", i+1, len(fields))
		}
		if _, err := strconv.Atoi(fields[1]); err != nil {
			return false, fmt.Errorf("inhibitor line %d: uid column %q is not a number", i+1, fields[1])
		}
		if _, err := strconv.Atoi(fields[3]); err != nil {
			return false, fmt.Errorf("inhibitor line %d: pid column %q is not a number", i+1, fields[3])
		}
		switch mode := fields[len(fields)-1]; mode {
		case "block", "block-weak":
			return true, nil
		case "delay":
			continue
		default:
			return false, fmt.Errorf("inhibitor line %d: unknown mode %q", i+1, mode)
		}
	}
	return false, nil
}
