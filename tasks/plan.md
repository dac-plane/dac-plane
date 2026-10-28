# Issue #99 — Carry provider-neutral execution briefs to runtime adapters

Tracker: https://github.com/dac-plane/dac-plane/issues/99
Branch: `feature/99-task-execution-brief`, from `main` at `e245554`.
Parent: #75. Blocks #91.

## Direction

Restore the product-owned task objective and acceptance criteria required by the architecture before
qualifying a real runtime. Every new submission carries one immutable execution brief through
idempotency, persistence, reads and runtime `start_turn` delivery. Provider, model, credential,
workspace and environment choices remain outside the brief.

The release must still follow ADR-0011: the new binary serves schema 13 before migration 14 and schema
14 afterwards. Genuine legacy tasks may omit a brief; no migration invents one.

## Implementation

1. **Decision and contracts**
   - [x] Add ADR-0013 and the bounded execution-brief domain value.
   - [x] Require objective and ordered acceptance criteria on Surface API task submission.
   - [x] Add a typed runtime-start argument contract and schema agreement coverage.
2. **Persistence and dispatch**
   - [x] Persist the brief in submission outcomes and schema-14 task columns.
   - [x] Read schema-13 briefs from immutable command results and preserve legacy omission.
   - [x] Carry the brief through ordinary, governed, rework and migration-restart starts.
3. **Rollout compatibility**
   - [x] Add production migration 14 and declare the release range 13–14.
   - [x] Renumber the isolated #74 rollout probe to 15 and declare its range 13–15.
   - [x] Update operational evidence only with newly observed results.
4. **Verification and delivery**
   - [x] Run contract, Surface API, stack, operations and complete CI checks.
   - [x] Review the complete diff for correctness, architecture, security and performance.
   - [x] Commit and open a focused PR with `Closes: #99`.

## Interface

`POST /v1/tenants/{tenant}/tasks` adds required `objective` and `acceptance_criteria`. The task record
exposes the paired fields when available. Runtime `start_turn` arguments carry the same brief; a
migration restart additionally carries the existing recovery request.

## Limits

No repository reference, runtime profile, model choice, secret, provider-specific prompt, environment
contract or qualification claim is added. Issue #91 owns the first real runtime adapter and evidence.

## Verification

```text
just test-contracts
just test-surface
just test-stack
just test-operations
just ci
git diff --check
```
