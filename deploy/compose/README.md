# Reference Compose deployment

This directory is the first deployment substrate defined by
[ADR-0009](../../docs/adr/0009-deployment-unit-and-first-substrate.md). The base stack runs a
PostgreSQL primary, a one-shot migration job and the `dac-plane serve` process. The development
overlay adds a TLS test issuer, trusted provisioning jobs and the separate-process simulator for
the runtime, environment and artifact-store roles. Every submission names the simulated environment
provider; its adapter accepts each provision and stop.

The base stack is production-shaped, not a production platform. It deliberately leaves database
high availability, backup and restore, ingress, certificate issuance, secret distribution and
orchestration to the operator.

## Deploying and operating

The user guide covers deploying and running this stack: the Compose settings, the database roles, upgrade and
rollback, and the procedure with real providers. See [Deploy](../../docs/guides/deploy/index.md) and
[Operate](../../docs/guides/operate/index.md). This file describes the Compose files and the test profiles built on
them.

| File | What it is |
|---|---|
| `compose.yaml` | The base stack: PostgreSQL, the `migrate` job and `dac-plane serve`. |
| `init-db.sh` | Creates the `dac_migrate` and `dac_serve` roles on first initialization. |
| `compose.pilot.yaml`, `compose.keycloak.yaml`, `artifact-store-haproxy.cfg` | Real providers by digest, Keycloak as a reference issuer, and the artifact store's TLS proxy. `just check-pilot-compose` keeps the overlay production-shaped. |
| `generate-pilot-config.sh`, `configure-pilot.sh` | Generate the pilot's configuration and provision it. |
| `compose.dev.yaml` | The development profile, below. |
| `compose.operations.yaml` | The operations test profile, below. |
| `compose.codex.yaml`, `compose.e2b.yaml`, `compose.containment.yaml`, `compose.artifacts.yaml`, `compose.artifacts.codex.yaml`, `compose.artifacts.dev.yaml` | Provider qualification overlays, below. |

## Development profile

From the repository root:

```sh
just stack-certs
just stack-up
just stack-down
```

`stack-certs` writes a throwaway CA, server certificates, database passwords, OAuth client secrets
and an env file under `.work/stack/`. The directory is ignored by Git. `just test-stack` starts a
clean stack, checks container readiness, drives submit through accepted review over HTTPS, prints
logs on failure and removes the database volume afterward.

The development profile's mock issuer, fixed tenant/provider identities, generated credentials and
simulator results are test fixtures. They are not production identity, provider qualification,
secret management or evidence that a real runtime behaves correctly.

## Operations test profile

`compose.operations.yaml` is a test overlay for issue #74. It creates two independently addressable
serving replicas against the same PostgreSQL primary and connects one simulator process to both
adapter listeners. Run it through the repository recipes, which assign a scenario-specific Compose
project and remove only that project's containers, network and volumes:

```sh
just test-operations-fencing
just test-operations-restart
just test-operations-backup
just test-operations-upgrade
just test-operations
```

The simulator settings used by this profile are test controls, not provider or production
administration interfaces. `DAC_ADAPTER_URLS` accepts a comma-separated set of adapter WebSocket
URLs and keeps one connection per simulated role per URL while sharing the runtime, environment,
artifact bytes and evidence ledger. It is mutually exclusive with the normal single `DAC_ADAPTER_URL` setting.
`DAC_SIM_CONTROL_ADDR` enables the otherwise absent HTTP pause/evidence listener. A pause may name
an exact operation or the next operation to reach a boundary, which lets a test arm the pause before
submission. The overlay binds that listener to host loopback only. Do not enable or publish it in a
production deployment.

The tagged next-release binary, schema-17 probe migration, pinned-old image and simulator evidence
listener exist only for this operations suite. They do not extend the release migration set or
qualify a real provider.

`compose.containment.yaml` layers over `compose.codex.yaml` and `compose.e2b.yaml` so Codex's commands run in the
task's sandbox. `just qualify-codex-containment` uses it. See
[ADR-0016](../../docs/adr/0016-run-agent-commands-inside-the-environment.md).

`just qualify-providers` adds the artifact store profile below, so Codex, E2B and SeaweedFS run together with no
simulator. See [the evidence note](../../docs/architecture/provider-qualification-75.md).

## Artifact store profile

`compose.artifacts.yaml` layers over `compose.codex.yaml` and `compose.e2b.yaml`. It replaces the simulator's
byte store with self-hosted SeaweedFS behind `dac-s3-adapter`
([ADR-0017](../../docs/adr/0017-qualify-an-s3-api-artifact-store.md)). As with PostgreSQL, the operator owns
the `artifact-data` volume, its backups and its capacity.

| Service | Role |
|---|---|
| `artifact-store` | HAProxy, pinned by digest. It owns the network identity and passes TLS through on `8443`. It is the only listener off loopback. |
| `artifact-store-seaweedfs` | SeaweedFS `4.51`, pinned by digest, in the proxy's network namespace. Every listener binds loopback, because its plaintext S3, filer, master, volume and IAM gRPC ports cannot be turned off. Joining the proxy's namespace lets a store restart rejoin a live namespace. |
| `artifact-store-provision` | `dac-s3-adapter provision` with the operator key. It creates or completes both buckets, verifies them, and waits for a conditional write, which SeaweedFS holds until its lock ring forms. Safe to run again. |
| `artifact-adapter` | `dac-s3-adapter` with the adapter key, which is not an admin. It checks the buckets at start and refuses to serve if they are wrong. |

Keys:
- `just stack-certs` generates the operator and adapter keys and the SeaweedFS identity file under
  `.work/stack/secrets/`.
- Never create the buckets with `weed shell`. Use `provision`, which goes through the S3 API.

Deleting a committed artifact is an operator procedure. The steps are in
[`adapters/s3/README.md`](../../adapters/s3/README.md#buckets-and-keys).

`compose.artifacts.dev.yaml` layers this profile over the development profile. The simulated runtime then
uses `scenario.artifacts.dev.json`, which uploads its fixture through the S3 adapter's presigned HTTPS
capability, exactly as the Codex adapter uploads a real artifact.

`just test-artifact-store-stack` needs no provider secret. It deploys that combination and checks:
- that the adapter is connected
- that only HTTPS S3 is exposed
- that the deployed lifecycle reaches `succeeded`, with its result committed through the store
- that the store holds exactly one frozen version with the fixture's bytes
- that the store recovers from a crash behind the same proxy
- that the profile still composes over the Codex and E2B overlays
