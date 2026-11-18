# Release evidence: v0.4.0

Evidence for [#162](https://github.com/dac-plane/dac-plane/issues/162). It records what was observed, including failures.

## No rehearsal or live qualification

The maintainer released v0.4.0 without an `-rc` rehearsal and without `just release-qualify`, to make artifact
downloads available to the reference application (#97) the same day. Steps 2 and 3 of the
[release procedure](../releases/README.md#cutting-a-release) did not run, so no live Codex and E2B run qualified these
images. The change since v0.3.0 is the artifact read and download path (#153, ADR-0025); outside it, the runtime and environment
adapters and the adapter kit changed only in comments and error and log wording. The artifact store stack test downloads a committed artifact through
SeaweedFS and the S3 adapter on every pull request and in the release gate, with simulated runtime and environment.

## Release

`v0.4.0` and `adapterkit/v0.4.0` are signed tags on `0cb1d04`, and GitHub reports `v0.4.0` verified.
[Run 44268900529](https://github.com/dac-plane/dac-plane/actions/runs/44268900529):

| Attempt | Result |
|---|---|
| 1 | Failed in the gate. `TestServeExposesTLSListenersAndReadiness` in the integration job reported only "never ready" within 30 s under the race detector. The same commit had passed that job on `main` (run 44268673414). Nothing was built or published. |
| 2 | The failed job rerun alone on the same tag. All 26 jobs passed, and the [GitHub Release](https://github.com/dac-plane/dac-plane/releases/tag/v0.4.0) was published at 08:59 UTC. |

The cause of the first attempt is not known: the test did not say which readiness check was missing. #164 makes it
report the last `/readyz` answer.

| Image, under `ghcr.io/dac-plane/` | Digest |
|---|---|
| `dac-plane` | `sha256:00f07c111e291a9eac235312a1d9dd4f9da505d09fdf243c0c0fdfd53fe792ec` |
| `dac-codex-adapter` | `sha256:580414243d6bda9b4584d32d6fa17fc4fb445447de3b498aa29e775c93ca45c7` |
| `dac-e2b-adapter` | `sha256:cd53930ee89c78c12281e12731e63d09e6257873f3e0dd382f8b442472ff6dad` |
| `dac-s3-adapter` | `sha256:2ffb3ef044b268c75990d3fb8a2a09e331985f76934c1a83f7fb30797af9733b` |

## A stranger's check

From a checkout of the tag on one arm64 machine, `just release-verify v0.4.0`:

| Run | Result |
|---|---|
| 1 | Did not start: the local Docker daemon was not running. |
| 2 | Failed before any check of the release: an unrelated local stack held host port 18443, which the clean room publishes. |
| 3 | Same as run 2: the command meant to stop that stack failed first. |
| 4 | Passed, after the stack was stopped. It pulled all four images and verified their labels and provenance, started the control plane and migrated it, built the external adapter against the published kit and passed all 14 conformance scenarios, and built every in-tree module without the `replace`. |

The upgrade test's pin moves to v0.4.0 in the same change as this record.
