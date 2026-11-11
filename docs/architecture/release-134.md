# Release evidence: v0.2.0

Evidence for [#134](https://github.com/dac-plane/dac-plane/issues/134). It records what was observed, including failures.

## Live provider run before the first tag

Host: one arm64 machine, 2026-11-09. Images: local builds of `main` with the `set-policy` change, before the Go 1.27.4
and dependency updates.

| Scope | Result |
|---|---|
| Full `qualify-providers` | 13 of 15 scenarios passed. All 8 containment scenarios passed. 5 of the 7 cancellation scenarios passed. |

The two failures were on the E2B side:

- `stop_lost_with_its_adapter`: the sandbox create never answered, and the attempt stayed in `provisioning` until the
  test's 3-minute deadline. This is the known limit "A provision can stall".
- `start_lost_with_its_adapter`: the test's own direct request to E2B, asking whether the sandbox still existed, failed
  with `E2B request failed`. The cause was not established.

## Upgrade from v0.1.0

`just test-operations-upgrade` pulls the v0.1.0 image by digest, lets it migrate and provision at schema 15, rolls both
replicas with a task waiting for approval, migrates, confirms v0.1.0 refuses the new schema, completes the task and
applies `set-policy`. It passed locally on arm64 on 2026-11-11 and runs on every pull request. It uses simulators, not
live providers.

## Live qualification of the rehearsal

Not run. The project owner chose to tag the release once the rehearsal passed, so `just release-qualify v0.2.0-rc.1`
was skipped. The published images have passed the clean room on both architectures but have not run against live
providers; the live run above used local arm64 builds of an earlier commit.

## Rehearsal

Built on GitHub's amd64 and arm64 runners and published to `ghcr.io/dac-plane/dry-run`.

| Tag | Commit | Run | Result |
|---|---|---|---|
| `v0.2.0-rc.1` | `fbcff36` | [43062908801](https://github.com/dac-plane/dac-plane/actions/runs/43062908801) | Every job passed: 26 of 26, ending in a draft prerelease. |

## Release

`v0.2.0` and `adapterkit/v0.2.0` are signed tags on `fbcff36`, the commit rc.1 ran on. In
[run 43065192054](https://github.com/dac-plane/dac-plane/actions/runs/43065192054) 25 of 26 jobs passed on the first
attempt. The clean room failed when it built an external adapter against the published kit: the module proxy and the
checksum database answered "unknown revision" for `adapterkit@v0.2.0`, although the tag had been pushed 13 minutes
earlier.

The likely cause is a lookup made before the tag existed. GitHub's dependency graph job for `main` ran on `fbcff36` at
16:53 UTC, when `go.mod` already required `adapterkit v0.2.0` and the tag was created at 17:08. The proxy began serving
the version at 17:35:58. The rc.1 kit version, which nothing looked up early, resolved at once. The proxy's own logs
are not available, so the cause is not proved.

The clean room's wait for the kit should have covered this, but did not wait at all: it ran `go list -m` inside the
repository checkout, where the `replace` directive answers any version query locally, even for a version that does not
exist. The failed job was rerun once the proxy served the version, and passed. The
[GitHub Release](https://github.com/dac-plane/dac-plane/releases/tag/v0.2.0) was published at 17:38 UTC.

| Image, under `ghcr.io/dac-plane/` | Digest |
|---|---|
| `dac-plane` | `sha256:4ee1bbf220a4499810b7df9b6e37f04c6e2efa825adb7ada817ca952dcc6ae7d` |
| `dac-codex-adapter` | `sha256:84fe8d6c08d56e5297e36fdd3aaf02d77eb1f0d58e542cd8592ce9344a2e9571` |
| `dac-e2b-adapter` | `sha256:af822cc7f4078c976b5f73e84f0d2f8d2b56a40a6ca2ee80f58e833d959c8810` |
| `dac-s3-adapter` | `sha256:8923a3e6b0cfd21630042993e06ea34034872550bbc1947406d5cc29202df62d` |

## Checked as a stranger would

With no registry login, `scripts/release/verify-release.sh` passed against the published release on 2026-11-11, in
about 3 minutes on one arm64 machine. It pulled and verified all four images with their labels and provenance, checked
the version output against the manifest, started the stack and migrated it, built the minimal external adapter
against the published kit and passed conformance, and built every in-tree module without the `replace`.

It ran from `main` at `697fdf2` rather than a checkout of the tag; the script, the kit example and every `go.mod` are
identical between the two. The issuer was published on host port 18090 instead of 18080, which another local service
held; the script reaches the stack only from inside its containers. The first attempt stopped before the stack
started, because `just release-verify` had not downloaded the Compose bundle.

## Findings handled after the release

- The clean room's kit wait now downloads the version from an empty directory, so it waits on the proxy and the
  checksum database, for up to 45 minutes. The job's limit is raised from 45 to 75 minutes to allow for it.
- `just release-verify` downloaded only the manifest, while the script also reads the Compose bundle. It now downloads
  both.
