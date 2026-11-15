package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inhibitorFixture is real output from systemd-inhibit --list --no-legend
// captured on 2026-11-16 from systemd 262 (262.1-1-arch). The last line comes
// from this command on the same host, so the fixture holds both a delay and a
// block lock:
//
//	systemd-inhibit --what=sleep --who=lares-fixture --why="block fixture" --mode=block sleep 3
const inhibitorFixture = `NetworkManager 0    root 1046    NetworkManager  sleep NetworkManager needs to turn off networks delay
UPower         0    root 1455    upowerd         sleep Pause device polling                      delay
Omarchy        1000 csh  1502    systemd-inhibit sleep Lock screen before suspend                delay
lares-fixture  1000 csh  1466952 systemd-inhibit sleep block fixture                             block
`

// delayFixture holds the fixture's delay locks without the block lock.
var delayFixture = strings.Join(strings.Split(inhibitorFixture, "\n")[:3], "\n") + "\n"

func TestBlockedByInhibitor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{"empty list", "", false},
		{"blank lines", "\n\n", false},
		{"delay locks only", delayFixture, false},
		{"a block lock", inhibitorFixture, true},
		{"a block-weak lock", "who 0 root 900 comm sleep idle reason block-weak\n", true},
		{"a block lock with no reason", "who 0 root 900 comm sleep  block\n", true},
		{"block last after a delay", delayFixture + "who 0 root 900 comm sleep reason block\n", true},
		{"delay last after a block", "who 0 root 900 comm sleep reason block\n" + delayFixture, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := blockedByInhibitor(tc.output)
			if err != nil {
				t.Fatalf("blockedByInhibitor: %v", err)
			}
			if got != tc.want {
				t.Errorf("blocked %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBlockedByInhibitorRejectsUnparsableLines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{"too few columns", "who 0 root 900 comm sleep\n", "want at least 7"},
		{"uid is not a number", "who notauid root 900 comm sleep reason delay\n", "uid column"},
		{"pid is not a number", "who 0 root notapid comm sleep reason delay\n", "is not a number"},
		{"header line", "WHO UID USER PID COMM WHAT WHY MODE\n", "is not a number"},
		{"extra leading field", "x who 0 root 900 comm sleep reason delay\n", "is not a number"},
		{"extra trailing field", "who 0 root 900 comm sleep reason delay extra\n", "unknown mode"},
		{"unknown mode", "who 0 root 900 comm sleep reason sometimes\n", "unknown mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocked, err := blockedByInhibitor(tc.output)
			if err == nil {
				t.Fatalf("blocked %v with no error; an unreadable list must fail closed", blocked)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// fakeCommand writes an executable command named name into dir.
//
// The gate tests put dir on PATH, so defaultSessionActive runs a scripted
// systemd-inhibit and loginctl instead of the host's own and the real command
// path is exercised.
func fakeCommand(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// fakeList writes a fake systemd-inhibit that prints output.
//
// The body uses the shell's echo builtin, so the script runs with a PATH that
// holds only the fake commands.
func fakeList(t *testing.T, dir, output string) {
	t.Helper()
	var body strings.Builder
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		body.WriteString("echo '" + line + "'\n")
	}
	fakeCommand(t, dir, "systemd-inhibit", body.String())
}

// gateEnv points the gate at a temp home and a temp bin directory.
func gateEnv(t *testing.T, binDir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", binDir)
}

func TestSessionActiveOnABlockInhibitor(t *testing.T) {
	bin := t.TempDir()
	fakeList(t, bin, inhibitorFixture)
	// If the gate stops reading inhibitors it reaches this and fails the test.
	fakeCommand(t, bin, "loginctl", "exit 1")
	gateEnv(t, bin)

	active, err := defaultSessionActive(context.Background())
	if err != nil {
		t.Fatalf("defaultSessionActive: %v", err)
	}
	if !active {
		t.Error("a block inhibitor was not treated as a busy host")
	}
}

func TestSessionActiveIgnoresDelayLocks(t *testing.T) {
	bin := t.TempDir()
	fakeList(t, bin, delayFixture)
	fakeCommand(t, bin, "loginctl", "exit 0")
	gateEnv(t, bin)

	active, err := defaultSessionActive(context.Background())
	if err != nil {
		t.Fatalf("defaultSessionActive: %v", err)
	}
	if active {
		t.Error("a delay-only lock was treated as a busy host")
	}
}

func TestSessionActiveFailsClosedWhenInhibitorsCannotBeRead(t *testing.T) {
	bin := t.TempDir()
	fakeCommand(t, bin, "systemd-inhibit", "echo 'systemd-inhibit is missing' >&2\nexit 1")
	gateEnv(t, bin)

	active, err := defaultSessionActive(context.Background())
	if err == nil {
		t.Fatal("a failed inhibitor read returned no error")
	}
	if !strings.Contains(err.Error(), "systemd-inhibit") {
		t.Errorf("error %q does not name the failed read", err)
	}
	if !active {
		t.Error("a failed inhibitor read reported the host as idle")
	}
}

func TestSessionActiveFailsClosedOnAnUnparsableLine(t *testing.T) {
	bin := t.TempDir()
	fakeCommand(t, bin, "systemd-inhibit", "echo 'WHO UID USER PID COMM WHAT WHY MODE'")
	gateEnv(t, bin)
	active, err := defaultSessionActive(context.Background())
	if err == nil {
		t.Fatal("an unparsable inhibitor line returned no error")
	}
	if !active {
		t.Error("an unparsable inhibitor line reported the host as idle")
	}
}
