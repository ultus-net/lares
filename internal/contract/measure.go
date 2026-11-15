package contract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var errEmptyArgv = errors.New("probe command is empty")

// Measure runs the probe with --json appended and parses what it prints.
//
// It captures stdout and stderr separately. A non-zero exit is an error naming
// the code and the last non-empty stderr line. Cancelling ctx kills the process
// and the returned error matches errors.Is against ctx.Err().
func Measure(ctx context.Context, argv []string) ([]Record, error) {
	return MeasureIn(ctx, "", argv)
}

// MeasureIn is Measure with a working directory for the probe. The dotfiles
// probe resolves its own paths from the checkout root, so a caller that does
// not run there passes the root as dir. An empty dir means the current one.
func MeasureIn(ctx context.Context, dir string, argv []string) ([]Record, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errEmptyArgv
	}
	args := make([]string, 0, len(argv))
	args = append(args, argv[1:]...)
	args = append(args, "--json")

	cmd := exec.CommandContext(ctx, argv[0], args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("run %s: %w", argv[0], ctxErr)
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			msg := lastNonEmptyLine(stderr.String())
			if msg == "" {
				return nil, fmt.Errorf("run %s: exit %d", argv[0], exit.ExitCode())
			}
			return nil, fmt.Errorf("run %s: exit %d: %s", argv[0], exit.ExitCode(), msg)
		}
		return nil, fmt.Errorf("run %s: %w", argv[0], err)
	}
	return Parse(&stdout)
}

// lastNonEmptyLine returns the last line of s that is not blank.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
