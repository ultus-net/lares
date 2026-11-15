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
  pass                       one reconcile pass
  status [--json]            queue, holds, and the newest verdict per item
  hold add <reason>          write a hold owned by pid-<pid>
  hold list                  list holds, marking stale ones
  hold rm <owner>            remove a hold
  defer <id> <duration>      keep an item out of the queue for a while
  reboot [--dry-run]         evaluate the reboot gate
  install --user             write the binary, units, and config
  tui                        the punch list
  version                    print the version

Flags:
  --repo <dir>      dotfiles checkout holding the probe (also LARES_REPO)
  --config <path>   config file (default $XDG_CONFIG_HOME/lares/config.toml)
`, version)
}

func main() {
	repoFlag := flag.String("repo", "", "dotfiles checkout holding the probe (also LARES_REPO)")
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

	dir := *repoFlag
	if dir == "" {
		dir = os.Getenv("LARES_REPO")
	}
	a := &app{repoDir: dir, configPath: *configFlag}

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
	repoDir    string
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

	if a.repoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("read working directory: %w", err)
		}
		a.repoDir = wd
	}

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

// probe returns the audit argv, with a relative script anchored at the repo.
func (a *app) probe() []string {
	argv := append([]string(nil), a.cfg.AuditCommand...)
	if len(argv) > 1 && !filepath.IsAbs(argv[1]) && a.repoDir != "" {
		argv[1] = filepath.Join(a.repoDir, argv[1])
	}
	return argv
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
		RepoDir:  a.repoDir,
	})
}

// measure runs the probe once, prints every record, and appends a verdict per
// non-summary record.
func (a *app) measure(ctx context.Context, args []string) {
	fset := flag.NewFlagSet("measure", flag.ExitOnError)
	asJSON := fset.Bool("json", false, "print records as JSON")
	if err := fset.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := a.load(); err != nil {
		fmt.Fprintf(os.Stderr, "measure: %v\n", err)
		os.Exit(3)
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	records, err := contract.MeasureIn(ctx, a.repoDir, a.probe())
	if err != nil {
		fmt.Fprintf(os.Stderr, "measure: %v\n", err)
		os.Exit(3)
	}

	now := time.Now()
	for _, rec := range records {
		if rec.IsSummary() {
			continue
		}
		verdict := state.Verdict{ID: rec.ID, State: string(rec.State), Detail: rec.Detail, Fix: rec.Fix, At: now}
		if err := a.store.AppendVerdict(verdict); err != nil {
			fmt.Fprintf(os.Stderr, "measure: %v\n", err)
			os.Exit(3)
		}
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

// hold handles `hold add`, `hold list`, and `hold rm`.
func (a *app) hold(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "hold: expected add, list, or rm")
		os.Exit(2)
	}
	if err := a.load(); err != nil {
		fmt.Fprintf(os.Stderr, "hold: %v\n", err)
		os.Exit(3)
	}
	switch args[0] {
	case "add":
		reason := strings.TrimSpace(strings.Join(args[1:], " "))
		if reason == "" {
			fmt.Fprintln(os.Stderr, "hold add: expected a reason")
			os.Exit(2)
		}
		hold := state.Hold{
			Owner:  fmt.Sprintf("pid-%d", os.Getpid()),
			Reason: reason,
			PID:    os.Getpid(),
			Start:  time.Now(),
		}
		if err := a.store.AddHold(hold); err != nil {
			fmt.Fprintf(os.Stderr, "hold add: %v\n", err)
			os.Exit(3)
		}
		fmt.Printf("hold %s: %s\n", hold.Owner, hold.Reason)
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
		if err := a.store.RemoveHold(args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "hold rm: %v\n", err)
			os.Exit(3)
		}
	default:
		fmt.Fprintf(os.Stderr, "hold: unknown action %q\n", args[0])
		os.Exit(2)
	}
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
	if err := a.load(); err != nil {
		fmt.Fprintf(os.Stderr, "defer: %v\n", err)
		os.Exit(3)
	}

	id := args[0]
	queue, _, err := a.store.Queue()
	if err != nil && !errors.Is(err, state.ErrNoQueue) {
		fmt.Fprintf(os.Stderr, "defer: %v\n", err)
		os.Exit(3)
	}
	now := time.Now()
	found := false
	for i, item := range queue.Items {
		if item.ID != id {
			continue
		}
		queue.Items[i].NextAttemptAt = now.Add(d)
		queue.Items[i].Term = "deferred"
		found = true
	}
	if !found {
		queue.Items = append(queue.Items, state.Item{
			ID:            id,
			Term:          "deferred",
			FirstSeen:     now,
			NextAttemptAt: now.Add(d),
		})
	}
	if _, err := a.store.SaveQueue(queue); err != nil {
		fmt.Fprintf(os.Stderr, "defer: %v\n", err)
		os.Exit(3)
	}
	fmt.Printf("deferred %s until %s\n", id, now.Add(d).Format(time.RFC3339))
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
	written, err := install.Install(install.Options{
		HomeDir:    home,
		BinaryPath: self,
		Config:     a.cfg,
		Adapters:   a.table,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(3)
	}
	for _, path := range written {
		fmt.Println(path)
	}
	fmt.Println("\nRun these once, by hand:")
	for _, step := range install.NextSteps(home) {
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
