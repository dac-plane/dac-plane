# E2B Cloud qualification evidence

Status: live qualification passed for the observed E2B Cloud account and tested adapter path. The adapter is not enabled for production task assignment; #102 owns that integration. This is a bounded qualification, not a provider-wide availability or production guarantee.

## Provider and software pins

- Provider target: E2B Cloud, managed service; qualification uses the documented control API v2 at `https://api.e2b.app`.
- E2B Cloud backend revision: not exposed or customer-pinnable by E2B. The API surface and response metadata are recorded at run time; no immutable server build claim is made.
- Qualification client pins: `@e2b/cli` 2.23.0 and E2B JavaScript SDK 2.61.0 (`qualification/package-lock.json`).
- DAC adapter revision and live run timestamp: `957567f2e421ee9134a8f52d69faa1402a8365e0`, observed 2026-10-30 07:59:54 UTC.

## Deterministic evidence

Previously observed on 2026-10-29: Go `go1.26.10 darwin/arm64`; Docker client/server `29.8.2` under OrbStack; deterministic E2B fake-provider tests, adapter-kit conformance, `just ci-fast`, and a local adapter image build passed at the then-current Embed implementation. The local image digest `sha256:cb4ddab294faf8a9949925cd9b5da672fc88771d538cc9aa3b0c2a443ba899d9` identifies that prior local adapter build only; it is not a provider image digest and is not evidence about E2B Cloud.

The previous Linux/KVM Embed gate was never run. That deployment choice has been superseded; Embed source/runtime revisions and service image digests do not apply to the managed Cloud qualification.

After the Cloud alignment changes, `just test-e2b-adapter`, the focused `TestCursorRoundTripAndRejection`, the tagged qualification compile, `just lint-workflows`, `just ci-fast`, and `git diff --check` passed locally on 2026-10-29. The previous PR integration run failed because its malformed-cursor fixture was actually a valid 12-character stream tag; the test now uses a malformed 13-character tag, and the focused test passes without changing parser behavior.

## Live E2B Cloud evidence

Observed on 2026-10-30 using `just qualify-e2b-environment` with `APIKEY_E2B` loaded from the ignored local `.env` file, Node `v22.19.0`, npm `10.9.3`, Go `go1.26.10 darwin/arm64`, E2B CLI `2.23.0`, SDK `2.61.0`, and API v2. The live test took 23.79 seconds (24.058 seconds for the Go package). The first local invocation on the prior day stopped at preflight because system Node was `v22.16.0`; selecting the already downloaded Node `v22.19.0` allowed the live test to run. The key was not printed or recorded.

An initial probe used a raw TCP socket to `1.1.1.1:443`; this was invalid egress evidence because it sent no TLS ClientHello or HTTP request. The resulting socket handshake is discarded. An API-shape probe confirmed Cloud rejects `::/0` as an invalid denied CIDR. Earlier raw-socket runs cleaned up all eight adapter-created sandboxes; each cleanup cancelled the sandbox and polled until GET returned 404. A separate SDK comparison made the same raw-socket mistake and is also discarded.

The corrected qualification follows E2B's own SDK tests: it first runs an HTTPS request from the host to `1.1.1.1`, verifies `curl` exists in the sandbox, then attempts HTTPS from the sandbox and accepts only connection-level curl failures with no HTTP status. The live run passed. It created two distinct sandboxes and reported `envdVersion=0.10.0`; filesystem and positive/negative process isolation passed. It read each sandbox's stored `allowInternetAccess=false` and `denyOut=["0.0.0.0/0"]`, observed both, replayed the original mapping in a child adapter process, forcibly killed that process, and launched a second process which verified the same mapping. It then stopped both sandboxes, observed them absent, and verified cleanup through provider GET returning 404. No full sandbox IDs are retained in evidence. An earlier raw TCP socket probe was invalid because it did not send TLS or HTTP; its apparent egress result is discarded. An API-shape probe confirmed Cloud rejects `::/0` as an invalid denied CIDR. Earlier raw-socket runs cleaned up all eight adapter-created sandboxes. The local E2B credential was read only from `.env`; neither the value nor raw environment was captured.

Checked the E2B repositories with both the GitHub search MCP and the public issue tracker. No public issue describing general public IPv4 HTTPS egress succeeding under `allowInternetAccess=false` / `denyOut=0.0.0.0/0` was found. The closest SDK issue, [e2b-dev/E2B#1957](https://github.com/e2b-dev/E2B/issues/1957), asks whether policy readback proves effective enforcement; E2B closed it as not planned and directed the reporter to support. [e2b-dev/runtime#3625](https://github.com/e2b-dev/runtime/issues/3625) concerns sandbox-local MMDS at `169.254.169.254`, an explicitly special path, not public Internet egress. E2B's own [network tests](https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/tests/sandbox/network.test.ts) demonstrate deny behavior with HTTPS requests. The implementation follows this test pattern now.

The 2026-10-30 observed run passed the two-sandbox, filesystem/process isolation, deny-egress, observation, process kill/restart replay, stop/absence, and cleanup checks on the recorded adapter revision. Deterministic tests separately cover lost create replies, cancellation behavior, and observation sequence persistence. The GitHub pull request CI is the remaining merge gate; check its current status separately because it can change after this note is committed.

## Limits

The 2026-10-30 live run predates the explicit sandbox lifetime. It ran for under 25 seconds, so E2B's 300-second default never took effect. The qualification now requests ten minutes and asserts the provider's reported `startedAt` to `endAt` span; that assertion has not yet run against E2B Cloud.

This is a bounded qualification of the E2B Cloud account and managed API behavior observed during one run, not an immutable E2B backend build, E2B BYOC/Embed, multi-region availability, task assignment/cancellation integration (#102), provider-wide availability, throughput, or cost guarantee. E2B Cloud usage may incur charges beyond available credits.
