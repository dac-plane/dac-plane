# Issue #101 — Prove cancellation across runtime and environment adapters

Tracker: https://github.com/dac-plane/dac-plane/issues/101
Branch: `test/101-runtime-environment-cancellation`, from `origin/main` after #106.
Parent: #75 / #65. Supplies the cancellation gate #91 was waiting on.

## Scope

- Run the Codex and E2B adapters in one deployed stack, with E2B as the only environment connection.
- Accept every terminal turn end, and a rejected start, as runtime stop evidence (ADR-0015).
- Repeat interrupts until the runtime stop is observed.
- Settle uncertain Codex starts, fence starts that produced no turn, and end commands before reporting a stop.
- Finish E2B stops durably across failures and restarts.
- Keep the Codex sandbox gap as a stated limit; it is separate work.

## Verification

- `just qualify-task-cancellation` against live Codex and E2B Cloud.
- `just test-codex-adapter`, `just test-e2b-adapter`, `just test-conformance`, `just ci-fast`, integration recipes.
- Evidence: `docs/architecture/task-cancellation-101.md`.
