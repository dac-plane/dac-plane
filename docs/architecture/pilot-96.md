# Pilot operation evidence — issue #96

Status: **pilot operated.** The published v0.1.0 digests passed the live provider qualification. A laptop rehearsal
ran the runbook end to end with real providers and fixed it wherever a step failed or had to be improvised, and the
pilot then ran from scratch on a separate Mac mini with the corrected runbook. Nothing below is a production or
availability claim.

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

## Release qualification

`just release-qualify v0.1.0` on 2026-11-05, 07:52 to 08:05 UTC, with `DAC_E2B_TEMPLATE_ID=dac-codex-exec`: it pulls
the four images by the digests in the release manifest and runs the [#75](provider-qualification-75.md) and
[#108](codex-containment-108.md) scenarios against them with no local build. The recipe exited 0: all 15 scenarios
passed in 743.5 seconds, all three adapters connected, no simulator ran and no provider key appeared in the Compose
logs. Codex reported app-server 0.165.0.

| Scenario | Time |
|---|---|
| Cancellation: active command | 74.0 s |
| Cancellation: environment adapter down | 51.1 s |
| Cancellation: control plane restart | 40.6 s |
| Cancellation: runtime adapter restart | 39.6 s |
| Cancellation: interrupt lost with its adapter | 38.6 s |
| Cancellation: start lost with its adapter | 31.6 s |
| Cancellation: stop lost with its adapter | 81.5 s |
| Containment: commands run in the sandbox | 35.8 s |
| Containment: credentials stay out of the workload | 38.7 s |
| Containment: egress stays denied | 91.6 s |
| Containment: stopping the environment alone ends the work | 60.3 s |
| Containment: runtime restart | 32.1 s |
| Containment: a result is read from the sandbox | 42.2 s |
| Containment: an open approval survives a control plane restart | 58.4 s |
| Containment: a runtime restart ends an open approval | 27.0 s |

## Pilot host

A Mac mini (Apple M4, 24 GB, macOS 26.6.2) on the company network, operated from the laptop over SSH with key
authentication. Colima 0.10.3 (`vz`, 4 CPUs, 8 GB, 60 GB disk), Docker Engine 29.5.2 (linux/aarch64), Docker Compose
5.7.0, OpenSSL 3.6.2, jq 1.7.1. The Compose files and scripts were copied from this branch (`ed03645`, plus the
bootstrap admin step below), and the release manifest from the laptop, where `gh release download` had fetched it;
its SHA-256 matched on both hosts. The Mac mini reached ghcr.io, quay.io, Docker Hub, Camer and E2B directly. Runs
took place on 2026-11-05 between 09:57 and 10:16 local time.

| Scenario | Result | Time | Observed |
|---|---|---|---|
| Configure | Pass | 117 s, then 4 s | First run pulled every image and provisioned; the second reused every identifier, its policy update changed 0 rows. |
| Bootstrap admin replaced | Pass | 4 s | `configure-pilot.sh` created `pilot-admin`, deleted `bootstrap-admin` and rotated the admin password file. `bootstrap-admin` then gets `invalid_grant`; `pilot-admin` is the only `master` user and holds `admin`. A later run signed in as `pilot-admin`. |
| Deploy and verify | Pass | 56 s | All services up and healthy; `dac-plane version` v0.1.0, schema 13 to 15; `health ready`; all three adapters connected. |
| Lifecycle | Pass | 44 s | One approval, task `succeeded`; the committed artifact is 6 bytes with digest `sha256:0628e754…12bd7`, which is the SHA-256 of `pilot\n`. |
| Cancellation | Pass | 55 s, 12 s after the cancel | `runtime_stopped`, `environment_stopped`, `cancelled`. E2B listed 0 sandboxes before, 1 (`dac-codex-exec`) during the run and at the cancel, and 0 after. |
| Remote access | Pass | | From the laptop through an SSH tunnel (PILOT.md step 7): the owner's token, fetched through the tunnel, gets 200 on tenant events; no token gets 401. On the host both ports listen on 127.0.0.1 only. |
| Backup and restore | Pass | 43 s backup, 48 s restore | The PILOT.md commands as written: no warnings, tasks identical, three adapters reconnected, no `dac-plane` exit. A new task succeeded in 44 s and `pilot-admin` still signed in. |
| Binary-first roll | Pass | 3 s control plane, 1 s adapters | Schema 15 before and after `migrate up`; `health ready`. |
| Runtime client secret rotation | Pass | | Regenerated in Keycloak: old secret 401, new 200. Written to its file, `codex-adapter` recreated. Over the next 330 seconds, past a token renewal, it connected twice with no secret-file error and no failed connection. A task then succeeded in 41 s. |

At the end every service was up, every health-checked service healthy, and no container had restarted. The
TLS, S3 key and attach key rotations were exercised on the laptop only.

## Rehearsal scenarios

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
- **The bootstrap admin step was not a procedure.** On the Mac mini, the runbook's "create a permanent admin with the
  admin console or `kcadm.sh`" named no commands, and `configure-pilot.sh` signed in as `bootstrap-admin`, so it would
  have broken once that user was deleted. The script now makes the replacement itself and signs in as the recorded
  admin afterwards.
- **Remote access needed no exposed port.** The runbook only described binding the published ports to a reachable
  address. It now starts with an SSH tunnel, which kept both ports on the Mac mini's loopback.
- **Colima's Compose plugin was not found.** Homebrew's `docker-compose` needs `cliPluginsExtraDirs` in
  `~/.docker/config.json`; the prerequisites now say so.

## Observations

- **Every artifact stage passes through `send_unknown`.** The worker records a stage operation as uncertain in the
  same transaction that dispatches it, before the send (`internal/worker/artifact.go`), and the reply reconciles it.
  This is the stage replay contract of the [#26 note](artifact-results-26.md), not a lost reply.
- **A Keycloak restart reads as refused credentials.** The adapter kit reports every failed token fetch as `the token
  endpoint refused the client credentials`, including a connection failure while Keycloak restarts. Fixed on this
  branch: the kit keeps that message for HTTP 400 and 401 and reports an unreachable endpoint, another status or an
  unusable answer as such (`adapterkit/clientcredentials.go`). The v0.1.0 images the pilot runs still carry the old
  message, so PILOT.md lists it as a symptom that is a stop condition only once Keycloak is healthy again.

## Not covered

- The E2B and model keys, the owner secret and the database passwords were not rotated; the first two need new keys
  from the providers.
- An upgrade to a newer release: v0.1.0 is the only one ([#116](https://github.com/dac-plane/dac-plane/issues/116)).
- Continuous operation: the pilot ran for about 20 minutes of scenarios, not a soak.

## Limits

- One host, one PostgreSQL primary, logical backups taken while no task runs. No high availability, failover or
  measured RPO and RTO.
- A macOS host running Docker in a Linux VM, not a Linux server.
- The first policy is set by a direct update, because v0.1.0 has no provisioning command for it
  ([#115](https://github.com/dac-plane/dac-plane/issues/115)).
- The pilot overlay and runbook come from this branch, not from the v0.1.0 Compose bundle, which predates them.
- No metrics endpoint: logs and health only.
- The provider limits of the [#75](provider-qualification-75.md) and [#108](codex-containment-108.md) notes.
