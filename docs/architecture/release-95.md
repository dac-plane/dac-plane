# Release evidence: v0.1.0

Evidence for [#95](https://github.com/dac-plane/dac-plane/issues/95). It records what was observed, including failures.
The release section is still empty because no final tag has been pushed.

## Live provider runs before the first tag

Host: one arm64 machine. Images: local builds of the distroless targets, built before the final `main` commit.

| Run | Scope | Result |
|---|---|---|
| 6 | Full `qualify-providers` | 14 of 15 scenarios passed. `runtime_adapter_restart` timed out: the provision became `uncertain` and the attempt stayed in `provisioning`. |
| 7.1, 7.2, 7.3 | `runtime_adapter_restart` alone, same images | All passed, in 37 to 44 seconds. |

The failure in run 6 matches a known limit from the [#75 note](provider-qualification-75.md). It was the first time that
limit was seen live. The adapter logged a failed provision with durability `unknown` about 48 seconds after dispatch, which
is what a create outlasting the 45-second client timeout looks like. E2B's own reply is not logged, so the cause is
not proved.

## Cancelling a stalled provision

Run 6 could not show whether cancelling ends a stall, because the test stopped as it cancelled. So this was forced, on
the same images and the live E2B API, with a one-off local proxy between the E2B adapter and E2B that delayed creates.
The proxy and its test were not committed.

| Case | Provision | Task cancelled after | Sandboxes left |
|---|---|---|---|
| Create reaches E2B, reply held for 60 seconds | `uncertain` after 35 seconds. The adapter found the sandbox by lookup and the attempt went on to start. | 15 seconds | 0 |
| Create never reaches E2B (lifetime set to 1 minute) | `uncertain` after 31 seconds, and no sandbox existed. | 5 minutes 4 seconds | 0 |

In both, the attempt ended `terminated` with both stops recorded and the reservation released. No credential appeared
in the logs.

What this means:

- A slow reply alone does not stall a task if the sandbox can be found. That points run 6 at a create that E2B had not
  listed when the adapter looked, but does not prove it.
- A cancelled lost create converged about 6 minutes after it was sent, which is the lifetime plus the 5-minute margin
  in the adapter. On the reference overlay's 15-minute lifetime that gives up to 20 minutes. This duration is calculated,
  not observed.
- `TestCancelConvergesAfterUncertainProvision` now covers the control-plane side of this without a provider.

Not tested: a create that E2B accepts after the adapter has given up, with the sandbox appearing during the wait.

## Findings handled before the first tag

- CodeQL reported 14 alerts on its first run. The 10 integer conversions came from two parsers that accepted values
  above the `bigint` range. Both now reject them. The remaining 4 were dismissed as false positives, each with its
  reason recorded on the alert.
- SonarCloud's 10 alerts were all in `architecture-explore/`, which is now excluded from analysis.

## Rehearsal

Both runs built on GitHub's amd64 and arm64 runners and published to `ghcr.io/dac-plane/dry-run`.

| Tag | Commit | Run | Result |
|---|---|---|---|
| `v0.1.0-rc.1` | `d4f7fd7` | [41695145795](https://github.com/dac-plane/dac-plane/actions/runs/41695145795) | CI, security, reproducible builds, scans, push and attestations passed. The clean room failed when its stack started, and no draft was created. |
| `v0.1.0-rc.2` | `87a8023` | [41700882873](https://github.com/dac-plane/dac-plane/actions/runs/41700882873) | Every job passed: 25 of 25, ending in a draft prerelease. |

The rc.1 failure was a bug in the clean room, which passed `--scale sim-adapter=0` for a service that the stack left
disabled. Running its later stages locally then found that the module proxy's first fetch of a new kit version can
time out, so the clean room now retries it. The job's time limit was also raised to 45 minutes as a precaution; in CI
the clean room took about 2 minutes.

Both tags were signed, and GitHub reported them verified. The attest action warned that it could not create a storage
record, an optional metadata entry nothing here uses. The attestations were created and verified regardless, and
that step is now turned off.

Dropped by decision of the project owner, to save time:

- The injected failures (unsigned tag, mismatched kit version, vulnerable dependency, planted fake secret). The gate's
  stops on those conditions have not been seen to fire.
- `just release-qualify` against the rehearsal's digests. Published images have passed the clean room but have not run
  against live providers. The live runs above used local arm64 builds of an earlier commit.

## Release

Not yet run. To record: the workflow run, image digests, and `just release-verify` with no registry login.
