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
  and prints the two privileged commands instead of running them.
