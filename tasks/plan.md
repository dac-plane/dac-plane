# Issue #108 — Run Codex's work inside the assigned environment

Tracker: https://github.com/dac-plane/dac-plane/issues/108
Branch: `feat/108-codex-in-environment`, from `origin/main` after #109.
Parent: blocks #96. Decision: ADR-0016.

## Scope

- Keep `codex-app-server` and the model key in the adapter, and run exec-server in the sandbox.
- Derive each sandbox's bearer token from a key both adapters share.
- Define the sandbox as a pinned `e2b.Dockerfile`, built by the pinned E2B CLI.
- Read results through exec-server, since Codex's `fs` RPCs reach only the adapter.
- Report a turn that lost its environment as failed, and end a task waiting on a failed Attempt.

## Verification

- `just qualify-codex-containment` against live Codex and E2B Cloud.
- `just qualify-e2b-environment`, `just test-codex-adapter`, `just test-e2b-adapter`, `just test-conformance`, `just ci-fast`.
- Evidence: `docs/architecture/codex-containment-108.md`.
