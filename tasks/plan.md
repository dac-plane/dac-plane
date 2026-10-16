# Implementation plan: issue #18 pre-persistence contract gate

Tracker: [GitHub issue #18](https://github.com/AssahBismarkabah/dac-plane/issues/18) (epic #11)

## Objective

Approve the lifecycle contracts before PostgreSQL makes them expensive to change. Verify identity binding, lifecycle transitions, audit semantics, budget accounting, result/review rules, adapter replay, cancellation, authority and artifact ordering agree across Go and JSON Schema, with executable fixtures and no vendor-required transports. Record the approval in `docs/architecture/contract-gate-18.md`. No persistence, no simulator, no final M1 freeze.

## Sources

- `docs/architecture/README.md` provider contract semantics list (normative).
- `docs/architecture/review-2026-10-15.md` invariants plus contract-freeze gate six items.
- `docs/adr/0003-provider-contract-semantics.md` all families plus rejected choices (normative).
- `architecture-explore/` evidence only: Plane-Bot `MASTER-RECORD.md`, `DAC-PLANE-MAPPING.md`, `DEEP-DIVES-03.md`, `DEEP-DIVES-04.md` (shape, no vendor names).
- Current contracts: `internal/identity`, `internal/lifecycle`, `internal/contracts` plus `contracts/jsonschema/{identity,lifecycle,adapter}` and guards in `internal/testsupport/schematest`.

## Design

- Gate file `docs/architecture/contract-gate-18.md` records prerequisites (#13-#17, #32 closed), per-family agreement (Go type, schema, fixture test), enum/identity/failure tables, vendor sweep result, and trade-offs (none expected; any found goes to baseline or new ADR, never an edit to accepted ADR).
- Extend `vendorIdentifiers` in `internal/testsupport/schematest/contracts_test.go` with the exact issue phrases `native helper` and `grok bot` (space forms, so `nativeRef`/`native_ref` fixtures do not false-positive; `grok` already covers the latter, explicit for readability).
- Note: issue verification path `internal/domain/...` does not exist; correct gate verification is `internal/identity`, `internal/lifecycle`, `internal/contracts` via `just test-contracts` and `just ci`.

## Ordered tasks

1. Rewrite this plan (this file) against the baseline.
2. Audit Go-vs-schema agreement: enums, identities, 13 failure codes, transition tables.
3. Confirm executable fixtures exist for replay/duplicates/generations, retirement/revocation/generation, hook allow/deny/rewrite/timeout, spend challenge/receipt, artifact committed/retry/fenced, env claim fencing, shadow/off/live plus migration preview/execute, stale precondition plus serial action.
4. Extend vendor guard, run `just test-contracts`.
5. Write `docs/architecture/contract-gate-18.md`.
6. Run `git diff --check` and full `just ci`.

## Verification

```sh
just test-contracts
go test ./internal/identity/... ./internal/lifecycle/... ./internal/contracts/...
just ci
git diff --check
```

Issue command `go test ./internal/domain/...` is stale (no such package); the above is the correct equivalent. State what was not verified in the PR.

## Boundaries

- No `internal/platform/migrations/*` SQL, no persistence (#19-#21), no simulator (#22), no application boundaries (#23-#26), no HTTP/transports/SDKs.
- No baseline diagram edits; no edits to accepted ADRs (new ADR only if a trade-off needs one).
- `architecture-explore/` read-only.
