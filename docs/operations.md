# Operations

How `lares` runs on a host, what it writes, and what it refuses to do. The
README is the overview; this page is the reference.

## One pass

`lares pass` is the whole loop, and `lares.timer` runs it on the resync
interval.

1. Run the probe (`hygiene-audit.sh --json`) and append one verdict per record.
2. Rebuild the queue from the findings.
3. For each finding in the safe set that is due, run its repair verb.
4. Re-measure after 5 seconds, 30 seconds, and 5 minutes. A finding that clears
   is converged. Three unchanged measurements escalate with the run log path.
5. Save the queue with a new generation.

A finding the safe set does not cover is queued and never attempted. The queue
is derived state: an item that stops being a finding leaves the queue, and its
attempt count resets. A judgement the operator made, such as a deferral, is
stored in the item's `NextAttemptAt`, so a rebuilt queue keeps it.

## The safe set

| Item | Verb |
| --- | --- |
| `hermes` | `install/host-setup.sh --only hermes --apply` |
| `maintenance` | `install/host-setup.sh --only maintenance --apply` |
| `notebook` | `install/host-setup.sh --only notebook --apply` |
| `profile` | `install/host-setup.sh --only profile --apply` |

Everything else queues. `patch-train` is excluded because its timer needs a
widened sudo grant before it can succeed. `offsite` needs a provider decision,
`backup` needs a mount and a password, and branch cleanup needs a per-branch
judgement.

## Backoff

A failed converge increments `attempts` and sets `next_attempt_at` to now plus
the resync interval doubled per attempt, capped at one day. A repair that cannot
work costs one attempt per day, not one per resync.

## Holds

A hold is a file `holds/<owner>.json` with an owner, a reason, a pid, and a
start time. Any long-horizon task writes one:

```bash
lares hold add "agent run: ticket 214"
```

The owner is `pid-<pid>` for `hold add`. A hold is stale when its pid is gone.
A stale hold is ignored, reported, and removed, so a crashed tool cannot pin the
host. A hold whose file does not parse is stale too.

`lares hold list` marks stale holds. `lares hold rm <owner>` removes one.

A live `opencode` or `orca` process with no hold is treated as a hold, and the
reboot verdict names it under `unclaimed`. An agent that forgets to write one is
the failure this policy exists to remove, so the daemon reports the mismatch
instead of rebooting over it.

## Deferring

```bash
lares defer offsite 24h
```

The item stays in the queue with `next_attempt_at` moved out. A deferral is
never silently dropped by a rebuild.

## Rebooting

```text
can_reboot = window_open AND no_hold AND no_interactive_session AND no_own_job
```

| Term | False when |
| --- | --- |
| `window` | the local clock is outside `window_start`-`window_end` |
| `hold` | any live hold exists |
| `session` | a session other than the user's manager is logged in, Omarchy's stay-awake marker exists, or `loginctl` cannot answer |
| `own-job` | a pass is running, or Omarchy's update lock is held |

The term order is the evaluation order, so the queue names the first blocker.
A blocked reboot is a queue item with that term, and it waits: nothing prompts,
because a question at 03:00 is a hung timer.

Activation comes from `adapters.toml`, so Arch uses `omarchy-system reboot`
(closes windows first) and Fedora atomic reboots plainly. `lares reboot
--dry-run` prints the verdict without acting.

## State

```text
~/.local/state/lares/
├── verdicts.jsonl        append-only, 90 day retention
├── queue.json            the queue
├── queue.json.bak        the previous generation
├── holds/                one file per live hold
└── runs/                 one log per item per UTC day, 30 day retention, 10 MiB cap
```

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
and the units are replaced.

It prints two commands and does not run them:

```bash
sudo install -m 0440 lares.sudoers /etc/sudoers.d/lares
loginctl enable-linger "$USER"
```

A tool that grants itself passwordless sudo is a privilege-escalation path.
This one prints the exact commands instead.

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
```

`config.toml`'s `adapter_override` names a section when detection is wrong. An
unrecognised host measures and never acts.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| timer inactive | `systemctl --user status lares.timer` |
| queue not moving | `lares status`; the item's `Term` names the blocker |
| reboot never runs | `lares reboot --dry-run`; the term names the blocker |
| probe fails | run the configured `audit_command` by hand with `--json` |
| queue recovered from backup | `queue.json.corrupt-*` holds the file that was replaced |
