# Issue #102 — Bind environments to task attempts

Tracker: https://github.com/dac-plane/dac-plane/issues/102
Branch: `feature/102-environment-attempt-binding`, from `origin/main` after #104.
Parent: #75 / #65. This issue supplies task-level environment lifecycle orchestration for #101.

## Contract and assignment

- Require `environment_provider_id` on task submission and rework, alongside the selected runtime provider. Validate that the provider has a trusted environment-role registration.
- Persist an immutable, provider-neutral environment assignment and provision-operation identity on each new Attempt. Existing Attempts without an assignment remain readable; never infer one for legacy records.
- Include the assignment and current observed environment state in submission and Attempt reads. Update the Surface schemas and OpenAPI contract.
- Use the existing `provision`, `stop` and `observe` environment methods, opaque native resource bindings and monotonic observations. Define any missing provider-neutral provision result required to carry the native resource reference and readiness evidence.
- Extend `RuntimeStart` with a provider-neutral environment assignment/reference so a runtime actually receives the environment prepared for its Attempt; keep Codex/E2B identifiers and credentials out of the contract.

## Orchestration and recovery

- In the submission transaction, persist both provider operations and the environment provision dispatch, but do not dispatch runtime start yet.
- Bind the provisioned native resource only to the assigned Attempt and operation. Dispatch the already-recorded runtime start only after accepted provisioning and a current `running` observation are durable.
- Preserve operation identity through retries and restart. An uncertain provision outcome must be reconciled or replayed against the same operation; it must never create a second environment just because a reply was lost.
- On cancellation, persist independent runtime-interrupt and environment-stop intents. Apply terminal cancellation and release the reservation only after both stop observations are durably accepted. A provision that may have been delivered is treated as potentially live and must be reconciled/stopped.
- Extend the already-qualified E2B adapter to handle generic task provision/stop requests using its existing metadata reconciliation, without rerunning or expanding provider qualification. This is required for #101 to use the #92 adapter through the new lifecycle.
- Keep all provider calls in the worker and all lifecycle decisions in the application/domain boundary. Add production migration 15 and support schema 14 during rollout as required by ADR-0011; renumber the isolated operations probe to 16 to avoid collision.

## Verification

- Contract and Surface coverage: required/invalid assignment, role-registration checks, replay payload identity, projection and schema/OpenAPI agreement.
- Simulator integration: provision-before-start, readiness gate, lost reply, duplicate and stale observations, restart/reconnect, cancellation with either stop arriving first, wrong-attempt binding/stop rejection, and reservation release after convergence.
- Run focused contracts, Surface and tagged PostgreSQL/application/worker integration recipes during development. Final acceptance: `just test-contracts`, `just test-surface`, relevant integration recipes, `just ci`, and `git diff --check`.
- Record exact commands and observed results in the PR. #102 proves simulator orchestration only; real provider qualification is #92 and deployed cross-adapter cancellation qualification is #101.
