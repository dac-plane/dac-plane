# Issue #96 — Operate the first supported dac-plane pilot

Tracker: https://github.com/dac-plane/dac-plane/issues/96
Branch: `feat/96-pilot-deployment`, from `main` at `12c724d`.
Parent: #94. Depends on #95, #75 and #108, all closed. Blocks #97.

## Where the tree stands

Surveyed on 2026-11-04. Each item blocks the pilot or shapes its design.

| Finding | Evidence | Consequence |
|---|---|---|
| v0.1.0 is published and public | Release is final and Latest; all four images pull anonymously by digest; `adapterkit@v0.1.0` resolves on proxy.golang.org | The pilot deploys these digests. Nothing here rebuilds them. |
| The published digests have never run against live providers | #96 body and `release-95.md`: earlier live runs used local arm64 builds of an older commit | `just release-qualify v0.1.0` must pass on the pilot's architecture before the pilot claims qualified providers. |
| The provider overlays only work over the development profile | `compose.codex.yaml`, `compose.e2b.yaml`, `compose.artifacts.yaml` hard-code `DAC_ADAPTER_TOKEN_URL: https://issuer:8443/default/token` and mount `dev_ca`; `qualify-providers` layers them on `compose.dev.yaml` | No production-shaped way exists to run real adapters against an external issuer. The pilot needs one. `#95` listed it as #96 scope. |
| Provisioning exists only as fixed dev jobs | `compose.dev.yaml` runs `provision tenant/principal/bind/register-provider/bind-adapter` with fixed IDs and the dev issuer | The pilot needs an operator procedure with its own IDs, issuer and subjects. `provision qualify-harness` exists for the harness qualification. |
| Tenant policy is set by raw SQL | `qualify-providers` runs `update policy ...` through `psql` as `dac_migrate`; neither `provision` nor Surface API v1 sets policy | The pilot repeats that update as a documented v0.1.0 limit. See decision 6. |
| No metrics | No Prometheus, OpenTelemetry or `/metrics` in `internal/` or `cmd/` | "Log/metric collection" in the issue cannot be met for metrics without new code. See decision 7. |
| Backup and upgrade are proven only with simulators | `operations-74.md`: `pg_dump -Fc` / `pg_restore`, one rolling upgrade with a test-only schema 16, simulators only | The pilot repeats these with real adapters and adds the artifact-store volume, which #74 did not cover. |
| Only one runbook-like text exists | `deploy/compose/README.md` covers upgrade, rollback and TLS reload | Credential rotation (OIDC clients, provider keys, S3 keys, attach key), inspection and incident procedures are missing. |
| Known provider limits still apply | `provider-qualification-75.md` and `codex-containment-108.md` "Limits": a provision can stall in `uncertain`, a lost sandbox takes about a minute to end | The runbook needs a procedure for each limit, and the evidence must not claim more. |

## Decisions

Agreed with the project owner on 2026-11-04.

1. **Two hosts, in order.** A rehearsal on the development laptop (arm64, OrbStack), then the pilot on a Mac mini the
   owner has access to (Apple Silicon, so arm64, Docker in a Linux VM). The Mac mini is set up from scratch with the
   runbook and the release assets only. Its chip, memory, ability to install Docker and Tailscale, and a week of
   uptime are to be confirmed before step 6.
2. **Grok Bot is not a deployment reference.** It is vendor-hosted and the exploration only read its desktop client.
   The deployment model comes from ADR-0009 and ADR-0011; the pilot maps each role (surface, control plane, runtime,
   sandbox, artifact store) to one instance, with E2B renting the sandbox fleet.
3. **A separate pilot overlay.** `compose.codex.yaml`, `compose.e2b.yaml`, `compose.containment.yaml` and
   `compose.artifacts.yaml` are qualification fixtures: they depend on `compose.dev.yaml` for secrets, configs and
   `provision-qualify`, run under the `qualification` profile, and build local targets. Changing them in place would
   risk the existing gates. A new `compose.pilot.yaml` layers on `compose.yaml` alone, requires published images, and
   takes the issuer, trust bundle, provider IDs and credentials from the env file. The adapters already read all of
   these from the environment, so no Go change is needed.
4. **One trust bundle.** Each adapter's `DAC_ADAPTER_CA_FILE` replaces its roots for the control plane and token
   issuer, and the S3 store is reached under the same CA in the qualification profile. The pilot passes one bundle
   holding every private root it needs. Whether the pilot uses a private CA throughout or a Tailscale certificate for
   the Surface API is a runbook choice; the overlay only takes file paths.
5. **Real issuer.** Keycloak in a container on the pilot host, with one confidential client per adapter role and a
   distinct audience for the Surface API and the adapter endpoint. `DAC_DEPLOYMENT=production`, so a development issuer
   is refused.
