# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- The `hygiene-audit.sh --json` contract reader: one record per line, a summary
  record last, and a state of `live`, `not-live`, `stale`, or `unknown`.
- An append-only verdict log, a queue with a generation counter and an atomic
  replace, holds keyed by owner, and one run log per item per day.
- Family detection from `/etc/os-release` plus `rpm-ostree`, with a config
  override, and an `adapters.toml` table for the stage and activate verbs.
- One reconcile pass that measures, converges the safe set, re-measures, and
  queues the rest with backoff.
- The reboot policy: window, holds, interactive sessions, and a running update
  train.
- `lares install --user`, which writes the binary, the units, and the config,
  and prints the one session command it will not run.
- The CLI: `measure`, `pass`, `status`, `hold add|list|rm`, `defer`, `reboot`,
  `install`, and `tui`, with JSON output on `measure` and `status`.
- A live `opencode` or `orca` process with no hold blocks a reboot and is named
  in the verdict, so a forgotten hold fails safe.
- The punch-list TUI (Bubble Tea): severity-ordered rows, one key per action,
  the holds, and a raw-detail pane.
- Verdict history pruned once per pass, with the rewrite skipped when nothing
  expires.
- An interprocess `flock` on `<state>/lock`. A pass, a reboot, a measure, and a
  hold or deferral write serialize on it, and a second process gets `ErrLocked`
  instead of waiting.
- `lares hold add --pid N` holds another process, `lares hold exec -- cmd` holds
  for the life of a command, and holds carry the process start time so a reused
  pid cannot keep a stale hold alive.
- `repo_dir` and `reboot_from_pass` in `config.toml`. A pass evaluates the
  reboot gate last, under the lock it already holds.
- A `bootc` adapter family, detected ahead of `rpm-ostree`.
- Deferrals are appended to the verdict log as `defer` records and replayed, so
  a rebuilt queue keeps the operator's judgement.
- `unknown` records are queued and shown, never treated as healthy.
- CLI smoke tests that drive the built binary: the four documented exit codes,
  `hold exec`'s hold and release, the deferral floor, the install file set and
  its single printed step, and the `measure` and `status` JSON shapes.
- The reboot gate reads `systemd-inhibit --list`. A `block` or `block-weak`
  lock blocks the reboot as a busy session, because a build or a backup asked
  the host to stay up. A `delay` lock does not. The parser is tested against
  output captured from systemd 262 (`inhibit_test.go` records the provenance),
  and a failed read or an unparsable line blocks instead of reporting idle. A
  host without the command fails the read, so it blocks rather than reading as
  idle.

### Changed

- The safe set narrows to `profile`. Every other repair queues with the
  precondition that blocked it: `needs-tty`, `needs-grant`, or
  `needs-operator`. `maintenance`, `hermes`, and `notebook` cannot run
  unattended: the first two open `/dev/tty` for sudo, and the third creates and
  pushes a GitHub repository.
- Convergence requires a verification measurement that names the item and says
  `live`. A missing record and an `unknown` record both leave the item queued.
- The reboot gate fails closed when the session state or the update train state
  cannot be read, and names a long-horizon process with no hold as `unclaimed`.
  Fedora families find the transaction by process name; a family with no train
  check blocks instead of reporting an idle train.
- Hold owners and item ids must be slugs. A name that would need rewriting is
  rejected, so two identities never share one file.
- An agent scan that cannot read `/proc` blocks a pass and a reboot under the
  `scan` term instead of reporting no agents or looking like an operator hold.
- The TUI no longer offers keys it does not implement. `[d]efer` records a
  deferral through the engine, and the detail pane prints the repair command.

### Removed

- Sudoers generation. The installer never asks for a standing grant, so a
  repair that needs root stays queued.
- `[s]etup` and `[c]lean` in the punch list, which had no handler.
