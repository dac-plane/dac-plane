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

Observed on 2026-10-29 using the configured local `APIKEY_E2B` secret, Node `v22.19.0`, npm `10.9.3`, Go `go1.26.10 darwin/arm64`, E2B CLI `2.23.0`, SDK `2.61.0`, API v2, and adapter commit `af2842839b1cec1713d2da31fc60920aaa381f84`. The first local invocation stopped at preflight on the system Node `v22.16.0`; after selecting the already downloaded Node `v22.19.0`, the live test reached Cloud.

Four live adapter attempts created two distinct sandboxes apiece and reported `envdVersion=0.10.0`; filesystem and positive/negative process-isolation probes passed before the egress gate. The first attempt with `allow_internet_access=false` allowed a direct connection to `1.1.1.1:443`. The second added `network.denyOut=["0.0.0.0/0"]`; the provider GET response confirmed `allowInternetAccess=false` and that deny rule, but the same direct connection still succeeded. An API-shape probe confirmed Cloud rejects `::/0` as an invalid denied CIDR. The third attempt also sent an empty `network.allowOut` list; the direct connection still succeeded. The fourth added a loopback-only `allowOut` and an exact `1.1.1.1/32` deny alongside the broad deny; the direct connection still succeeded. Each failed adapter run reported no cleanup errors; the cleanup path cancels both created sandboxes and polls until GET returns 404.

At 2026-10-29 15:40 UTC, a comparison using the pinned E2B JavaScript SDK `2.61.0` created a sandbox with the same deny policy, called `updateNetwork` with a loopback-only allow-list plus the broad and exact deny rules, and read it back (`allowInternetAccess=false`, `allowOut=["127.0.0.1/32"]`, deny rules present). A direct sandbox command still connected to `1.1.1.1:443`. This uses E2B's own SDK path and shows the failure is not caused by DAC's HTTP serialization or adapter command execution. The probe sandbox was killed by the SDK. None of these runs reached stop/restart reconciliation; adapter cleanup did verify all eight adapter-created sandboxes absent after the failing gate.

The provider's published API schema says `allow_internet_access=false` is equivalent to denying `0.0.0.0/0`, and the provider's response confirms the requested configuration was stored. Live execution contradicts the expected effect through both DAC's adapter and the pinned provider SDK. No DAC-side fix is justified by this evidence; E2B must explain whether the account needs a feature enabled or remediate the observed Cloud enforcement failure. The egress isolation gate fails and E2B Cloud remains off/unqualified. Do not treat the Actions secret's presence as a successful CI qualification; the manual workflow has not run because its definition is not yet on the default branch. Never record the API key, raw environment, or full sandbox IDs.

Checked the E2B repositories for a matching report. No public issue describing general public IPv4 egress still succeeding under `allowInternetAccess=false` / `denyOut=0.0.0.0/0` was found. The closest SDK issue, [e2b-dev/E2B#1957](https://github.com/e2b-dev/E2B/issues/1957), asks whether policy readback proves effective enforcement; E2B closed it as not planned and directed the reporter to support. [e2b-dev/runtime#3625](https://github.com/e2b-dev/runtime/issues/3625) concerns sandbox-local MMDS at `169.254.169.254`, an explicitly special path, not public Internet egress. E2B's public docs still describe `allowInternetAccess=false` as blocking outbound traffic. This mismatch should be raised with E2B support for account/service diagnosis, then the same live probe should be rerun.

The qualification must observe two distinct Cloud sandboxes, filesystem separation, an active uniquely marked process isolated between them, a successfully executed but denied outbound connection while the host can reach the probe endpoint, stop/absent observations, journal close/reopen reconciliation without a duplicate create, and cleanup of both sandboxes. The egress gate currently fails: the connection succeeds despite the stored deny-all policy. Other later live gates remain untested because the test stops at that failure. Any missing/unclassified probe or cleanup failure keeps the provider off/unqualified.

## Limits

This is a bounded qualification of the E2B Cloud account and managed API behavior observed during a specific run, not an immutable E2B backend build, E2B BYOC/Embed, multi-region availability, task assignment/cancellation integration (#102), provider-wide availability, throughput, or cost guarantee. E2B Cloud usage may incur charges beyond available credits. No live provider pass or production qualification claim has been recorded yet.
