# Issue #92 — Qualify E2B Embed as the first execution-environment provider

Tracker: https://github.com/dac-plane/dac-plane/issues/92
Branch: `feature/92-e2b-environment-qualification`, from merged #103.
Parent: #75. Depends on #72 and #73. #102 owns task environment assignment and cancellation orchestration.

## Provider pin and qualification boundary

The plan named E2B Runtime Embed release `2026.30` at `f32ee8a2a50052f32e3632ceb451111a98dd5104`, but that commit does not contain `embed/`. Use the exact Embed package source revision `7a69f45621f864495695e698dc5a2b93a56d2d52`; its Compose `.env` pins the runtime source used to build its service images at `7278c2a380767c9989da73cf4c04b1af1b32da18`. Record both revisions and every resulting OCI digest. This is a commit-pinned Embed package, not a claim that release `2026.30` contains Embed.

Use E2B Embed on an isolated disposable Linux/KVM host. Its Compose deployment uses host networking and the unauthenticated orchestrator API on port 5008; all host ports except the explicitly required trusted API path must be firewalled, with 5008 and the egress proxy ports unreachable externally. This qualifies one host only.

The E2B sandbox-create idempotency feature is disabled by default in the pinned offline Embed setup. Persist DAC operation identity in sandbox metadata, reconcile by querying that identity before any retry, and do not advertise `operation.replay` unless provider-side idempotency and crash-window tests prove the guarantee. Unknown or ambiguous outcomes remain errors; never issue an unverified second create.

## Implementation

1. Add independent `adapters/e2b` Go module and executable using `adapterkit` only. Keep provider protocol parsing local to the adapter; do not import `internal/` or change public contracts.
2. Implement the existing environment methods: provision an E2B sandbox for the fenced migration target-environment stage, stop the bound sandbox for source/target cleanup, and observe a sandbox by provider resource reference. Persist operation fingerprints, sandbox mappings, state, and monotonic observation sequence in bbolt.
3. Add deterministic fake-HTTP tests for strict provider responses, operation replay, create lost-reply reconciliation, ambiguous outcomes, adapter restart, stale/duplicate observations, cancellation response, and credential redaction. Do not advertise replay absent a tested provider guarantee.
4. Add a pinned Embed qualification runner that fetches `7a69f45621f864495695e698dc5a2b93a56d2d52`, validates Linux/KVM and Docker Compose prerequisites, records OCI image digests, and documents private-host firewall requirements. Keep generated keys, data, and logs under `.work/`.
5. Exercise two isolated sandboxes, filesystem/process separation, configured egress, stop/observe, adapter restart/reconciliation, and cleanup on the real host. Record exact commands, revisions, image digests, versions, durations, failures, resource cleanup, and provider disposition in an evidence note. If Linux/KVM is unavailable, leave this gate explicitly unverified and the provider off/shadow.
6. Run focused adapter/conformance checks during development, then `just ci-fast`, provider qualification when a suitable host exists, `just ci`, and `git diff --check` for final acceptance.

## Limits

This issue qualifies only the pinned single-host E2B Embed deployment. It does not qualify E2B Cloud/BYOC, multi-host scaling, high availability, ordinary task-to-environment assignment, or task cancellation orchestration (#102). No production availability, throughput, RPO, or RTO claim follows.
