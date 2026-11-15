# lares

A reconciliation loop for Linux hosts that are configured by a dotfiles
repository.

`lares` measures a machine through the `hygiene-audit.sh --json` contract,
converges the repairs that are safe to repeat, and queues everything that needs
a person. It keeps an append-only verdict log, so a host can answer "not-live
since when" without a database.

The name is Roman: each household had its own `lar`, a guardian of that house.
One binary runs per host. There is no server, no control plane, and no model in
the decision path.

## What it does

- Measures through commands it does not own. A probe lives in the dotfiles
  repository, and `lares` reads the verdict it prints.
- Converges only the repairs that are idempotent and cannot prompt: timers,
  journal bounds, a profile selection, and a vault push. A repair counts as
  converged only when a second measurement says the state moved.
- Queues everything else with the reason it queued, how many times it has been
  attempted, and when it may be tried again.
- Records holds. An agent run, a build, an import, or a call writes a hold, and
  a reboot or a disruptive converge never runs while one is live.

## What it refuses to do

- It writes no probe and no repair of its own. When the dotfiles repository
  gains a check, `lares` learns it from the contract, not from a code change.
- It never grants itself privilege. The installer prints the sudoers drop-in and
  stops; the operator installs it.
- It never reboots while a hold exists, outside the reboot window, with an
  interactive session logged in, or while an update train is running.
- It never pushes, force-pushes, or rewrites git history, and it deletes no
  branch that is not merged.
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
`~/.config/lares/{config.toml,adapters.toml}`. It prints the two commands it
will not run for you: the sudoers drop-in and `loginctl enable-linger`.

## Commands

```text
lares measure [--json]        measure once and print the verdicts
lares pass                    one reconcile pass: measure, converge, queue
lares status [--json]         the queue, the holds, and the last measure
lares hold add <reason>       write a hold for this process
lares hold list               list holds, marking stale ones
lares defer <id> <duration>   keep an item out of the queue for a while
lares install --user          write the binary, units, and config
lares tui                     the punch list
```

## State

```text
~/.local/state/lares/
├── verdicts.jsonl   append-only, one record per measure
├── queue.json       items awaiting judgement
├── holds/           one file per live hold
└── runs/            one log per converge, named by item and timestamp
```

## Docs

- [Operations](docs/operations.md): one pass, the safe set, holds, the reboot
  terms, state durability, and troubleshooting.

## Licence

MIT. See `LICENSE`.
