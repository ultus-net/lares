// Command lares measures a host, converges the repairs that are safe to repeat,
// and queues the rest for a person.
//
// Exit codes:
//
//	0  success, and no finding
//	1  a measure or pass reported a finding (not-live or stale)
//	2  usage error
//	3  runtime error
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ultus-net/lares/internal/adapter"
	"github.com/ultus-net/lares/internal/config"
	"github.com/ultus-net/lares/internal/contract"
	"github.com/ultus-net/lares/internal/install"
	"github.com/ultus-net/lares/internal/reconcile"
	"github.com/ultus-net/lares/internal/state"
	"github.com/ultus-net/lares/internal/tui"
)

// version is set at link time with -X main.version=<value>.
var version = "dev"

// usage prints the command list.
func usage(w *os.File) {
	fmt.Fprintf(w, `lares %s -- measure a host, converge what is safe, queue the rest

Usage:
  lares <command> [flags]

Commands:
  measure [--json]           measure once, print records, append verdicts
  pass                       one reconcile pass, including the reboot gate
  status [--json]            queue, holds, and the newest verdict per item
  hold add [--pid N] <reason>  write a hold for pid N, or for this process
  hold exec -- <command...>  hold while a command runs, then release it
  hold list                  list holds, marking stale ones
  hold rm <owner>            remove a hold
  defer <id> <duration>      keep an item out of the queue for a while
  reboot [--dry-run]         evaluate the reboot gate
  install --user             write the binary, units, and config
  tui                        the punch list
  version                    print the version

Flags:
  --config <path>   config file (default $XDG_CONFIG_HOME/lares/config.toml)
`, version)
}

