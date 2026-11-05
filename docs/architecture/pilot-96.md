# Pilot operation evidence — issue #96

Status: **rehearsal done, pilot pending.** The laptop rehearsal below ran the runbook end to end on the published
v0.1.0 digests with real providers. The pilot on the Mac mini (plan step 6) and `just release-qualify v0.1.0` (plan
step 4) have not run yet; their results will be added here. Nothing below is a production or availability claim.

The runbook is [`deploy/compose/PILOT.md`](../../deploy/compose/PILOT.md). The deployment is
[`compose.yaml`](../../deploy/compose/compose.yaml) with [`compose.pilot.yaml`](../../deploy/compose/compose.pilot.yaml)
and [`compose.keycloak.yaml`](../../deploy/compose/compose.keycloak.yaml), configured by
[`generate-pilot-config.sh`](../../deploy/compose/generate-pilot-config.sh) and
[`configure-pilot.sh`](../../deploy/compose/configure-pilot.sh).

## Pins

| Item | Value |
|---|---|
| Release | [v0.1.0](https://github.com/dac-plane/dac-plane/releases/tag/v0.1.0), revision `be8151258111284af94fd5b353fdda29b04aa38c`, schema 13 to 15, protocol 1 |
| `ghcr.io/dac-plane/dac-plane` | `sha256:ad1721deb432feb600783711ed164c8c51cb8540ad26d16dc83c871dede5d8c1` |
| `ghcr.io/dac-plane/dac-codex-adapter` | `sha256:929ece501f0e59b0f82b916623d14a182e357ec7a9bfc9143fdaefc1b6315e30` |
| `ghcr.io/dac-plane/dac-e2b-adapter` | `sha256:2ffc7072681904e2042ce14859440a0af2b2477767f5578482116c213f41e171` |
| `ghcr.io/dac-plane/dac-s3-adapter` | `sha256:0d5a69516b24067c416eb40709da77be8e9a29cb0d324dd1b19168c246810188` |
| Issuer | Keycloak 26.8.1 by digest, production mode, its own PostgreSQL 16.15 |
| Artifact store | SeaweedFS 4.51 behind HAProxy, as in [#93](artifact-store-93.md) |
| Sandbox template | `dac-codex-exec`, as qualified in [#75](provider-qualification-75.md) |
| Model | Camer `deepseek-v4-flash-0731` |

## Rehearsal host

Darwin arm64 (macOS 27.0.1), OrbStack 2.2.5, Docker Engine 29.8.2 (linux/arm64), Docker Compose 5.5.0. The control
plane ran with `DAC_DEPLOYMENT=production`, the Surface API and the issuer published on 127.0.0.1 only. Runs took
place on 2026-11-04 and 2026-11-05.

## Scenarios

The task scenarios used a driver that calls the Surface API with the owner's token: submit, approve the one
`action_approval` the task raises, and accept the revision.

| Scenario | Result | Time | Observed |
|---|---|---|---|
| Bootstrap | Pass | | `configure-pilot.sh` ran twice; the second run reused every identifier and its policy update changed 0 rows. |
| Version and health | Pass | | `dac-plane version` reports v0.1.0, schema 13 to 15; `health ready` succeeds; all three adapters connected, each under its own Keycloak client. |
| Audience separation | Pass | | Owner token: 200 on tenant events. An adapter token or none: 401. Tokens carry `iss` `https://keycloak:8443/realms/dac`, the service account's id as `sub`, and the listener's audience (plus Keycloak's default `account`). |
| Lifecycle | Pass | 47 s | One approval, verification passed, revision accepted, task `succeeded`. The artifact is one SeaweedFS version holding exactly `pilot\n`. |
| Cancellation | Pass | 52 s, 9 s after the cancel | Events in order: `runtime_stopped`, `environment_stopped`, `cancelled`. E2B listed no sandbox afterwards. |
| Backup and restore, run 1 | Pass | 47 s backup, 61 s restore | Databases and the artifact volume only. Tasks and the artifact read back unchanged; a new task succeeded in 40 s. The adapter state volumes were not in the backup, so `down -v` discarded them. |
| Backup and restore, run 2 | Failed, then fixed | 45 s backup, 35 s to failure | With the adapter volumes added: the restore failed because `dac-plane` exited before Keycloak was ready. See failures. |
| Backup and restore, run 3 | Pass | 58 s restore | Same backup, readiness health check: no `dac-plane` exit, tasks identical to before the backup, all three adapters reconnected, adapter state restored with its owner. A new task succeeded in 47 s. |
| Backup and restore, run 4 | Pass | 60 s restore | Final runbook form, with `pilot create` first: no warnings, Compose owns the volumes. |
| Binary-first roll | Pass | 3 s control plane, 2 s adapters | Same digests, as PILOT.md step 10 allows while v0.1.0 is the only release: `migrate status` at 15, roll `dac-plane`, then the adapters, then `migrate up`, still 15. Every adapter reconnected after each roll. |
| Adapter client secret rotation | Failed, then fixed | | The old secret gets 401 and the new 200 at the token endpoint. The adapter failed at its next renewal. See failures. |
| TLS certificate rotation | Pass | 25 s | New serial served by `dac-plane`, Keycloak and SeaweedFS; adapters reconnected within 25 s of Keycloak's start. |
| S3 adapter key and attach key rotation | Pass | 15 s and 1 s | A task then succeeded in 54 s, storing its artifact with the new S3 key and reaching the sandbox with the new attach key. |

## Failures and fixes

Every failure is recorded.

- **Replaced secret files are not seen.** The runbook rotated an adapter secret with `mv`. Compose bind-mounts each
  secret as one file, pinned to the file it started with, so the adapter kept reading the deleted original and logged
  `cannot open the client secret file` at its next token renewal. Fixed in the runbook: recreate the container after
  every credential change.
- **The backup missed adapter state.** The Codex and E2B adapters keep the operations they hold in their own volumes.
  The backup now stops the adapters and the store, dumps both databases and copies all three volumes, then starts
  them again.
- **The control plane raced Keycloak.** Keycloak's health check only opened its TLS port, which is open while Keycloak
  still answers 503 "Bootstrap in progress". `dac-plane` exits when issuer discovery fails, and Compose's `--wait`
  failed on that first exit, although the restarted `dac-plane` became ready 2 seconds later. Run 1 passed only on
  timing. The health check now asks `/health/ready` on Keycloak's unpublished management port.
- **The configuration script could not be run again.** `generate-pilot-config.sh` made `identity.json` read-only and
  then failed to rewrite it on a second run, which certificate and S3 key rotation need. It now makes the file
  writable first, as it already did for the database URLs.
- **The TLS and S3 rotation rows were not exact.** "Issue a new one with the CA" and "restart" became the commands
  that were run.

## Observations

- **Every artifact stage passes through `send_unknown`.** The worker records a stage operation as uncertain in the
  same transaction that dispatches it, before the send (`internal/worker/artifact.go`), and the reply reconciles it.
  This is the stage replay contract of the [#26 note](artifact-results-26.md), not a lost reply.
- **A Keycloak restart reads as refused credentials.** The adapter kit reports every failed token fetch as `the token
  endpoint refused the client credentials`, including a connection failure while Keycloak restarts. Fixed on this
  branch: the kit keeps that message for HTTP 400 and 401 and reports an unreachable endpoint, another status or an
  unusable answer as such (`adapterkit/clientcredentials.go`). The v0.1.0 images the pilot runs still carry the old
  message, so PILOT.md lists it as a symptom that is a stop condition only once Keycloak is healthy again.

## Not covered by the rehearsal

- E2B's sandbox listing was read after the cancellation, not during the run.
- The bootstrap admin was not replaced by a named administrator.
- The E2B and model keys, the owner secret and the database passwords were not rotated; the first two need new keys
  from the providers.
- An upgrade to a newer release: v0.1.0 is the only one ([#116](https://github.com/dac-plane/dac-plane/issues/116)).
- `just release-qualify v0.1.0` and the Mac mini pilot.

## Limits

- One host, one PostgreSQL primary, logical backups taken while no task runs. No high availability, failover or
  measured RPO and RTO.
- A macOS host running Docker in a Linux VM, not a Linux server.
- The first policy is set by a direct update, because v0.1.0 has no provisioning command for it
  ([#115](https://github.com/dac-plane/dac-plane/issues/115)).
- The pilot overlay and runbook come from this branch, not from the v0.1.0 Compose bundle, which predates them.
- No metrics endpoint: logs and health only.
- The provider limits of the [#75](provider-qualification-75.md) and [#108](codex-containment-108.md) notes.
