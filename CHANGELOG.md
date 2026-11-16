# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- An in-process Go measure engine. The host checks are `shell`, `patch-train`,
  `systemd-failed`, and `disk`; a host needs no probe script and no checkout.
- Repository checks. `repos_dir` is enumerated one level deep for immediate
  children holding a `.git` entry, and an explicit `repos` list replaces
  discovery. Each `repo-<name>` check fetches with a timeout, reads porcelain
  status, and reports worktree records that git marks prunable.
- `fast-forward`, the safe repair for a clean checkout that is behind its
  upstream. It prunes dead worktree records, refuses a dirty checkout or an
  unpushed commit, and moves a branch only by fast-forward.
- Outbound backup. Each clean checkout is pushed to `backup/<host>/<repo>` in
  its own remote. The branch is created once with `--set-upstream` and reused on
  later passes. The item is `live` only when the remote tip, the local tracking
  ref, and `HEAD` agree after a fetch, and ahead and behind are both zero; a
  push exit code alone is not evidence.
- `backup-<name>` checks and the `push` repair. The push never forces, refuses
  the remote default branch resolved with `git ls-remote --symref`, overrides
  ambient `push.followTags`, and never pushes tags, notes, or refs outside the
  backup namespace. An existing local backup branch moves only forward, an empty
  remote can receive its first backup, and an unreadable checkout reports
  `unknown` rather than a false backup finding.
- Tests for the backup refusals: default-branch target, a diverged remote, a
  rejected push, a credential file that is never read, branch reuse, and
  bilateral convergence.
- `repos_dir` and `repos` in `config.toml`.
- `reconcile.Measure`, so `lares measure` uses the same engine a pass does.
- Measure tests for every host check, repository classification, discovery
  bounds, and the repair's refusal paths.
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
- `reboot_from_pass` in `config.toml`. A pass evaluates the
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
- A notification pass step. Each new finding, meaning every item left in the
  queue plus a blocked reboot, is surfaced once: printed for the journal in
  `auto` or `stdout` mode, and sent with `notify-send` when a session exists in
  `auto` or `desktop` mode. `notify` in `config.toml` picks the mode and
  defaults to `auto`. Dedupe is keyed by item id, state, and detail in
  `notified.json`, written under the state lock, so a repeated identical failure
  is silent and a changed detail is surfaced again. A `fix-did-not-hold` finding
  and a blocked reboot are sent as critical urgency. A desktop failure is
  reported on stderr and never fails the pass.

### Changed

- The README names `notify` and the `notified.json` state file, which the
  notification step added to the config and the state directory.
- REQUIREMENT (operator, 2026-11-16): pushes are mandatory. This supersedes the
  earlier in-repo rule that lares never pushes. A local-only commit is not
  backed up, so every clean checkout now syncs to `backup/<host>/<repo>`. The
  safety rules the old refusal protected are unchanged: lares never forces,
  never rewrites history, never deletes an unmerged branch, never pushes to a
  remote's default branch, never pushes tags, notes, or refs outside the backup
  namespace, and reads no secret.
- Push failures are classified. A protected branch, a diverged remote, and an
  auth failure queue as `needs-operator` and are not retried blind. A network
  fault retries with backoff. An unpushed backup whose retryable push fails
  three times is `escalated` and outranks the rest of the punch list. A terminal
  failure never escalates.
- The safe set is the repository fast-forward and the outbound push. Every other
  repair queues with the precondition that blocked it: `needs-grant` for a
  staged deployment or a privileged verb, and `needs-operator` for a dirty
  checkout, a detached head, an unpushed commit, a failed unit, or low disk.
- `audit_command` is optional. An empty value measures the host in-process; a
  non-empty value runs the external probe and queues every record it prints,
  because lares did not produce the repair.
- The installed service unit runs with `WorkingDirectory=%h`, and the installer
  records `repos_dir` instead of a checkout.
- Convergence requires a verification measurement that names the item and says
  `live`. A missing record and an `unknown` record both leave the item queued.
- On Fedora atomic and bootc the `patch-train` item names the adapter's stage
  command (`rpm-ostree upgrade` or `bootc upgrade`) instead of a repair lares
  never runs. A staged deployment, read from `rpm-ostree status --json` or
  `bootc status --json`, reports the item live and clears it. A failed or
  unparsable status keeps the item queued and names the error.
- The reboot gate fails closed when the session state or the update train state
  cannot be read, and names a long-horizon process with no hold as `unclaimed`.
  A bootc host counts a running `rpm-ostree` as well as a running `bootc`, so a
  Universal Blue image that still updates through rpm-ostree cannot reboot
  mid-transaction. Fedora families find the transaction by process name; a
  family with no train check blocks instead of reporting an idle train.
- `patch-train` reads the deployment state in a fixed order: `rpm-ostree status
  --json`, then `ostree admin status --json`, then `bootc status --format json`.
  rpm-ostree and ostree answer without root, so a standard Universal Blue image
  that detects as `bootc` stays measurable. bootc is last because it demands
  root on those images. The ostree reader carries no version, so the detail
  names the deployment by checksum prefix.
- `systemd-failed` ignores `systemd-remount-fs.service` on a Fedora atomic or
  bootc host with a read-only root, where the unit fails on every boot because
  an immutable composefs root cannot be remounted. The detail names the
  exclusion, and every other failed unit stays a finding. A scope that
  `systemctl` cannot read is named in the detail; when neither scope can be read
  the record is `unknown`, so the check never reads an unreadable host as idle.
- An unreadable `repos_dir` produces an `unknown` `repo-discovery` record
  instead of an empty check set, so a missing or unreadable directory cannot
  shrink the measurement to a clean host.
- `repo_dir` is accepted and ignored, so a config written by the previous
  installer still loads. `repos_dir` and `repos` replace it; delete the old key.
- A repository fast-forward runs its safety checks before any mutating command,
  so a refused repair leaves worktree bookkeeping untouched. A failed worktree
  prune is recorded in the repair log instead of being discarded.
- Hold owners and item ids must be slugs. A name that would need rewriting is
  rejected, so two identities never share one file.
- An agent scan that cannot read `/proc` blocks a pass and a reboot under the
  `scan` term instead of reporting no agents or looking like an operator hold.
- The TUI no longer offers keys it does not implement. `[d]efer` records a
  deferral through the engine, and the detail pane prints the repair command.

### Removed

- The dotfiles coupling: `repo_dir`, the `LARES_REPO` environment variable, and
  the `--repo` flag. lares no longer reads a checkout to find a probe or a
  repair.
- A PR body scratch file that the #3 merge committed by accident. `.prbody-*`
  and `.tmp-*` are ignored so local scratch cannot land again.
- The `hygiene-audit.sh` probe as the default measurement, and the
  `install/profile-select.sh` repair as the safe set.
- Sudoers generation. The installer never asks for a standing grant, so a
  repair that needs root stays queued.
- `[s]etup` and `[c]lean` in the punch list, which had no handler.
