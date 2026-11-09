# Deploy dac-plane

This guide deploys a released dac-plane with real providers on one host, using the reference Docker Compose stack:
Codex as the runtime, its commands in E2B sandboxes, SeaweedFS as the artifact store, and Keycloak as the OpenID
Connect issuer. Every step uses published images by digest; nothing is built on the host. It is the procedure the
first pilot ran ([evidence](https://github.com/dac-plane/dac-plane/blob/main/docs/architecture/pilot-96.md)).

It is a single-host deployment, not a production platform: one PostgreSQL primary, logical backups, and no
availability, throughput or cost claim. [Operate](../operate/index.md) covers running it, and its
[limits](../operate/index.md#limits) say what it does not prove.

## What runs where

![One host runs the control plane with its database, the Codex, E2B and S3 adapters, the SeaweedFS artifact store behind HAProxy, and Keycloak with its database. Only the Surface API and Keycloak are published, on loopback. The adapters connect to the control plane over the Compose network. E2B sandboxes and the model provider are in the cloud, reached outbound.](../assets/deployment.png)

| Role | Service | Where |
|---|---|---|
| Control plane | `dac-plane`, `migrate`, `provision` | this host |
| Database | `postgres` | this host |
| Runtime | `codex-adapter` (Codex app-server inside) | this host; its commands run in the sandbox |
| Environment | `e2b-adapter` | this host; sandboxes run in E2B's cloud |
| Artifact store | `artifact-store` (HAProxy), `artifact-store-seaweedfs`, `artifact-adapter` | this host |
| Identity | `keycloak`, `keycloak-db` | this host |
| Model | the model provider Codex is configured for | the provider's cloud |

Only the Surface API (`DAC_SURFACE_BIND:DAC_SURFACE_PORT`) and Keycloak (`DAC_ISSUER_BIND:DAC_ISSUER_PORT`) are
published, on loopback by default. Adapters reach the control plane on the Compose network. Every connection to E2B
and the model provider is outbound.

## Prerequisites

- An arm64 host. The released images are live-qualified on arm64 only; on amd64, run `just release-qualify` there
  first.
- Docker with Compose v2.24.4 or later (for `!reset` and `!override`), `openssl`, `jq` and `curl`. On macOS, Docker
  runs in a Linux VM (Docker Desktop, OrbStack or Colima); give it at least 4 CPUs and 8 GB. Colima runs headless,
  which suits a host reached over SSH: `brew install colima docker docker-compose`, add
  `"cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]` to `~/.docker/config.json` so `docker compose`
  finds the plugin, then `colima start --cpu 4 --memory 8 --disk 60 --vm-type vz`.
- The release's `release-manifest.json`, and these files from
  [`deploy/compose/`](https://github.com/dac-plane/dac-plane/tree/main/deploy/compose) at the same commit as this
  guide: `compose.yaml`, `compose.pilot.yaml`, `compose.keycloak.yaml`, `init-db.sh`, `artifact-store-haproxy.cfg`,
  `generate-pilot-config.sh` and `configure-pilot.sh`. The v0.1.0 Compose bundle predates them, so take them from
  `main`.
- A model-provider API key for Codex, an E2B API key, and an E2B template built from the repository with
  `just build-e2b-codex-template`. The template lives in the E2B account, so one built elsewhere can be reused.
- On a host that must stay up: sleep disabled and automatic restart after power loss.

## 1. Lay out the deployment

Keep the Compose files and the deployment's configuration apart. `PILOT_HOME` holds every secret, so it stays on the
host, owner-only.

```sh
export PILOT_HOME="$HOME/dac-pilot"     # configuration and secrets
export PILOT_COMPOSE="$HOME/dac-pilot-compose"   # the Compose files listed above
mkdir -p "$PILOT_COMPOSE"
# copy the files listed in Prerequisites into $PILOT_COMPOSE
gh release download v0.1.0 -R dac-plane/dac-plane -p release-manifest.json -D "$PILOT_COMPOSE"
```

## 2. Generate the configuration

```sh
"$PILOT_COMPOSE/generate-pilot-config.sh" "$PILOT_HOME" "$PILOT_COMPOSE/release-manifest.json" [EXTRA_HOST_NAME...]
```

This writes a private CA, one server certificate for the control plane, artifact store and Keycloak, database, S3,
Keycloak and attach-key secrets, `config/identity.json` and `pilot.env`, with image digests from the manifest. It
never overwrites an existing key, secret or certificate. Pass the names other machines use to reach the host, for
example its Tailscale name, as extra host names; they are added to the certificate.

Stop if a manifest image is not named by digest; the script refuses it.

## 3. Add provider credentials and settings

```sh
install -m 0444 /path/to/model-api-key "$PILOT_HOME/secrets/model-api-key"
install -m 0444 /path/to/e2b-api-key   "$PILOT_HOME/secrets/e2b-api-key"
```

Then set the four provider lines at the end of `$PILOT_HOME/pilot.env`: `DAC_CODEX_MODEL_PROVIDER`,
`DAC_CODEX_MODEL`, `DAC_CODEX_BASE_URL` and `DAC_E2B_TEMPLATE_ID`. Compose refuses to start while any is empty. The
[configuration reference](../reference/configuration.md) describes each.

The credential files are mode 0444 because Compose bind-mounts file secrets and the containers run as non-root users.
`$PILOT_HOME/secrets` itself is 0700.

## 4. Configure identity and provisioning

```sh
"$PILOT_COMPOSE/configure-pilot.sh" "$PILOT_HOME" "$PILOT_COMPOSE"
```

This starts Keycloak and PostgreSQL, then:

1. creates the `dac` realm, with audience scopes `dac-plane-surface` and `dac-plane-adapters`;
2. creates confidential clients with service accounts only: `pilot-owner` (Surface API) and one per adapter role,
   and writes each client secret to `$PILOT_HOME/secrets`;
3. runs `migrate up`;
4. provisions, through [`dac-plane provision`](../reference/cli.md#provision): the tenant, a human owner principal
   bound to `pilot-owner`'s service account, the artifact store, runtime and environment providers, each adapter
   binding, and the runtime's live harness qualification;
5. sets the first policy: `process.exec` and `fs.write` ask the owner;
6. records every identifier in `$PILOT_HOME/pilot.ids` and the provider identifiers in `pilot.env`;
7. replaces Keycloak's temporary `bootstrap-admin` with a permanent `pilot-admin` in the `master` realm, under a new
   password it writes to `secrets/keycloak-admin-password`, deletes `bootstrap-admin` once `pilot-admin` has signed
   in, and records `DAC_KEYCLOAK_ADMIN_USERNAME=pilot-admin` in `pilot.env` for later runs.

It is safe to run again: recorded identifiers are reused and every provisioning operation is idempotent.

Keycloak issues `sub` as the service account's user id, not the client id, so the bindings name those ids. The
policy step is a direct database update, because v0.1.0 has no
[`provision set-policy`](../reference/cli.md#set-policy) ([#115](https://github.com/dac-plane/dac-plane/issues/115));
it only applies to the empty policy tenant creation wrote.

## 5. Deploy

Every later command uses the same Compose invocation:

```sh
pilot() {
  docker compose -p dac-pilot --env-file "$PILOT_HOME/pilot.env" --project-directory "$PILOT_COMPOSE" \
    -f "$PILOT_COMPOSE/compose.yaml" -f "$PILOT_COMPOSE/compose.pilot.yaml" -f "$PILOT_COMPOSE/compose.keycloak.yaml" "$@"
}
pilot up -d --wait
```

## 6. Verify

```sh
pilot ps
pilot exec -T dac-plane /dac-plane version          # v0.1.0, schema_min=13, schema_current=15, protocol=1
pilot exec -T dac-plane /dac-plane health ready
pilot logs dac-plane | grep '"adapter connected"'   # three lines: runtime, environment, artifact_store
```

Every service is `running` and every service with a health check is `healthy`. Stop if an adapter logs a refused
token or binding: the client id, its secret file and its binding must agree, and `configure-pilot.sh` can be run
again.

A Surface API call from the host, with the owner's token:

```sh
. "$PILOT_HOME/pilot.ids"
issuer_at="keycloak:8443:127.0.0.1:18443"
token=$(curl -fsS --cacert "$PILOT_HOME/config/ca.crt" --connect-to "$issuer_at" \
  https://keycloak:8443/realms/dac/protocol/openid-connect/token \
  -u "pilot-owner:$(cat "$PILOT_HOME/secrets/owner-client-secret")" -d grant_type=client_credentials | jq -r .access_token)
curl -fsS --cacert "$PILOT_HOME/config/ca.crt" --resolve dac-plane:8443:127.0.0.1 \
  -H "Authorization: Bearer $token" "https://dac-plane:8443/v1/tenants/$DAC_TENANT_ID/events"
```

An adapter's token, or none, gets 401 there: the two audiences never cross.

## 7. Reach it from another machine

The simplest way keeps both ports on the host's loopback and forwards them over SSH from the caller:

```sh
ssh -f -N -L 28443:127.0.0.1:8443 -L 38443:127.0.0.1:18443 PILOT_HOST
curl --cacert ca.crt --connect-to keycloak:8443:127.0.0.1:38443 https://keycloak:8443/realms/dac/...
curl --cacert ca.crt --connect-to dac-plane:8443:127.0.0.1:28443 https://dac-plane:8443/v1/...
```

`ca.crt` is a copy of `$PILOT_HOME/config/ca.crt`, which is public. Nothing is published beyond the host.

To serve callers without SSH instead, bind the published ports to an address other machines reach, for example the
host's Tailscale address, in `pilot.env` (`DAC_SURFACE_BIND`, `DAC_ISSUER_BIND`), and `pilot up -d`. The certificate
must name the host as the caller addresses it (step 2). The caller trusts `config/ca.crt` and keeps the names the
issuer and certificate carry:

```sh
curl --cacert ca.crt --connect-to keycloak:8443:HOST:18443 https://keycloak:8443/realms/dac/...
curl --cacert ca.crt --connect-to dac-plane:8443:HOST:8443 https://dac-plane:8443/v1/...
```

Do not publish the adapter endpoint, the artifact store or PostgreSQL.

## The base stack

`compose.yaml` alone is the production-shaped core the steps above build on: PostgreSQL, a one-shot `migrate` job
and `dac-plane serve`. It deliberately leaves database high availability, ingress, certificate issuance, secret
distribution and orchestration to the operator. `compose.pilot.yaml` adds the providers and `compose.keycloak.yaml`
the issuer; with your own issuer, leave the latter out.

Compose reads these from the env file; every path is a host path, mounted into the container as a secret or config:

| Variable | Required | Purpose |
|---|---|---|
| `DAC_PLANE_IMAGE` | no | The `dac-plane` image, by digest from the release manifest. Defaults to a local `dac-plane:dev` build. |
| `DAC_DEPLOYMENT` | no | `production` by default. Only `development` accepts a development issuer. |
| `DAC_SURFACE_PORT` | no | Published Surface API port; defaults to `8443`. |
| `DAC_ADAPTER_PORT` | no | Published adapter endpoint port; defaults to `9443`. |
| `DAC_IDENTITY_CONFIG_FILE` | yes | The [identity configuration file](../reference/configuration.md#identity-configuration-file). |
| `DAC_TLS_CERT_FILE` | yes | PEM server certificate. |
| `DAC_TLS_KEY_FILE` | yes | PEM server private key. |
| `DAC_POSTGRES_MIGRATE_PASSWORD_FILE` | yes | Password of the schema-owning role. |
| `DAC_POSTGRES_SERVE_PASSWORD_FILE` | yes | Password of the serving role. |
| `DAC_MIGRATE_DATABASE_URL_FILE` | yes | PostgreSQL URL for the migration job. |
| `DAC_SERVE_DATABASE_URL_FILE` | yes | PostgreSQL URL for the serving process. |

Secret values belong in files, never in the env file, logs or command arguments.

On first initialization, `init-db.sh` creates two database roles:

- `dac_migrate` owns the database and runs schema migrations, under the migration advisory lock.
- `dac_serve` can connect, use the schema, and read and write table rows. It cannot create or alter schema.

A serving process never migrates, and refuses a database whose schema is outside the range its release supports.
