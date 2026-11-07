# Operate a deployment

This page runs the deployment from [Deploy](../deploy/index.md), and continues its step numbers. Every command uses
the `pilot` function from [step 5](../deploy/index.md#5-deploy).

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

dac-plane upgrades binary first
([ADR-0011](https://github.com/dac-plane/dac-plane/blob/main/docs/adr/0011-roll-compatible-binaries-before-schema-migrations.md)):
first check that the new release serves the current schema, roll every binary to it against the unchanged database,
and only then move the schema forward. Migrations expand before they contract, and a contracting migration never
ships in the same rollout as binaries that still need the old shape.

Back up (step 9), set the new digests in `pilot.env`, then

```sh
pilot run --rm migrate migrate status       # the new release must serve the current schema
pilot up -d --wait --force-recreate --no-deps dac-plane
pilot up -d --wait --force-recreate --no-deps artifact-adapter e2b-adapter codex-adapter
pilot run --rm migrate                      # its default command is migrate up
```

then verify ([step 6](../deploy/index.md#6-verify)). Every roll takes `--no-deps`: `dac-plane` depends on
`migrate`, which runs the same image, so `pilot up dac-plane` without it would recreate `migrate` and apply the new
schema before the binaries have rolled.

Before the migration, roll back by restoring the previous digests against the unchanged schema. After it, a binary
rollback is safe only while the schema is inside the older release's range; check with `migrate status` first.
`migrate down-to` refuses a downgrade that would discard recorded evidence. That refusal is a stop condition, not a
prompt to delete data.

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
a file with `mv`, or with an editor that writes a new file, leaves the container reading the deleted original: in the
pilot's rehearsal, a rotated adapter failed with `cannot open the client secret file` at its next renewal. So
recreate the container after every credential change, as the table says.

The control plane reloads its TLS certificate from the mounted files for new connections. Keep the old certificate
valid until existing connections have drained.

Never paste a credential into a command line, log or ticket; write it to its file.

## 12. Known problems and what to do

| Symptom | Cause | Action |
|---|---|---|
| A task stays in `provisioning` | An E2B create outlasted the send timeout, or its reply was lost, so the operation is `uncertain` ([evidence](https://github.com/dac-plane/dac-plane/blob/main/docs/architecture/provider-qualification-75.md#limits)) | Cancel the task through the Surface API; it ends once the environment stop is confirmed. Check E2B's sandbox list for a leftover sandbox. |
| A task takes about a minute to fail after a sandbox is lost | Codex reports the lost link, then the adapter waits for it to return ([evidence](https://github.com/dac-plane/dac-plane/blob/main/docs/architecture/codex-containment-108.md#limits)) | None; it ends on its own. |
| Adapters log `the token endpoint refused the client credentials` | In v0.1.0 every failed token fetch reads so, including while Keycloak restarts. Later releases say `could not be reached` or `answered HTTP 503` for those and keep this message for a real refusal. | A stop condition only if it continues once `keycloak` is healthy; then check the client id, its secret file and the client in Keycloak. |
| A turn is reported ended while its command still runs | After a runtime restart, until the environment stop lands, up to about 40 seconds | Treat the report alone as no proof the command ended; wait for the attempt to end. |

## Limits

- One host, one PostgreSQL primary, logical backups. No high availability, failover or measured RPO and RTO.
- On macOS, Docker runs in a Linux VM, which is not a Linux server.
- The CA, certificates and identity provider are the deployment's own; no public certificate.
- The Surface API caller is a service account bound to a human principal. Interactive login for people belongs to the
  reference application ([#97](https://github.com/dac-plane/dac-plane/issues/97)).
- Keycloak access tokens also carry its `account` audience; dac-plane checks only for its own.
- The first policy is set by a direct update ([#115](https://github.com/dac-plane/dac-plane/issues/115)).
- No metrics endpoint.
- Only Codex, E2B and SeaweedFS have run live, and only together. The
  [v0.1.0 release notes](https://github.com/dac-plane/dac-plane/blob/main/docs/releases/v0.1.0.md#known-limits) list
  the release's limits.
