# Release evidence: v0.2.0

Evidence for [#134](https://github.com/dac-plane/dac-plane/issues/134). It records what was observed, including failures.

## Live provider run before the first tag

Host: one arm64 machine, 2026-11-09. Images: local builds of `main` with the `set-policy` change, before the Go 1.27.4
and dependency updates.

| Scope | Result |
|---|---|
| Full `qualify-providers` | 13 of 15 scenarios passed. All 8 containment scenarios passed. 5 of the 7 cancellation scenarios passed. |

The two failures were on the E2B side:

- `stop_lost_with_its_adapter`: the sandbox create never answered, and the attempt stayed in `provisioning` until the
  test's 3-minute deadline. This is the known limit "A provision can stall".
- `start_lost_with_its_adapter`: the test's own direct request to E2B, asking whether the sandbox still existed, failed
  with `E2B request failed`. The cause was not established.

## Upgrade from v0.1.0

`just test-operations-upgrade` pulls the v0.1.0 image by digest, lets it migrate and provision at schema 15, rolls both
replicas with a task waiting for approval, migrates, confirms v0.1.0 refuses the new schema, completes the task and
applies `set-policy`. It passed locally on arm64 on 2026-11-11 and runs on every pull request. It uses simulators, not
live providers.

## Rehearsal

To be recorded.

## Release

To be recorded.