func main() {
	configFlag := flag.String("config", "", "config file path")
	versionFlag := flag.Bool("version", false, "print the version")

	flag.Usage = func() { usage(os.Stderr) }
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return
	}
	if flag.NArg() < 1 {
		usage(os.Stderr)
		os.Exit(2)
	}

	a := &app{configPath: *configFlag}

	ctx := context.Background()
	switch cmd := flag.Arg(0); cmd {
	case "measure":
		a.measure(ctx, flag.Args()[1:])
	case "pass":
		a.pass(ctx)
	case "status":
		a.status(flag.Args()[1:])
	case "hold":
		a.hold(flag.Args()[1:])
	case "defer":
		a.deferItem(flag.Args()[1:])
	case "reboot":
		a.reboot(ctx, flag.Args()[1:])
	case "install":
		a.install(flag.Args()[1:])
	case "tui":
		a.runTUI()
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "lares: unknown command %q\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

// app carries the parsed global flags between commands.
type app struct {
	configPath string

	loaded bool
	cfg    config.Config
	store  *state.Store
	family adapter.Family
	table  map[adapter.Family]adapter.Adapter
}

// load reads the config, opens the store, detects the family, and reads the
// adapter table. A missing adapters.toml is not an error: an unknown host
// measures and never acts.
func (a *app) load() error {
	if a.loaded {
		return nil
	}
	path := a.configPath
	if path == "" {
		var err error
		path, err = config.DefaultConfigPath()
		if err != nil {
			return fmt.Errorf("locate config: %w", err)
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	a.cfg = cfg

	store, err := state.Open(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	a.store = store

	family, err := adapter.DetectHost()
	if err != nil {
		return fmt.Errorf("detect distro family: %w", err)
	}
	a.family = family

	table, err := adapter.Load(filepath.Join(cfg.ConfigDir, "adapters.toml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load adapters: %w", err)
	}
	if table == nil {
		table = map[adapter.Family]adapter.Adapter{}
	}
	a.table = table

	a.loaded = true
	return nil
}

// engine builds a reconcile engine over the loaded state.
func (a *app) engine() (*reconcile.Engine, error) {
	if err := a.load(); err != nil {
		return nil, err
	}
	return reconcile.New(reconcile.Options{
		Config:   a.cfg,
		Store:    a.store,
		Family:   a.family,
		Adapters: a.table,
	})
}

// measure runs one measurement, prints every record, and appends a verdict per
// non-summary record.
func (a *app) measure(ctx context.Context, args []string) {
	fset := flag.NewFlagSet("measure", flag.ExitOnError)
	asJSON := fset.Bool("json", false, "print records as JSON")
	if err := fset.Parse(args); err != nil {
		os.Exit(2)
	}
	engine, err := a.engine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "measure: %v\n", err)
		os.Exit(3)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var records []contract.Record
	err = a.withLock(func() error {
		var err error
		records, err = engine.Measure(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		for _, rec := range records {
			if rec.IsSummary() {
				continue
			}
			verdict := state.Verdict{ID: rec.ID, State: string(rec.State), Detail: rec.Detail, Fix: rec.Fix, At: now}
			if err := a.store.AppendVerdict(verdict); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "measure: %v\n", err)
		os.Exit(3)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		for _, rec := range records {
			if err := enc.Encode(rec); err != nil {
				fmt.Fprintf(os.Stderr, "measure: %v\n", err)
				os.Exit(3)
			}
		}
	} else {
		for _, rec := range records {
			fmt.Printf("%-16s %-9s %s\n", rec.ID, rec.State, rec.Detail)
		}
	}

	if anyFinding(records) {
		os.Exit(1)
	}
}

// printReboot reports the reboot gate's verdict when a pass evaluated it.
func printReboot(verdict reconcile.RebootVerdict) {
	if verdict.Term == "" && !verdict.Allowed {
		return // the gate did not run
	}
	if verdict.Allowed {
		fmt.Println("reboot: activated")
		return
	}
	fmt.Printf("reboot: blocked on %s", verdict.Term)
	if verdict.HoldAge > 0 {
		fmt.Printf(" (oldest hold %s)", verdict.HoldAge.Round(time.Minute))
	}
	if len(verdict.Unclaimed) > 0 {
		fmt.Printf(" unclaimed=%s", strings.Join(verdict.Unclaimed, ","))
	}
	fmt.Println()
}

// anyFinding reports whether a non-summary record is not-live or stale.
func anyFinding(records []contract.Record) bool {
	for _, rec := range records {
		if !rec.IsSummary() && rec.IsFinding() {
			return true
		}
	}
	return false
}

// pass runs one reconcile pass.
func (a *app) pass(ctx context.Context) {
	engine, err := a.engine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pass: %v\n", err)
		os.Exit(3)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	result, err := engine.Pass(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pass: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("converged=%d queued=%d escalated=%d waiting=%d\n",
		len(result.Converged), len(result.Queued), len(result.Escalated), len(result.Skipped))
	printReboot(result.Reboot)
	for _, notice := range result.Notices {
		fmt.Println(notice)
	}
	for _, failure := range result.NotifyFailed {
		fmt.Fprintf(os.Stderr, "notify: %s\n", failure)
	}
	if anyFinding(result.Measured) {
		os.Exit(1)
	}
}

// status prints the queue, the holds, and the newest verdict per item.
func (a *app) status(args []string) {
	fset := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fset.Bool("json", false, "print status as JSON")
	if err := fset.Parse(args); err != nil {
		os.Exit(2)
	}
	engine, err := a.engine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(3)
	}
	status, err := engine.Status()
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(3)
	}

	if *asJSON {
		payload := struct {
			Queue    state.Queue              `json:"queue"`
			Holds    []state.Hold             `json:"holds"`
			Verdicts map[string]state.Verdict `json:"verdicts"`
		}{Queue: status.Queue, Holds: status.Holds, Verdicts: status.Verdicts}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(os.Stderr, "status: %v\n", err)
			os.Exit(3)
		}
		return
	}

	fmt.Println("Queue:")
	for _, item := range status.Queue.Items {
		next := "now"
		if !item.NextAttemptAt.IsZero() {
			next = item.NextAttemptAt.Format(time.RFC3339)
		}
		fmt.Printf("  %s  term=%s  attempts=%d  next=%s\n", item.ID, item.Term, item.Attempts, next)
	}
	fmt.Println("Holds:")
	for _, hold := range status.Holds {
		fmt.Printf("  %s  %s  pid=%d\n", hold.Owner, hold.Reason, hold.PID)
	}
	fmt.Println("Verdicts:")
	for id, verdict := range status.Verdicts {
		fmt.Printf("  %-16s %-9s %s\n", id, verdict.State, verdict.Detail)
	}
}

// hold handles `hold add`, `hold exec`, `hold list`, and `hold rm`.
func (a *app) hold(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hold: expected add, exec, list, or rm")
		os.Exit(2)
	}
	if args[0] == "exec" {
		a.holdExec(args[1:])
		return
	}
	if err := a.load(); err != nil {
		fmt.Fprintf(os.Stderr, "hold: %v\n", err)
		os.Exit(3)
	}
	switch args[0] {
	case "add":
		a.holdAdd(args[1:])
	case "list":
		holds, err := a.store.HoldsIncludingStale()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hold list: %v\n", err)
			os.Exit(3)
		}
		for _, hold := range holds {
			mark := ""
			if hold.Stale {
				mark = " (stale)"
			}
			fmt.Printf("%s  %s  pid=%d%s\n", hold.Owner, hold.Reason, hold.PID, mark)
		}
	case "rm":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "hold rm: expected an owner")
			os.Exit(2)
		}
		owner := args[1]
		if err := a.withLock(func() error { return a.store.RemoveHold(owner) }); err != nil {
			fmt.Fprintf(os.Stderr, "hold rm: %v\n", err)
			os.Exit(3)
		}
	default:
		fmt.Fprintf(os.Stderr, "hold: unknown action %q\n", args[0])
		os.Exit(2)
	}
}

