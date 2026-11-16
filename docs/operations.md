# Operations

How `lares` runs on a host, what it writes, and what it refuses to do. The
README is the overview; this page is the reference.

## One pass

`lares pass` is the whole loop, and `lares.timer` runs it on the resync
interval.

1. Take `lock`, so a second lares process exits instead of working the same
   items. `pass`, `reboot`, and every hold change share that lock.
2. Run the in-process checks and append one verdict per record.
3. Rebuild the queue from the findings, replaying any deferral from the log.
4. For each finding in the safe set that is due, run its repair.
5. Re-measure after 5 seconds, 30 seconds, and 5 minutes. The repair converged
   only when a verification names the item and says `live`. A missing record and
   an `unknown` record both leave the item queued.
6. Save the queue, then evaluate the reboot gate when `reboot_from_pass` is on.

A finding the safe set does not cover is queued and never attempted. The queue
is derived state: an item that stops being a finding leaves the queue, and its
attempt count resets. A deferral is written to `verdicts.jsonl` as well, so a
rebuilt queue keeps the operator's judgement.

## Checks

The checks are Go and read the host directly. There is no probe script and no
checkout to find.

| Item | Reads | Safe repair |
| --- | --- | --- |
| `shell` | the login shell field of the current uid in `/etc/passwd`; the path must be absolute, exist, and be executable | none |
| `patch-train` | `rpm-ostree status --json`, then `ostree admin status --json`, then `bootc status --format json`, or the Arch version stamp | none |
| `systemd-failed` | `systemctl --failed --no-legend --plain` for the system and the user | none |
| `disk` | total and available bytes on `/` and the home directory | none |
| `repo-<name>` | `git fetch --prune`, `git status --porcelain=v2 --branch`, and `git worktree list --porcelain` | `fast-forward` |

A check that cannot read the host returns `unknown`. The queue shows the item
with the `unverified` term and never repairs it. A family with no adapter
reports `patch-train` as `unknown` instead of claiming the host is current.

`systemd-failed` names a scope it could not read. If both scopes fail, the
record is `unknown`, so an unreadable `systemctl` never looks like an idle host.

`systemd-failed` excludes `systemd-remount-fs.service` on a Fedora atomic or
bootc host whose root is read-only. An immutable composefs root cannot be
remounted, so that unit fails on every boot and no operator action clears it.
The detail names the exclusion whenever it applies. Every other failed unit
stays a finding.

### The patch train

`patch-train` reports a staged deployment as `stale`. Staging needs root, so a
staged deployment is `needs-grant` and waits for a person or the reboot gate,
which activates it.

The deployment state is read in a fixed order, and the first reader that
answers wins:

1. `rpm-ostree status --json` answers without root and carries the version. It
   covers Fedora atomic and every Universal Blue image that still updates
   through rpm-ostree, including a host detected as `bootc`.
2. `ostree admin status --json` answers without root on any ostree host. It
   keeps a bootc-only image measurable when rpm-ostree is absent. It carries no
   version, so the detail names the deployment by checksum prefix.
3. `bootc status --format json` is last because it demands root on the current
   Universal Blue images. It only helps when lares runs privileged.

A reader that fails or returns an unparsable document falls through to the next.
When every reader fails, the check reports `unknown`. The Arch family reads the
version stamp's modification time and reports `stale` when it is older than 35
days.

### Repositories

`repos_dir` is enumerated one level deep: each immediate child that holds a
`.git` entry is a repository, and an explicit `repos` list replaces discovery.
The enumeration is capped at 200 directories and never walks a tree. A
`repos_dir` that cannot be read is an `unknown` `repo-discovery` record, never an
empty set, so a missing or unreadable directory cannot report the host healthy.

Each check fetches with a 60 second timeout, so a behind count is current. A
failed fetch keeps the last known remote refs; the detail says the fetch failed
and the state is at worst stale, never wrong in the unsafe direction.

| State | Meaning |
| --- | --- |
| `live` | clean and level with the upstream |
| `stale` | clean, behind the upstream, or holding prunable worktree records |
| `not-live` | uncommitted paths, a detached head, no upstream, or unpushed commits |

Only a `stale` checkout is in the safe set. `fast-forward` prunes dead worktree
records and runs `git merge --ff-only <upstream>`. It refuses a dirty worktree
or a local commit the upstream does not have, so a repair can never discard
work. The fast-forward itself never rewrites history.

### Outbound backup

REQUIREMENT (operator, 2026-11-16): pushes are mandatory. A local-only commit is
not backed up, so every clean checkout is also pushed to
`backup/<host>/<repo>` in its own remote. This is the mirror of the inbound
fast-forward, and it supersedes the earlier rule that lares never pushes.

The branch is created once with `--set-upstream` and reused on every later
pass. A new branch per pass would fill the remote, trip the destination's CI,
and make an item unable to reach `live` twice.

The item is `live` only when convergence is bilateral, proved after a fetch:

- `git ls-remote --exit-code` names the local `HEAD` as the remote tip.
- the local tracking ref (`@{u}`) resolves to that same commit.
- ahead and behind are both zero against `@{u}`.

A push exit code alone is not evidence, the same way a version print is not
evidence that a service is up.

