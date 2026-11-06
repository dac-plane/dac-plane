# Operating a dac-plane pilot

This runbook deploys a released dac-plane with real providers on one host: Codex as the runtime, its commands in
E2B sandboxes, SeaweedFS as the artifact store, and Keycloak as the OIDC issuer. It is the procedure for
[#96](https://github.com/dac-plane/dac-plane/issues/96). Every step uses published images by digest; nothing is built
on the host.

It is a pilot, not a production platform. One host, one PostgreSQL primary, logical backups, and no availability,
throughput or cost claim. [Limits](#limits) lists what the pilot does not prove.

## What runs where

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
published, on loopback by default. Adapters reach the control plane on the Compose network. Every connection to
E2B and the model provider is outbound.

## Prerequisites

- An arm64 host. The released images are live-qualified on arm64 only; on amd64, run `just release-qualify` there
  first.
- Docker with Compose v2.24.4 or later (for `!reset` and `!override`), `openssl`, `jq` and `curl`. On macOS, Docker
  runs in a Linux VM (Docker Desktop, OrbStack or Colima); give it at least 4 CPUs and 8 GB. Colima runs headless,
  which suits a host reached over SSH: `brew install colima docker docker-compose`, add
  `"cliPluginsExtraDirs": ["/opt/homebrew/lib/docker/cli-plugins"]` to `~/.docker/config.json` so `docker compose`
  finds the plugin, then `colima start --cpu 4 --memory 8 --disk 60 --vm-type vz`.
- The release's `release-manifest.json`, and the Compose files from the same commit as this runbook: `compose.yaml`,
  `compose.pilot.yaml`, `compose.keycloak.yaml`, `init-db.sh`, `artifact-store-haproxy.cfg`,
  `generate-pilot-config.sh` and `configure-pilot.sh`. The v0.1.0 Compose bundle predates the pilot files, so take
  them from `main`.
- A model-provider API key for Codex, an E2B API key, and an E2B template built from this repository with
  `just build-e2b-codex-template`. The template lives in the E2B account, so one built elsewhere can be reused.
- On a host that must stay up: sleep disabled and automatic restart after power loss.

## 1. Lay out the pilot

Keep the Compose files and the pilot's configuration apart. `PILOT_HOME` holds every secret, so it stays on the host,
owner-only.

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
`DAC_CODEX_MODEL`, `DAC_CODEX_BASE_URL` and `DAC_E2B_TEMPLATE_ID`. Compose refuses to start while any is empty.

The credential files are mode 0444 because Compose bind-mounts file secrets and the containers run as non-root
users. `$PILOT_HOME/secrets` itself is 0700.

## 4. Configure identity and provisioning

```sh
"$PILOT_COMPOSE/configure-pilot.sh" "$PILOT_HOME" "$PILOT_COMPOSE"
```

This starts Keycloak and PostgreSQL, then:

1. creates the `dac` realm, with audience scopes `dac-plane-surface` and `dac-plane-adapters`;
2. creates confidential clients with service accounts only: `pilot-owner` (Surface API) and one per adapter role;
   writes each client secret to `$PILOT_HOME/secrets`;
3. runs `migrate up`;
4. provisions, through `dac-plane provision`: the tenant, a human owner principal bound to `pilot-owner`'s service
   account, the artifact store, runtime and environment providers, each adapter binding, and the runtime's live
   harness qualification;
5. sets the first policy: `process.exec` and `fs.write` ask the owner;
6. records every identifier in `$PILOT_HOME/pilot.ids` and the provider identifiers in `pilot.env`;
7. replaces Keycloak's temporary `bootstrap-admin` with a permanent `pilot-admin` in the `master` realm, under a new
   password it writes to `secrets/keycloak-admin-password`, deletes `bootstrap-admin` once `pilot-admin` has signed in,
   and records `DAC_KEYCLOAK_ADMIN_USERNAME=pilot-admin` in `pilot.env` for later runs.

It is safe to run again: recorded identifiers are reused and every provisioning operation is idempotent.

Keycloak issues `sub` as the service account's user id, not the client id, so the bindings name those ids. The
policy step is a direct database update, because v0.1.0 has no provisioning command for policy
([#115](https://github.com/dac-plane/dac-plane/issues/115)); it only applies to the empty policy tenant creation
wrote.

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
host's Tailscale address, in `pilot.env` (`DAC_SURFACE_BIND`, `DAC_ISSUER_BIND`), and `pilot up -d`. The certificate must name the host as the
caller addresses it (step 2). The caller trusts `config/ca.crt` and keeps the names the issuer and certificate carry:

```sh
curl --cacert ca.crt --connect-to keycloak:8443:HOST:18443 https://keycloak:8443/realms/dac/...
curl --cacert ca.crt --connect-to dac-plane:8443:HOST:8443 https://dac-plane:8443/v1/...
```

Do not publish the adapter endpoint, the artifact store or PostgreSQL.

## 8. Inspect

| Question | Command |
|---|---|
| What runs, and is it healthy? | `pilot ps` |
| Which release and schema? | `pilot exec -T dac-plane /dac-plane version`, `pilot run --rm migrate migrate status` |
| What did a service log? | `pilot logs --since 30m SERVICE` |
| Which adapters are connected? | `pilot logs dac-plane \| grep 'adapter connected'` |
| What did the tenant see? | the Surface API's `/v1/tenants/{tenant}/events` with the owner's token |

Logs are JSON on standard output and stay in Docker's log driver; collect them with the host's log tooling. There is
no metrics endpoint in v0.1.0.

## 9. Back up and restore

A backup is the two databases and three volumes, taken together: the artifact store's objects and the state of the
Codex and E2B adapters, each of which records the operations it holds for the control plane. `$PILOT_HOME`, which
holds the keys, is kept encrypted elsewhere by the operator.

```sh
backup="$HOME/dac-pilot-backups/$(date -u +%Y%m%dT%H%M%SZ)"; mkdir -p "$backup"
debian=debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3
pilot stop codex-adapter e2b-adapter artifact-adapter artifact-store-seaweedfs
pilot exec -T postgres pg_dump -U dac_migrate -Fc dac >"$backup/dac.dump"
pilot exec -T keycloak-db pg_dump -U keycloak -Fc keycloak >"$backup/keycloak.dump"
for volume in artifact-data codex-data e2b-data; do
  docker run --rm -v "dac-pilot_$volume:/data:ro" -v "$backup:/backup" "$debian" \
    tar -C /data -czf "/backup/$volume.tgz" .
done
pilot start artifact-store-seaweedfs artifact-adapter e2b-adapter codex-adapter
```

The adapters and the store stop before the database dump and stay stopped until every volume is copied, so the
databases and the volumes describe the same moment. Take backups while no task is running: a stopped adapter fails a
running attempt.

Restore into an empty stack: `pilot down -v`, then `pilot create`, which recreates the volumes empty, then
`pilot up -d --wait postgres keycloak-db`, then

```sh
pilot exec -T postgres pg_restore -U dac_migrate -d dac <"$backup/dac.dump"
pilot exec -T keycloak-db pg_restore -U keycloak -d keycloak <"$backup/keycloak.dump"
for volume in artifact-data codex-data e2b-data; do
  docker run --rm -v "dac-pilot_$volume:/data" -v "$backup:/backup:ro" "$debian" \
    tar -C /data -xzf "/backup/$volume.tgz"
done
pilot up -d --wait
```

Stop conditions: `pg_restore` reports errors, `migrate status` is outside the release's range, or a task read
through the Surface API after restore differs from before.

## 10. Upgrade and roll back

Follow [ADR-0011](../../docs/adr/0011-roll-compatible-binaries-before-schema-migrations.md) and the order in
[README.md](README.md#upgrade-and-rollback): back up (step 9), set the new digests in `pilot.env`, then

```sh
pilot run --rm migrate migrate status       # the new release must serve the current schema
pilot up -d --wait --force-recreate --no-deps dac-plane
pilot up -d --wait --force-recreate --no-deps artifact-adapter e2b-adapter codex-adapter
pilot run --rm migrate                      # its default command is migrate up
```

then verify (step 6). Every roll takes `--no-deps`: `dac-plane` depends on `migrate`, which runs the same image, so
`pilot up dac-plane` without it would recreate `migrate` and apply the new schema before the binaries have rolled.

Roll back by restoring the previous digests while the schema is inside that release's range. `migrate down-to`
refuses a downgrade that would discard evidence; that refusal is a stop condition, not a prompt to delete data.

v0.1.0 is the only release, so there is no newer one to upgrade to yet. The procedure can be exercised by rolling
each service to the same digests in this order. Version-to-version safety is proven in CI by
`just test-operations-upgrade` ([#116](https://github.com/dac-plane/dac-plane/issues/116) moves it to the published
previous release).

## 11. Rotate credentials

| Credential | Procedure | Downtime |
|---|---|---|
| Adapter client secret | Regenerate it on the client's Credentials tab in Keycloak, write it to the adapter's secret file in `$PILOT_HOME/secrets`, then `pilot up -d --force-recreate --no-deps ADAPTER`. | Seconds, while the adapter reconnects |
| Owner client secret | Same, for `pilot-owner` and `owner-client-secret`. | None |
| TLS certificate | Move `config/server.crt` and `secrets/server.key` aside, run `generate-pilot-config.sh` again with the same arguments, which issues a new pair from the same CA, then `pilot up -d --force-recreate dac-plane artifact-store-seaweedfs keycloak`, and delete the old pair once the new serial is served. | About 25 seconds: the adapters reconnect after Keycloak returns |
| E2B or model key | Replace the file, then `pilot up -d --force-recreate e2b-adapter` or `codex-adapter`. | The adapter reconnects |
| S3 adapter key | Write a new value from `openssl rand -hex 24` to `s3-adapter-secret`, run `generate-pilot-config.sh` again, which rewrites `s3-identities.json` from it, then `pilot up -d --wait --force-recreate --no-deps artifact-store-seaweedfs artifact-adapter`, with no task storing an artifact. | About 15 seconds |
| Attach key | Replace the file, then recreate `e2b-adapter` and `codex-adapter` together, with no attempt running. | Running attempts lose their exec-server link |
| Database passwords | Write a new value from `openssl rand -hex 24` to the role's password file and set it with `ALTER ROLE ... PASSWORD`, run `generate-pilot-config.sh` again, which rewrites the URL files from it, then recreate `dac-plane`. | Brief |

Compose bind-mounts each secret as a single file, and the mount stays attached to the file it started with. Replacing
a file with `mv` or an editor that writes a new file leaves the container reading the deleted original: in the
rehearsal, a rotated adapter failed with `cannot open the client secret file` at its next renewal. So recreate the
container after every credential change, as the table says.

Never paste a credential into a command line, log or ticket; write it to its file.

## 12. Known limits and what to do

| Symptom | Cause | Action |
|---|---|---|
| A task stays in `provisioning` | An E2B create outlasted the send timeout, or its reply was lost, so the operation is `uncertain` ([#75 note](../../docs/architecture/provider-qualification-75.md#limits)) | Cancel the task through the Surface API; it ends once the environment stop is confirmed. Check E2B's sandbox list for a leftover sandbox. |
| A task takes about a minute to fail after a sandbox is lost | Codex reports the lost link, then the adapter waits for it to return ([#108 note](../../docs/architecture/codex-containment-108.md#limits)) | None; it ends on its own. |
| Adapters log `the token endpoint refused the client credentials` | In v0.1.0 every failed token fetch reads so, including while Keycloak restarts. Later releases say `could not be reached` or `answered HTTP 503` for those and keep this message for a real refusal. | A stop condition only if it continues once `keycloak` is healthy; then check the client id, its secret file and the client in Keycloak. |
| A turn is reported ended while its command still runs | After a runtime restart, until the environment stop lands, up to about 40 seconds | Treat the report alone as no proof the command ended; wait for the attempt to end. |

## Limits

- One host, one PostgreSQL primary, logical backups. No high availability, failover or measured RPO and RTO.
- On macOS, Docker runs in a Linux VM, which is not a Linux server.
- The CA, certificates and identity provider are the pilot's own; no public certificate.
- The Surface API caller is a service account bound to a human principal. Interactive login for people belongs to the
  reference application ([#97](https://github.com/dac-plane/dac-plane/issues/97)).
- Keycloak access tokens also carry its `account` audience; dac-plane checks only for its own.
- The first policy is set by a direct update ([#115](https://github.com/dac-plane/dac-plane/issues/115)).
- No metrics endpoint.