// holdAdd writes a hold for the pid named by --pid, or for this process.
func (a *app) holdAdd(args []string) {
	fset := flag.NewFlagSet("hold add", flag.ExitOnError)
	pid := fset.Int("pid", 0, "pid to hold, default this process")
	if err := fset.Parse(args); err != nil {
		os.Exit(2)
	}
	reason := strings.TrimSpace(strings.Join(fset.Args(), " "))
	if reason == "" {
		fmt.Fprintln(os.Stderr, "hold add: expected a reason")
		os.Exit(2)
	}
	if *pid == 0 {
		*pid = os.Getpid()
	}
	if !state.ProcessAlive(*pid) {
		fmt.Fprintf(os.Stderr, "hold add: pid %d is not running\n", *pid)
		os.Exit(2)
	}
	ticks, ok := state.ProcessStartTicks(*pid)
	if !ok {
		// Without a start time the hold still names the pid. Report it, because
		// a reused pid could hold the host longer than intended.
		fmt.Fprintf(os.Stderr, "hold add: cannot read the start time of pid %d, holding by pid only\n", *pid)
	}
	hold := state.Hold{
		Owner:      fmt.Sprintf("pid-%d", *pid),
		Reason:     reason,
		PID:        *pid,
		StartTicks: ticks,
		Start:      time.Now(),
	}
	if err := a.withLock(func() error { return a.store.AddHold(hold) }); err != nil {
		fmt.Fprintf(os.Stderr, "hold add: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("hold %s: %s\n", hold.Owner, hold.Reason)
}

// startProcess starts cmd. Tests replace it to observe the lock ordering.
var startProcess = func(cmd *exec.Cmd) error { return cmd.Start() }

// waitProcess reaps cmd. Tests replace it to observe the lock state at the reap.
var waitProcess = func(cmd *exec.Cmd) error { return cmd.Wait() }

// startUnderLock starts cmd while the state lock is held, then publishes the
// hold that names the started pid, and only then releases the lock.
//
// A pass that holds the lock therefore never runs between the command's start
// and its hold, and a pass already running makes this refuse before the command
// exists. A command whose hold cannot be written is killed under the same lock,
// so no pass can observe it running unheld. The kill is signalled before the
// unlock and the corpse is reaped after it, because a process stuck in
// uninterruptible I/O can delay the reap and would otherwise stall every lock
// user.
func (a *app) startUnderLock(cmd *exec.Cmd, reason string) (state.Hold, error) {
	if err := a.store.Lock(); err != nil {
		return state.Hold{}, err
	}
	if err := startProcess(cmd); err != nil {
		_ = a.store.Unlock()
		return state.Hold{}, err
	}
	owner := fmt.Sprintf("pid-%d", cmd.Process.Pid)
	ticks, _ := state.ProcessStartTicks(cmd.Process.Pid)
	hold := state.Hold{Owner: owner, Reason: reason, PID: cmd.Process.Pid, StartTicks: ticks, Start: time.Now()}
	addErr := a.store.AddHold(hold)
	if addErr != nil {
		_ = cmd.Process.Kill()
	}
	_ = a.store.Unlock()
	if addErr != nil {
		_ = waitProcess(cmd)
	}
	return hold, addErr
}

// holdExec runs a command under a hold that names the command's pid.
//
// The hold names the child rather than lares, so a lares crash still leaves the
// running command protected. The hold is removed when the command exits.
func (a *app) holdExec(args []string) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hold exec: expected -- <command...>")
		os.Exit(2)
	}
	if err := a.load(); err != nil {
		fmt.Fprintf(os.Stderr, "hold exec: %v\n", err)
		os.Exit(3)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	hold, err := a.startUnderLock(cmd, strings.Join(args, " "))
	if err != nil {
		fmt.Fprintf(os.Stderr, "hold exec: %v\n", err)
		os.Exit(3)
	}
	err = cmd.Wait()
	if removeErr := a.withLock(func() error { return a.store.RemoveHold(hold.Owner) }); removeErr != nil {
		fmt.Fprintf(os.Stderr, "hold exec: release hold: %v\n", removeErr)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "hold exec: %v\n", err)
		os.Exit(3)
	}
}

