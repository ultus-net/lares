# lares

A reconciliation loop for a Linux host: it measures the machine, syncs the
repositories it owns, converges the repairs that are safe to repeat, and queues
everything that needs a person. It keeps an append-only verdict log, so a host
can answer "not-live since when" without a database.

The name is Roman: each household had its own `lar`, a guardian of that house.
One binary runs per host. There is no server, no control plane, and no model in
the decision path.

## What it does

- Measures in-process. The checks are Go and read the host directly: the login
  shell, the deployment status of an atomic host, failed systemd units, free
  space, and the git state of the configured repositories. A host needs no
  checkout and no probe script.
- Syncs repositories in both directions. A clean checkout that is behind its
  upstream is fast-forwarded, and dead worktree records are pruned. A clean
  checkout with unpushed commits is pushed to `backup/<host>/<repo>`, so the
  only copy on a single-disk host also lives off-box. A dirty checkout, a
  detached head, or a diverged remote queues for a person.
- Converges only the repairs that are idempotent and cannot prompt. A repair
  counts as converged only when a verification measurement names the item and
  says `live`.
- Queues everything else with the precondition that blocked it, how many times
  it has been attempted, and when it may be tried again.
- Records holds. An agent run, a build, an import, or a call writes a hold, and
  a reboot or a converge never runs while one is live. `lares hold exec -- cmd`
  holds for exactly the life of a command.

## Checks

| Item | Reads | Unattended repair |
| --- | --- | --- |
| `shell` | the login shell in `/etc/passwd`, and that it exists and is executable | none; `needs-operator` |
| `patch-train` | `rpm-ostree status --json`, then `ostree admin status --json`, then `bootc status --format json`, or the Arch version stamp | none; a staged deployment needs `needs-grant` and a reboot |
| `systemd-failed` | `systemctl --failed` for the system and the user | none; `needs-operator` |
| `disk` | free bytes and free percentage on `/` and the home directory | none; `needs-operator` |
| `repo-<name>` | `git fetch --prune`, `git status --porcelain=v2 --branch`, and `git worktree list` | `fast-forward` when the checkout is clean, on a branch, and behind its upstream |
| `backup-<name>` | `git ls-remote --symref`, `git fetch`, and `git rev-list --left-right --count HEAD...@{u}` | `push` to `backup/<host>/<name>` when the checkout is clean, on a branch, and the remote branch is absent or behind |

A check that cannot read the host reports `unknown`. The queue shows it and
never repairs it, because no check proved the item healthy.

`systemd-failed` ignores `systemd-remount-fs.service` on a read-only composefs
root, where that unit fails on every boot by design. The detail says so. An
unreadable `repos_dir` is an `unknown` `repo-discovery` record, never an empty
set.

## What it refuses to do

- It writes no repair it cannot verify. The safe set is a closed list in the
  measure engine, and a check outside it queues instead of running.
- It never grants itself privilege and never writes sudoers. A repair that needs
  root stays queued as `needs-grant`.
- It never reboots while a hold exists, outside the reboot window, with an
  interactive session logged in, or while an update train is running.
- REQUIREMENT (operator, 2026-11-16): pushes are mandatory. A local-only commit
  is not backed up, so each clean checkout syncs outbound to
  `backup/<host>/<repo>`, the mirror of the inbound fast-forward. This
  supersedes the earlier in-repo rule that lares never pushes. Every rule that
  refusal protected still holds: lares never forces, never rewrites history,
  never deletes an unmerged branch, never pushes to a remote's default branch
  (resolved with `git ls-remote --symref`, not against a literal `main`), never
  pushes tags, notes, or refs outside the backup namespace, and reads no secret.
  A protected branch, a diverged remote, or an auth failure queues for a person.
- It reads no secret. Provider credentials stay in the file that owns them.

## Install

```bash
git clone https://github.com/ultus-net/lares
cd lares
make build
./lares install --user
```

`lares install --user` writes the binary to `~/.local/bin/lares`, the units to
`~/.config/systemd/user/lares.{service,timer}`, and the config to
`~/.config/lares/{config.toml,adapters.toml}`. It records `repos_dir` so the
timer checks the same checkout set, and prints the one command it will not run
for you: `loginctl enable-linger`. A re-install replaces the binary and the
units and leaves an edited config alone.

Outbound backup needs one operator step after install: a deploy key scoped to
the destination repository, used through `GIT_SSH_COMMAND`. lares parses no
token and opens no key. The key, the systemd drop-in, and the reinstall
procedure are in [docs/operations.md](docs/operations.md#backup-credentials-and-restore).

## Commands

```text
lares measure [--json]        measure once and print the verdicts
lares pass                    one reconcile pass: measure, converge, queue
lares status [--json]         the queue, the holds, and the newest verdict
lares hold add [--pid N] <reason>  write a hold for pid N, or for this process
lares hold exec -- <command>  hold while a command runs, then release it
lares hold list               list holds, marking stale ones
lares hold rm <owner>         remove a hold
lares defer <id> <duration>   keep an item out of the queue for a while
lares reboot [--dry-run]      evaluate the reboot gate
lares install --user          write the binary, units, and config
lares tui                     the punch list
```

## Configuration

```toml
# Optional. When unset, lares measures the host in-process.
# audit_command = ["bash", "/opt/my-probe.sh"]

state_dir = "/home/me/.local/state/lares"
config_dir = "/home/me/.config/lares"
repos_dir = "/home/me/Documents/projects"

# Optional. An explicit list replaces discovery under repos_dir.
# repos = ["/home/me/Documents/projects/lares"]

window_start = "02:00"
window_end = "05:00"
resync = "15m"

# Optional. auto prints the journal line and adds a desktop notification when a
# session exists. auto | desktop | stdout | off
# notify = "auto"

reboot_from_pass = true
```

`repos_dir` is enumerated one level deep: each immediate child that holds a
`.git` entry is a repository. The enumeration is bounded and never walks an
arbitrary tree.

An external `audit_command` is an escape hatch for a measurement lares does not
ship. Its records are queued and never repaired unattended, because lares did
not produce the repair.

`notify` decides how a new finding reaches a person, and dedupe is per item,
state, and detail, so the same failure is surfaced once. The channels and the
dedupe rule are in [docs/operations.md](docs/operations.md#notifications).

## State

```text
~/.local/state/lares/
├── verdicts.jsonl   append-only, one record per measure or deferral
├── queue.json       items awaiting judgement
├── notified.json    the findings already surfaced to a person
├── holds/           one file per live hold
├── runs/            one log per converge, named by item and timestamp
└── lock             the interprocess lock a pass and a reboot share
```

## Docs

- [Operations](docs/operations.md): one pass, the safe set, holds, the reboot
  terms, state durability, and troubleshooting.

## Licence

MIT. See `LICENSE`.