The push runs only when the checkout is clean, on a branch, the remote is
reachable, and no live hold exists. It never forces, never rewrites history,
never deletes an unmerged branch, never pushes tags, notes, or refs outside the
backup namespace, and never pushes to the remote's default branch. The default
branch is read with `git ls-remote --symref <remote> HEAD`, so the refusal holds
for `main`, `master`, `trunk`, or any other name. An empty remote reports no
default branch, and the first backup may create its branch there.

Failures are classified, never retried blind:

| Failure | Handling |
| --- | --- |
| protected branch, default-branch target | terminal; queue as `needs-operator` |
| non-fast-forward, diverged remote | terminal; queue, never force |
| auth, missing deploy key | terminal until the operator acts |
| network, DNS, timeout | retryable with backoff |

An unpushed backup whose retryable push fails three times is `escalated` and
outranks the rest of the punch list, because on a single-disk host the checkout
is the only copy. A terminal failure never escalates: retrying cannot help, so it
stays `needs-operator`.

A terminal failure persists in the queue as `needs-operator` with `blocked`
naming its kind, and no pass retries it. It leaves the queue when a measurement
proves the item live. When the operator fixes the cause, clear the block with a
deferral, which lets the next attempt try again:

```bash
lares defer backup-<name> 2m "rotated the deploy key"
```

The destination is the source repository's own remote. If that repository runs
workflows on every branch, exclude `backup/*` from its triggers. lares does not
edit CI configuration.

### Backup credentials and restore

lares parses no token and opens no key. Provision a deploy key with write access
to the destination repository only, and let git use it through
`GIT_SSH_COMMAND`. Treat the key like `secrets.env`: the operator provisions it
after install, it lives in no repository, and the restore procedure below names
it so a reinstalled host rejoins.

Add a systemd drop-in so the timer, not only an interactive shell, sees the key:

```ini
# ~/.config/systemd/user/lares.service.d/backup.conf
[Service]
Environment=GIT_SSH_COMMAND=ssh -i %h/.ssh/lares-backup -o IdentitiesOnly=yes
```

Then run `systemctl --user daemon-reload`.

To restore a host:

1. `lares install --user`.
2. Provision `~/.ssh/lares-backup` and the drop-in above.
3. Run `lares pass`. The first pass creates `backup/<host>/<repo>` when it is
   absent, or reuses it and sets the local upstream when it exists.
4. `lares status` lists `backup-<name>`. A `live` state means both the remote
   tip and the local upstream name `HEAD`.

## The safe set

| Item | Verb |
| --- | --- |
| `repo-<name>` | `fast-forward` |
| `backup-<name>` | `push` |

Everything else queues, with the precondition that blocked it:

| Term | Meaning |
| --- | --- |
| `needs-tty` | the repair would open `/dev/tty` for sudo |
| `needs-grant` | the repair needs a privilege the host has not granted |
| `needs-operator` | the repair writes to a remote or needs a judgement call |
| `unverified` | the check returned `unknown`, so no repair is safe |
| `hold` | a live hold is blocking disruptive work |
| `scan` | `/proc` could not be read, so an unclaimed agent cannot be ruled out |
| `fix-did-not-hold` | the repair ran and verification saw no movement |
| `backoff` | the item is due, but its retry delay has not elapsed |
| `deferred` | the operator moved it out |
| `escalated` | an unpushed backup failed three retryable pushes; it outranks the punch list |

`patch-train` needs a sudo grant, and a staged deployment needs a reboot. A
dirty or ahead repository needs a person. A diverged or protected backup needs a
person. Everything lares cannot prove queues rather than running.

## Backoff

A failed converge increments `attempts` and sets `next_attempt_at` to now plus
the resync interval doubled per attempt, capped at one day. A repair that cannot
work costs one attempt per day, not one per resync.

A classified terminal push failure does not schedule a retry. It stays
`needs-operator` with `blocked` until a measurement proves the item live, or the
operator clears the block with a deferral. A network push failure retries with
the normal backoff. Each failed retryable push increments `attempts`; at three
the item becomes `escalated`.

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
and the units are replaced. The service unit runs with `WorkingDirectory=%h`,
so the timer needs no checkout. lares measures the host in-process.

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

`config.toml`'s `adapter_override` names a section when detection is wrong.
`repos_dir` is the directory whose immediate children are checked, and `repos`
replaces discovery with an explicit list. `reboot_from_pass` (default true)
decides whether a pass evaluates the reboot gate, and it exists so a host that
reboots by hand can turn the gate off without editing the binary.

`repo_dir` is accepted and ignored so a config written by the previous installer
still loads. Delete it; `repos_dir` and `repos` replace it.

`audit_command` is optional. When set, lares runs that command instead of its
in-process checks and queues every record it prints, because lares did not
produce the repair.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| timer inactive | `systemctl --user status lares.timer` |
| queue not moving | `lares status`; the item's `Term` names the blocker |
| reboot never runs | `lares reboot --dry-run`; the term names the blocker |
| a check reports `unknown` | run the host tool it reads by hand; the detail names the failure |
| a repository is not checked | `repos_dir` holds it one level deep, or `repos` names it |
| a repository stays queued | `lares status`; a dirty or ahead checkout needs a person |
| queue recovered from backup | `queue.json.corrupt-*` holds the file that was replaced |
