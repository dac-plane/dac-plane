# Release evidence: v0.3.0

Evidence for [#145](https://github.com/dac-plane/dac-plane/issues/145). It records what was observed, including failures.

## Live runs before the first tag

Host: one arm64 machine, 2026-11-16. Images: local builds of the release preparation branch. Model: Camer's
`deepseek-v4-flash-0731` through Codex app-server 0.165.0.

| Scope | Result |
|---|---|
| `qualify-codex-connector`, first run | Failed. Both scenarios: the model never saw the connector tool. |
| `qualify-codex-connector`, after the starting-attempt fix | Failed. Both scenarios, for the second cause below. |
| `qualify-codex-connector`, after relaying tools as dynamic tools | Passed twice. An `always` grant ran without approval and an `ask` grant waited for one; each artifact held exactly the title the simulated connector returned for the issue number the model asked for. No credential appeared in the stack logs. |
| `qualify-codex-connector`, after review fixes | Passed. Each artifact also held a marker that the simulated connector derives from the connection's credential and that appears in no prompt, so the artifact proves the call ran. Calls made before the attempt executes now wait for it instead of being refused, and the adapter's gateway requests keep the no-redirect policy. |
| `qualify-codex-runtime` | Passed: approval and restart with an exact artifact, a delegated agent's instructions reaching the model, and denial with no effect. |

The two failures were in the path from the gateway to the model, which no deterministic test covered:

- **The gateway refused the thread's startup.** Codex lists a thread's tools when the thread starts, while the attempt is
  still `starting`. The gateway accepted only an executing or waiting attempt and answered 403, so the thread had no
  connector tools. A starting attempt may now list its tools; a call still needs an executing attempt.
  `TestConnectorGatewayServesAStartingAttempt` fails without the fix.
- **The model provider dropped Codex's MCP tools.** Codex 0.165.0 sends MCP tools inside a Responses API `namespace`
  tool, which Camer drops without an error. The Codex adapter now offers the gateway's tools as dynamic tools, which
  reach the model as plain functions, and relays their calls to the gateway
  ([ADR-0024](../adr/0024-relay-connector-tools-to-codex-as-dynamic-tools.md)).

Not run: the GitHub connector against a real GitHub App, which needs a dedicated consented fixture, and a check of the
runtime host's egress, which the reference stack does not confine. Both are known limits in the notes.

## Upgrade from v0.2.0

`just test-operations-upgrade` pulls the v0.2.0 image by digest, rolls both replicas with a task in flight, migrates to
the release schema and delegates a task to an agent afterwards. It runs on every pull request. It uses simulators, not
live providers.

## Rehearsal

`v0.3.0-rc.1` and `adapterkit/v0.3.0-rc.1` are signed tags on `2881e7e`, and GitHub reports both verified.
[Run 43921963527](https://github.com/dac-plane/dac-plane/actions/runs/43921963527) passed all 26 jobs, published to
`ghcr.io/dac-plane/dry-run` and drafted a prerelease.

`just release-qualify v0.3.0-rc.1` then ran from a checkout of the rc tag against the rehearsal's published digests,
with no local build, on one arm64 machine. Model: Camer's `deepseek-v4-flash-0731` through Codex app-server 0.165.0;
environment: E2B Cloud with the `dac-codex-exec` template; artifact store: SeaweedFS.

| Run | Started (UTC) | Result |
|---|---|---|
| 1 | 2026-11-16 17:59 | Did not start. The issuer's host port, 18080, was held by an unrelated local stack. No test ran. |
| 2 | 2026-11-16 18:02 | Did not start, for the same reason. |
| 3 | 2026-11-16 18:03 | Passed, after the unrelated stack was stopped. `TestContainmentQualification` (8 cases) and `TestTaskCancellationQualification` (7 cases) passed in 737 s, the runtime, environment and artifact store adapters connected, and no provider credential appeared in the stack logs. |

The connector qualification is not part of `release-qualify`. It ran on local builds of the same code, as recorded
above, not on the published digests.

## Release

`v0.3.0` and `adapterkit/v0.3.0` are signed tags on `2881e7e`, the commit rc.1 ran on, and GitHub reports both
verified. [Run 43927205195](https://github.com/dac-plane/dac-plane/actions/runs/43927205195) passed all 26 jobs, and the
[GitHub Release](https://github.com/dac-plane/dac-plane/releases/tag/v0.3.0) is published and immutable. This is the
first release published from its tag with nothing to approve, as [ADR-0019](../adr/0019-publish-a-final-release-from-its-tag.md)
describes.

| Image, under `ghcr.io/dac-plane/` | Digest |
|---|---|
| `dac-plane` | `sha256:78a534d2d9ef3213843d4f1d34cc8c666ad01e542eca71f3ea2e10a260ec8656` |
| `dac-codex-adapter` | `sha256:b4149bd24f12f1ceb133fda551133b4e39538f09b8dc733f3b99373f1f5c51c6` |
| `dac-e2b-adapter` | `sha256:92c269f532c36725e4aed660decf38eb405f8ae421c4384b6db5a532b4831770` |
| `dac-s3-adapter` | `sha256:1df8f995e2ace89143c903dc3e8f1ef97127b07ff608e415af8f659e381e7862` |

These differ from the rehearsal's digests: each image records its version, so a rebuild for the final tag is a
different image. The live runs above qualified the rehearsal's images of the same commit, not these.

From a checkout of the tag, `just release-verify v0.3.0` passed in about 24 minutes on one arm64 machine. It pulled all
four images and verified their labels and provenance, started the control plane and migrated it, built the external
adapter against the published kit and passed all 14 conformance scenarios, and built every in-tree module without the
`replace`.

The upgrade test's pin moves to v0.3.0 in the same change as this record.
