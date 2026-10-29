# E2B Cloud qualification evidence

Status: incomplete; the earlier egress failure was caused by an invalid probe, and the corrected live egress probe passed. The process-level adapter kill/restart acceptance case has not yet been demonstrated, so the provider remains off/unqualified.

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

Observed on 2026-10-29 using the configured local `APIKEY_E2B` secret, Node `v22.19.0`, npm `10.9.3`, Go `go1.26.10 darwin/arm64`, E2B CLI `2.23.0`, SDK `2.61.0`, and API v2. The first local invocation stopped at preflight on the system Node `v22.16.0`; selecting the already downloaded Node `v22.19.0` allowed the live test to run.

An initial probe used a raw TCP socket to `1.1.1.1:443`; this was invalid egress evidence because it sent no TLS ClientHello or HTTP request. The resulting socket handshake is discarded. An API-shape probe confirmed Cloud rejects `::/0` as an invalid denied CIDR. Earlier raw-socket runs cleaned up all eight adapter-created sandboxes; each cleanup cancelled the sandbox and polled until GET returned 404. A separate SDK comparison made the same raw-socket mistake and is also discarded.

The corrected qualification follows E2B's own SDK tests: it first runs an HTTPS request from the host to `1.1.1.1`, verifies `curl` exists in the sandbox, then attempts HTTPS from the sandbox and requires a known curl network-denial result with no HTTP response. The live run passed this gate. It created two distinct sandboxes and reported `envdVersion=0.10.0`; filesystem and positive/negative process isolation passed. It read each sandbox's stored `allowInternetAccess=false` and `denyOut=["0.0.0.0/0"]`, observed both, closed and reopened the journal, replayed the original mapping without creating a duplicate, stopped both, observed them absent, and verified cleanup through provider GET returning 404. The corrected run completed in 16.56 seconds before adding explicit cleanup summary logs. The journal close/reopen verifies persisted mapping recovery, but it did not kill and restart the adapter process; that acceptance case remains open. A rerun from this task could not start because `APIKEY_E2B` was absent from its shell environment; no new live result is claimed. Do not treat the Actions secret's presence as a CI qualification; the manual workflow has not run because its definition is not yet on the default branch. Never record the API key, raw environment, or full sandbox IDs.

Checked the E2B repositories with both the GitHub search MCP and the public issue tracker. No public issue describing general public IPv4 HTTPS egress succeeding under `allowInternetAccess=false` / `denyOut=0.0.0.0/0` was found. The closest SDK issue, [e2b-dev/E2B#1957](https://github.com/e2b-dev/E2B/issues/1957), asks whether policy readback proves effective enforcement; E2B closed it as not planned and directed the reporter to support. [e2b-dev/runtime#3625](https://github.com/e2b-dev/runtime/issues/3625) concerns sandbox-local MMDS at `169.254.169.254`, an explicitly special path, not public Internet egress. E2B's own [network tests](https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/tests/sandbox/network.test.ts) demonstrate deny behavior with HTTPS requests. The implementation follows this test pattern now.

The observed corrected run passed the two-sandbox, filesystem/process isolation, deny-egress, observation, stop/absence, and journal close/reopen replay checks. The issue's process-level adapter kill/restart scenario is still required before qualification can be complete. A new run on the final committed revision is also still required.

## Limits

This is a bounded qualification of the E2B Cloud account and managed API behavior observed during a specific run, not an immutable E2B backend build, E2B BYOC/Embed, multi-region availability, task assignment/cancellation integration (#102), provider-wide availability, throughput, or cost guarantee. E2B Cloud usage may incur charges beyond available credits. The live environment gates passed for this account in the corrected run; the final committed revision still needs one recorded qualification run.