6. **Tenant policy and the upgrade stay on v0.1.0.** A `set_policy` provisioning record needs schema migration 16
   (the operation list is a check constraint, see `00013_qualify_harness.sql`), and only a new release carries it. Decided
   on 2026-11-04, after comparing how other projects separate upgrade testing from staging: an upgrade is proven in CI
   from the previous release, and a staging or pilot deployment runs the current release and receives the next one
   first (Temporal's self-hosted upgrade guide; Atlas's guide to on-premise migrations). So the pilot runs v0.1.0 only.
   Its first policy is set with the same update `qualify-providers` uses, documented as a v0.1.0 limit. The rolling
   upgrade item is exercised as the binary-first procedure on v0.1.0, and the evidence cites the CI upgrade test for
   version-to-version safety. Follow-ups: `provision set-policy` with migration 16, and an upgrade test from the
   published v0.1.0 digest. The next release rolls out to the pilot first.
7. **Metrics.** Structured logs plus health and readiness only. Metrics are a stated limit with a follow-up issue and
   their own ADR.
8. **Evidence.** `docs/architecture/pilot-96.md`, with commands, versions, digests, durations and limits. Host names
   and endpoints that are not meant to be public stay out.

## Steps

1. **Add `compose.pilot.yaml`.** Codex (contained), E2B, SeaweedFS behind HAProxy and the S3 adapter over the base
   stack, every image by digest with no `build:`, and a `provision` tool service under a `tools` profile so the operator
   runs `docker compose run --rm provision ...`.
   Verify: `docker compose config` of `compose.yaml` plus `compose.pilot.yaml` resolves with no `issuer:8443`, no
   `dev_ca`, no simulator and no `build:`; it refuses to start without each required setting; `just ci-fast` and
   `just test-artifact-store-stack` are unaffected.
2. **Open the follow-ups.** `provision set-policy` with migration 16, and an upgrade test from the published v0.1.0
   digest. Neither blocks the pilot.
3. **Write the runbook.** Done on 2026-11-04: `deploy/compose/PILOT.md`, `generate-pilot-config.sh`,
   `configure-pilot.sh` and `compose.keycloak.yaml` (Keycloak 26.8.1 by digest). On the laptop, with no provider
   credential: the bootstrap ran twice with identical identifiers, the v0.1.0 control plane served in production
   mode, the S3 adapter connected under its Keycloak identity, the owner's token read tenant events (200) while an
   adapter token and no token were refused (401), and dac-plane stayed ready through a Keycloak restart. Covered: host prerequisites, the env file, secret layout, the trust bundle,
   Keycloak realm and clients, the ordered provisioning commands, deploy, migrate, inspect, backup and restore of
   PostgreSQL and the artifact volume, binary-first upgrade and rollback (ADR-0011), rotation of every credential, and
   one procedure per known limit. Each procedure states its stop conditions.
4. **Qualify the published digests on the laptop.** `just release-qualify v0.1.0`.
   Verify: the run passes and the log credential scan finds nothing.
   Done on 2026-11-05: 15 of 15 scenarios in 743.5 seconds, exit 0; recorded in `docs/architecture/pilot-96.md`.
5. **Rehearse on the laptop.** Follow the runbook from an empty directory with the release assets: deploy, a real
   lifecycle through the Surface API, cancellation, backup and restore, an upgrade, rotation. Fix the runbook wherever
   a step had to be improvised.
   Done on 2026-11-04 and 2026-11-05; results and the four fixes it forced are in `docs/architecture/pilot-96.md`.
6. **Run the pilot on the Mac mini.** The same runbook, from scratch. Leave it running, reach it from the laptop over
   Tailscale, and repeat the lifecycle and operations scenarios there.
   Verify: `health ready` everywhere, `dac-plane version` reports v0.1.0 and schema 13 to 15, all three adapters
   connected with their own identities, and the event stream, audit trail and E2B's sandbox listing agree.
7. **Record the evidence.** `docs/architecture/pilot-96.md`, including failed runs. Tick the #96 acceptance items, link
   the note from #96 and #94, and give #97 the endpoint and issuer it should use.

## Owner-only actions

Access to the Mac mini, and permission from whoever owns it to hold provider credentials and run continuously. Holding
the Codex and E2B credentials, and any usage cost they incur. Approving that the pilot endpoint may be reachable by #97.

## Out of scope

Kubernetes, multi-region, high availability, measured SLO, RPO or RTO, a public multi-tenant service, an example UI
(#97), a metrics endpoint, and Kubernetes packaging, which ADR-0009 defers to its own decision.

## Limits to state in the evidence

One host, one PostgreSQL primary, logical backup only. A macOS host running Docker in a Linux VM, not a Linux server. The
rolling upgrade has no newer release to roll to; it proves the procedure, and schema compatibility across versions rests
on the CI upgrade test (#74), which uses a test-only schema until the follow-up moves it to the v0.1.0 digest. The first
policy is set by a direct update, because v0.1.0 has no provisioning command for it. The pilot overlay and runbook come
from `main`, because the v0.1.0 Compose bundle predates them. The provider limits from the #75 and #108 notes. No availability, throughput, cost or production
claim.
