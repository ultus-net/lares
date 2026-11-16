# Operations

How `lares` runs on a host, what it writes, and what it refuses to do. The
README is the overview; this page is the reference.

## One pass

`lares pass` is the whole loop, and `lares.timer` runs it on the resync
interval.

1. Take `lock`, so a second lares process exits instead of working the same
   items. `pass`, `reboot`, and every hold change share that lock.
2. Run the probe (`hygiene-audit.sh --json`) and append one verdict per record.
3. Rebuild the queue from the findings, replaying any deferral from the log.
4. For each finding in the safe set that is due, run its repair verb.
5. Re-measure after 5 seconds, 30 seconds, and 5 minutes. The repair converged
   only when a verification names the item and says `live`. A missing record and
   an `unknown` record both leave the item queued.
6. Save the queue, then evaluate the reboot gate when `reboot_from_pass` is on.

A finding the safe set does not cover is queued and never attempted. The queue
is derived state: an item that stops being a finding leaves the queue, and its
attempt count resets. A deferral is written to `verdicts.jsonl` as well, so a
rebuilt queue keeps the operator's judgement.

## The safe set

| Item | Verb |
| --- | --- |
| `profile` | `install/profile-select.sh <profile>` |

Everything else queues, with the precondition that blocked it:

| Term | Meaning |
| --- | --- |
| `needs-tty` | the repair calls `as_root`, which opens `/dev/tty` for sudo |
| `needs-grant` | the repair needs a privilege the host has not granted |
| `needs-operator` | the repair writes to a remote or needs a judgement call |
| `unverified` | the probe returned `unknown`, so no repair is safe |
| `hold` | a live hold is blocking disruptive work |
| `scan` | `/proc` could not be read, so an unclaimed agent cannot be ruled out |
| `fix-did-not-hold` | the repair ran and verification saw no movement |
| `backoff` | the item is due, but its retry delay has not elapsed |
| `deferred` | the operator moved it out |

`maintenance` and `hermes` need a TTY. `patch-train` needs a sudo grant.
`notebook` creates and pushes a GitHub repository, so it is never unattended.
`offsite` needs a provider decision, `backup` needs a mount and a password, and
branch cleanup needs a per-branch judgement.

## Backoff

A failed converge increments `attempts` and sets `next_attempt_at` to now plus
the resync interval doubled per attempt, capped at one day. A repair that cannot
work costs one attempt per day, not one per resync.

## Holds

A hold is a file `holds/<owner>.json` with an owner, a reason, a pid, the
process start time, and a wall-clock start. Any long-horizon task writes one:

```bash
lares hold add "agent run: ticket 214"
lares hold add --pid 4242 "import running outside lares"
lares hold exec -- ./long-build.sh
```

`hold exec` starts the command, writes a hold that names the command's pid, and
removes it when the command exits. The hold names the child rather than lares,
so a lares crash leaves the running command protected.

The owner is `pid-<pid>`, and a non-slug owner is rejected rather than
rewritten, so two identities never share one file. A hold is stale when its pid
is gone, or when `/proc/<pid>/stat` reports a different start time, which means
the pid was reused. A stale hold is ignored, reported, and removed, so a crashed
tool cannot pin the host. A permission error means the process exists under
another user, so it keeps the hold.

`lares hold list` marks stale holds. `lares hold rm <owner>` removes one.

A live `opencode` or `orca` process with no hold is treated as a hold, and the
reboot verdict names it under `unclaimed`. An agent that forgets to write one is
the failure this policy exists to remove, so the daemon reports the mismatch
instead of rebooting over it.

## Deferring

```bash
lares defer offsite 24h
```

The item stays in the queue with `next_attempt_at` moved out, and the deferral
is appended to `verdicts.jsonl` as a `defer` record. A rebuild replays it, so a
lost queue keeps the operator's judgement. The shortest deferral is one minute.

## Rebooting

```text
can_reboot = window_open AND no_hold AND no_interactive_session AND no_own_job
```

| Term | Blocks when |
| --- | --- |
| `window` | the local clock is outside `window_start`-`window_end` |
| `hold` | any live hold exists, or a live `opencode`/`orca` process has none |
| `scan` | `/proc` could not be read, so no agent could be ruled out |
| `session` | a session other than the user's manager is logged in, Omarchy's stay-awake marker exists, a `block` or `block-weak` inhibitor lock is held, or `loginctl` cannot answer |
| `own-job` | an update transaction is running, or its state cannot be read |

The term order is the evaluation order, so the queue names the first blocker.
A blocked reboot is a queue item with that term, and it waits: nothing prompts,
because a question at 03:00 is a hung timer. The verdict fails closed: an
unreadable session state or train state blocks instead of guessing.

