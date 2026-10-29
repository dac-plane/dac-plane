# E2B Cloud qualification evidence

Status: incomplete; provider remains off/unqualified until all live criteria pass. This note records observed evidence and distinguishes it from the earlier self-hosted Embed attempt.

## Provider and software pins

- Provider target: E2B Cloud, managed service; qualification uses the documented control API v2 at `https://api.e2b.app`.
- E2B Cloud backend revision: not exposed or customer-pinnable by E2B. The API surface and response metadata are recorded at run time; no immutable server build claim is made.
- Qualification client pins: `@e2b/cli` 2.23.0 and E2B JavaScript SDK 2.61.0 (`qualification/package-lock.json`).
- DAC adapter revision and live run timestamps: populated from the actual run; not yet observed.

## Deterministic evidence

Previously observed on 2026-10-29: Go `go1.26.10 darwin/arm64`; Docker client/server `29.8.2` under OrbStack; deterministic E2B fake-provider tests, adapter-kit conformance, `just ci-fast`, and a local adapter image build passed at the then-current Embed implementation. The local image digest `sha256:cb4ddab294faf8a9949925cd9b5da672fc88771d538cc9aa3b0c2a443ba899d9` identifies that prior local adapter build only; it is not a provider image digest and is not evidence about E2B Cloud.

The previous Linux/KVM Embed gate was never run. That deployment choice has been superseded; Embed source/runtime revisions and service image digests do not apply to the managed Cloud qualification.

After the Cloud alignment changes, `just test-e2b-adapter`, the focused `TestCursorRoundTripAndRejection`, the tagged qualification compile, `just lint-workflows`, `just ci-fast`, and `git diff --check` passed locally on 2026-10-29. The previous PR integration run failed because its malformed-cursor fixture was actually a valid 12-character stream tag; the test now uses a malformed 13-character tag, and the focused test passes without changing parser behavior.

## Live E2B Cloud evidence

The local `just qualify-e2b-environment` attempt stopped at its Node preflight: this machine has Node `v22.16.0`, while the pinned E2B CLI requires `22.19.0` or newer. No npm install or E2B API request occurred, and no Cloud resources were created. The manual GitHub workflow pins Node `22.19.0` and uses the existing `APIKEY_E2B` Actions secret; it has not run yet. Record only sanitized evidence: date/time, OS, Go and Node versions, E2B CLI/SDK pins, adapter commit, API surface, returned service metadata, per-scenario outcome and duration, resource cleanup count, failures, and provider disposition. Never record the API key, raw environment, or full sandbox IDs.

The qualification must observe two distinct Cloud sandboxes, filesystem separation, an active uniquely marked process isolated between them, a successfully executed but denied outbound connection while the host can reach the probe endpoint, stop/absent observations, journal close/reopen reconciliation without a duplicate create, and cleanup of both sandboxes. Any missing/unclassified probe or cleanup failure keeps the provider off/unqualified.

## Limits

This is a bounded qualification of the E2B Cloud account and managed API behavior observed during a specific run, not an immutable E2B backend build, E2B BYOC/Embed, multi-region availability, task assignment/cancellation integration (#102), provider-wide availability, throughput, or cost guarantee. E2B Cloud usage may incur charges beyond available credits. No live provider pass or production qualification claim has been recorded yet.
