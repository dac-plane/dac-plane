# Issue #92 — Qualify E2B Cloud as the first execution-environment provider

Tracker: https://github.com/dac-plane/dac-plane/issues/92
Branch: `feature/92-e2b-environment-qualification`, from merged #103.
Parent: #75. Depends on #72 and #73. #102 owns task environment assignment and cancellation orchestration.

## Provider decision and qualification boundary

Use E2B Cloud's managed sandbox API. E2B provisions and operates the Firecracker hosts; DAC needs no customer-owned Linux/KVM machine, Render service, or self-hosted OpenSandbox cluster. The E2B key is supplied through the existing local `APIKEY_E2B` environment entry and GitHub Actions secret of the same name. Never print or persist the key in the repository, tests, evidence, logs, or artifacts.

Use the documented E2B control API v2 (`https://api.e2b.app`) and its stable metadata lookup for create reconciliation. Pin and record the adapter commit and E2B CLI/SDK versions used by the qualification. E2B does not expose a customer-selectable build revision for E2B Cloud; record the qualification date, API surface, returned service metadata, and that provider server revision is managed and not pinnable. The open-source Embed source/runtime commits are not part of this qualification.

The adapter remains out-of-process and imports `adapterkit` only. Keep E2B-specific API fields inside `adapters/e2b`; do not change DAC's public contracts. E2B Cloud execution is billable after any account credits, so create only two short-lived default sandboxes, use the account's configured budget/usage guard, and always clean them up. Do not switch the provider to live based only on deterministic tests.

## Implementation

1. Keep `adapters/e2b` as an independent Go module with an executable, durable bbolt operation/resource mapping, monotonic observations, and existing provision/stop/observe methods. Correct API request and response mapping against E2B Cloud's official v2 API; persist operation identity in sandbox metadata and reconcile an uncertain create before any retry. Do not advertise `operation.replay` without a provider-side guarantee.
2. Make cancellation durable even if it arrives before provision begins. Report provider 404 observations as the contract's `absent` state. Add regression coverage for both behaviors.
3. Replace the Embed Compose/KVM qualification runner with a Cloud qualification runner that reads `APIKEY_E2B` from the process environment, writes it only to a mode-0600 temporary file when a CLI needs a file, and removes it on exit. Use the production Cloud API origin, the pinned CLI/SDK lockfile, two isolated sandboxes, positive filesystem/process checks, a verified denied-egress probe, stop/observe, journal close/reopen recovery, and deletion/cleanup verification. Do not print command environments or secrets.
4. Add a manually triggered GitHub Actions qualification job that receives the existing `APIKEY_E2B` secret, runs the same repository recipe on a standard Ubuntu runner, uploads only sanitized evidence, and reports a failing result if cleanup or required probes fail. It does not need KVM, Docker, an E2B Embed source checkout, or privileged runner access.
5. Rewrite `adapters/e2b/README.md` and `evidence.md` for the managed service. Separate deterministic results from observed live Cloud results; record date, API surface, package versions, adapter commit, sandbox references in redacted form, elapsed times, isolation/egress outcomes, cleanup, failures, cost/credit boundary, and explicit limits. Keep disposition off/unqualified until every required live scenario passes.
6. Correct the unrelated invalid-cursor fixture in `internal/httpapi/api_test.go` that made the existing PR integration job fail: use a stream tag with an actually invalid shape, preserving parser behavior.

## Verification

- Run `just test-e2b-adapter` and the focused `internal/httpapi` test that previously failed.
- Run the tagged qualification compile and `just ci-fast`, then `git diff --check`.
- Run the live recipe with the configured E2B key, capture its exit code and redacted logs, and verify all created sandboxes are deleted even after failure.
- Run the manual GitHub workflow with the repository secret and same qualification recipe. Do not claim the real provider acceptance criterion until its observed result passes.
- Recheck PR #104's review threads, CI, and description; report the actual current state. #92 remains open until the real provider evidence is complete.

## Limits

This qualifies the E2B Cloud API and account configuration observed during the run, not a pinnable E2B server build, E2B BYOC/Embed, multi-region availability, task assignment/cancellation integration (#102), provider-wide availability, throughput, or cost guarantees. The deterministic fake-provider suite does not replace live evidence.