Inhibitor locks come from `systemd-inhibit --list`. A lock with mode `block` or
`block-weak` means a program asked the host to stay up: a build, a backup, or a
recording. Mode `delay` only postpones a sleep request, so it does not block a
reboot. The parser reads the mode from the end of each line, because the reason
field may be empty or contain spaces. A line it cannot parse, or a failed call
to `systemd-inhibit`, blocks the reboot instead of reporting an idle host. Arch
and Fedora atomic both ship the command. A host without it fails the read, so
the gate blocks rather than reading the host as idle.

The train check is per family. Arch asks `omarchy-update-lock held`. Fedora
atomic and bootc look for a running `rpm-ostree` or `bootc` process. A family
with no check at all is an error, so it blocks rather than claiming the train is
idle.

`pass` evaluates the gate last, while it still holds `lock`, unless
`reboot_from_pass` is false. `lares reboot` evaluates and acts in one lock hold,
so no repair can start between the check and the activation.
`lares reboot --dry-run` prints the verdict without acting.

Activation comes from `adapters.toml`, so Arch uses `omarchy-system reboot`
(closes windows first) and Fedora atomic or bootc reboots plainly.

## State

```text
~/.local/state/lares/
├── verdicts.jsonl        append-only, 90 day retention
├── queue.json            the queue
├── queue.json.bak        the previous generation
├── holds/                one file per live hold
├── runs/                 one log per item per UTC day, 30 day retention, 10 MiB cap
└── lock                  the interprocess lock, flock on a file
```

`lock` is not reentrant. A second process gets an error instead of waiting, so a
timer pass never queues behind a stuck one. A pass, a reboot, a measure, and a
hold or deferral write each take it.

`queue.json` is replaced, never edited: temp file, fsync, backup, rename,
directory fsync. A reader that finds a corrupt primary renames it to
`queue.json.corrupt-<unix-nano>`, loads the backup, and reports `recovered`.
When both files are unusable the read fails rather than starting empty.

## Install

```bash
lares install --user
```

Writes the binary, `lares.service`, `lares.timer`, `config.toml`, and
`adapters.toml`. An existing config or adapter file is left alone; the binary
and the units are replaced. The service unit gets `WorkingDirectory=<repo_dir>`,
so the timer finds the probe in the checkout it was installed from.

It prints one command and does not run it:

```bash
loginctl enable-linger "$USER"
```

The installer writes no sudoers file. A tool that grants itself passwordless
sudo is a privilege-escalation path, and lares never asks for a standing grant:
a repair that needs root stays queued as `needs-grant`.

## Adapters

`~/.config/lares/adapters.toml` holds the update verbs per family:

```toml
[arch]
stage = ["patch-train-update"]
activate = ["omarchy-system", "reboot"]
stamp = "/var/lib/patch-train/version-stamp"

[fedora-atomic]
stage = ["rpm-ostree", "upgrade"]
activate = ["systemctl", "reboot"]

[bootc]
stage = ["bootc", "upgrade"]
activate = ["systemctl", "reboot"]
```

Detection order is Arch first, then Fedora with `bootc` present, then Fedora with
`rpm-ostree` present. A host that names neither measures and never acts.

The family also decides who stages an update. On Arch the `patch-train` timer
runs the stage verb itself. On Fedora atomic and bootc the stage verb needs
root, and lares holds no grant, so lares never runs it. The `patch-train` queue
item carries the exact command instead, under the `needs-grant` term:

```text
rpm-ostree upgrade
bootc upgrade
```

Run that command with root, then reboot in a window.

A staged deployment clears the item. Fedora atomic asks
`rpm-ostree status --json` for a deployment with `staged` true. Bootc asks
`bootc status --json` for a non-null `status.staged`. The record then reports
`live` with the detail `staged deployment waits for reboot`. If the status
command fails, or prints a layout lares does not know, the item stays queued and
its detail names the error. A check that cannot see the deployment never reports
it staged.

After a reboot the staged deployment becomes the booted one, so the probe again
reports no patch-train timer and the item returns on the next pass. On Fedora the
item is a standing reminder to stage an update, not a one-shot task. Defer it
when an update is not wanted.

`config.toml`'s `adapter_override` names a section when detection is wrong.
`repo_dir` is the checkout that holds the probe. `reboot_from_pass` (default
true) decides whether a pass evaluates the reboot gate, and it exists so a host
that reboots by hand can turn the gate off without editing the binary.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| timer inactive | `systemctl --user status lares.timer` |
| queue not moving | `lares status`; the item's `Term` names the blocker |
| reboot never runs | `lares reboot --dry-run`; the term names the blocker |
| probe fails | run the configured `audit_command` by hand with `--json` |
| queue recovered from backup | `queue.json.corrupt-*` holds the file that was replaced |