// withLock runs fn under the state lock, so it never interleaves with a pass.
func (a *app) withLock(fn func() error) error {
	if err := a.store.Lock(); err != nil {
		return err
	}
	defer a.store.Unlock()
	return fn()
}

// deferItem moves an item's next attempt out by a duration.
func (a *app) deferItem(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "defer: expected <id> <duration>")
		os.Exit(2)
	}
	d, err := time.ParseDuration(args[1])
	if err != nil || d < time.Minute {
		fmt.Fprintf(os.Stderr, "defer: %q is not a duration of at least 1m\n", args[1])
		os.Exit(2)
	}
	engine, err := a.engine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "defer: %v\n", err)
		os.Exit(3)
	}
	id := args[0]
	until, err := engine.Defer(id, d, "deferred from the CLI")
	if err != nil {
		fmt.Fprintf(os.Stderr, "defer: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("deferred %s until %s\n", id, until.Format(time.RFC3339))
}

// reboot evaluates the gate, and activates a reboot unless --dry-run.
func (a *app) reboot(ctx context.Context, args []string) {
	fset := flag.NewFlagSet("reboot", flag.ExitOnError)
	dryRun := fset.Bool("dry-run", false, "print the verdict without acting")
	if err := fset.Parse(args); err != nil {
		os.Exit(2)
	}
	engine, err := a.engine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reboot: %v\n", err)
		os.Exit(3)
	}

	var verdict reconcile.RebootVerdict
	if *dryRun {
		verdict, err = engine.RebootAllowed(ctx)
	} else {
		verdict, err = engine.Reboot(ctx)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "reboot: %v\n", err)
		os.Exit(3)
	}
	if verdict.Allowed {
		if *dryRun {
			fmt.Println("allowed")
		} else {
			fmt.Println("activated")
		}
		return
	}
	fmt.Printf("blocked: %s\n", verdict.Term)
	if len(verdict.Unclaimed) > 0 {
		fmt.Printf("unclaimed: %s\n", strings.Join(verdict.Unclaimed, ", "))
	}
}

// install writes the binary, units, and config for the current user.
func (a *app) install(args []string) {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	user := fset.Bool("user", false, "install for the current user")
	if err := fset.Parse(args); err != nil {
		os.Exit(2)
	}
	if !*user {
		fmt.Fprintln(os.Stderr, "install: only --user is supported")
		os.Exit(2)
	}
	if err := a.load(); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(3)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(3)
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(3)
	}
	cfg := a.cfg
	written, err := install.Install(install.Options{
		HomeDir:    home,
		BinaryPath: self,
		Config:     cfg,
		Adapters:   a.table,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(3)
	}
	for _, path := range written {
		fmt.Println(path)
	}
	fmt.Println("\nRun this once, by hand. lares never enables linger itself:")
	for _, step := range install.NextSteps() {
		fmt.Printf("  %s\n", step)
	}
	fmt.Println("\nThen enable the timer:")
	fmt.Println("  systemctl --user daemon-reload")
	fmt.Println("  systemctl --user enable --now lares.timer")
}

// runTUI starts the punch list.
func (a *app) runTUI() {
	engine, err := a.engine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(3)
	}
	model, err := tui.New(tui.Options{Engine: engine, Store: a.store})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(3)
	}
	if err := model.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		os.Exit(3)
	}
}
