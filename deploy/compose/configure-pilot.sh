#!/bin/sh
# Configures the identity and provisioning of an operated pilot (#96), after
# generate-pilot-config.sh. It starts Keycloak and PostgreSQL, creates the
# `dac` realm with one confidential client per adapter role and one for the
# pilot's owner, migrates the schema, provisions the tenant, owner, providers
# and bindings through `dac-plane provision`, seeds the harness qualification
# and the first policy, and records every identifier. It is safe to run again:
# identifiers already recorded in PILOT_HOME/pilot.ids are reused.
#
# usage: configure-pilot.sh PILOT_HOME COMPOSE_DIR
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 PILOT_HOME COMPOSE_DIR" >&2
  exit 2
fi
home=$(CDPATH= cd -- "$1" && pwd)
compose_dir=$(CDPATH= cd -- "$2" && pwd)
secrets="$home/secrets"
ids="$home/pilot.ids"
env_file="$home/pilot.env"
[ -s "$env_file" ] || { echo "$env_file is missing; run generate-pilot-config.sh first" >&2; exit 2; }
umask 077
touch "$ids"
issuer=https://keycloak:8443/realms/dac

# Compose interpolates every service on every command, so until this script
# has recorded the provider identifiers, the adapters' required settings get
# inert placeholders. Only Keycloak, PostgreSQL, migrate and provision run
# here; no adapter starts.
set -a
. "$env_file"
. "$ids"
set +a
for name in DAC_RUNTIME_PROVIDER_ID DAC_ENVIRONMENT_PROVIDER_ID DAC_ARTIFACT_PROVIDER_ID \
  DAC_CODEX_MODEL_PROVIDER DAC_CODEX_MODEL DAC_CODEX_BASE_URL DAC_E2B_TEMPLATE_ID; do
  eval "value=\${$name:-}"
  [ -n "$value" ] || export "$name=unconfigured"
done
for name in runtime-client-secret environment-client-secret artifact-client-secret model-api-key e2b-api-key; do
  [ -e "$secrets/$name" ] || : >"$secrets/$name"
done

compose() {
  docker compose -p dac-pilot --project-directory "$compose_dir" \
    -f "$compose_dir/compose.yaml" -f "$compose_dir/compose.pilot.yaml" -f "$compose_dir/compose.keycloak.yaml" "$@"
}
record() {
  grep -q "^$1=" "$ids" || printf '%s=%s\n' "$1" "$2" >>"$ids"
  export "$1=$2"
}
provision() {
  compose run --rm --no-deps -T provision "$@"
}

compose up -d --wait keycloak postgres

# Keycloak's admin API, reached through the published port under the name its
# certificate and issuer carry.
kc_connect="keycloak:8443:127.0.0.1:${DAC_ISSUER_PORT:-18443}"
kc() {
  method=$1 path=$2
  shift 2
  curl -fsS --cacert "$DAC_TRUST_BUNDLE_FILE" --connect-to "$kc_connect" -X "$method" \
    -H "Authorization: Bearer $admin_token" "https://keycloak:8443/admin/realms$path" "$@"
}
# The container health check only proves the port is open; Keycloak answers
# 503 until its first schema initialization ends.
tries=0
until curl -fsS -o /dev/null --cacert "$DAC_TRUST_BUNDLE_FILE" --connect-to "$kc_connect" \
  https://keycloak:8443/realms/master/.well-known/openid-configuration 2>/dev/null; do
  tries=$((tries + 1))
  [ "$tries" -lt 90 ] || { echo "Keycloak did not become ready" >&2; exit 1; }
  sleep 2
done
admin_token=$(curl -fsS --cacert "$DAC_TRUST_BUNDLE_FILE" --connect-to "$kc_connect" \
  https://keycloak:8443/realms/master/protocol/openid-connect/token \
  -d grant_type=password -d client_id=admin-cli -d username=bootstrap-admin \
  --data-urlencode "password=$(cat "$DAC_KEYCLOAK_ADMIN_PASSWORD_FILE")" | jq -er .access_token)

if ! kc GET /dac >/dev/null 2>&1; then
  kc POST "" -H 'Content-Type: application/json' \
    -d '{"realm":"dac","enabled":true,"accessTokenLifespan":300,"registrationAllowed":false}'
fi

# One audience scope per dac-plane listener, so a Surface token can never open
# an adapter connection or the reverse (ADR-0007, ADR-0008).
scope_id() {
  kc GET /dac/client-scopes | jq -er --arg name "$1" '.[] | select(.name == $name) | .id'
}
for audience in dac-plane-surface dac-plane-adapters; do
  if ! scope_id "$audience" >/dev/null 2>&1; then
    kc POST /dac/client-scopes -H 'Content-Type: application/json' -d "$(jq -n --arg a "$audience" '{
      name: $a, protocol: "openid-connect", attributes: {"include.in.token.scope": "true"},
      protocolMappers: [{name: "audience", protocol: "openid-connect", protocolMapper: "oidc-audience-mapper",
        config: {"included.custom.audience": $a, "access.token.claim": "true", "id.token.claim": "false"}}]}')"
  fi
done

# A confidential client with a service account and no other flow. Its secret
# goes to a file the adapter reads, and its service-account user id is the
# token subject a binding names.
client() {
  client_id=$1 audience=$2 secret_file=$3
  uuid=$(kc GET "/dac/clients?clientId=$client_id" | jq -r '.[0].id // empty')
  if [ -z "$uuid" ]; then
    kc POST /dac/clients -H 'Content-Type: application/json' -d "$(jq -n --arg c "$client_id" '{
      clientId: $c, protocol: "openid-connect", publicClient: false, clientAuthenticatorType: "client-secret",
      serviceAccountsEnabled: true, standardFlowEnabled: false, implicitFlowEnabled: false,
      directAccessGrantsEnabled: false}')"
    uuid=$(kc GET "/dac/clients?clientId=$client_id" | jq -er '.[0].id')
  fi
  kc PUT "/dac/clients/$uuid/default-client-scopes/$(scope_id "$audience")"
  chmod 0600 "$secret_file" 2>/dev/null || true
  kc GET "/dac/clients/$uuid/client-secret" | jq -er .value >"$secret_file"
  chmod 0444 "$secret_file"
  kc GET "/dac/clients/$uuid/service-account-user" | jq -er .id
}
owner_subject=$(client pilot-owner dac-plane-surface "$secrets/owner-client-secret")
runtime_subject=$(client "$DAC_RUNTIME_CLIENT_ID" dac-plane-adapters "$secrets/runtime-client-secret")
environment_subject=$(client "$DAC_ENVIRONMENT_CLIENT_ID" dac-plane-adapters "$secrets/environment-client-secret")
artifact_subject=$(client "$DAC_ARTIFACT_CLIENT_ID" dac-plane-adapters "$secrets/artifact-client-secret")

compose run --rm migrate

field() { jq -er --arg f "$1" '.[$f]'; }
with_id() { [ -n "${1:-}" ] && printf -- '-id %s' "$1"; }

# shellcheck disable=SC2046 # with_id expands to zero or two words.
record DAC_TENANT_ID "$(provision tenant $(with_id "${DAC_TENANT_ID:-}") | field tenant_id)"
record DAC_OWNER_PRINCIPAL_ID "$(provision principal -tenant "$DAC_TENANT_ID" -kind human $(with_id "${DAC_OWNER_PRINCIPAL_ID:-}") | field principal_id)"
provision bind -tenant "$DAC_TENANT_ID" -principal "$DAC_OWNER_PRINCIPAL_ID" -issuer "$issuer" -subject "$owner_subject" >/dev/null
existing() { case "$1" in unconfigured) ;; *) printf '%s' "$1" ;; esac; }
record DAC_ARTIFACT_PROVIDER_ID "$(provision register-provider -tenant "$DAC_TENANT_ID" -role artifact_store $(with_id "$(existing "$DAC_ARTIFACT_PROVIDER_ID")") | field provider_id)"
record DAC_RUNTIME_PROVIDER_ID "$(provision register-provider -tenant "$DAC_TENANT_ID" -role runtime -artifact-store "$DAC_ARTIFACT_PROVIDER_ID" $(with_id "$(existing "$DAC_RUNTIME_PROVIDER_ID")") | field provider_id)"
record DAC_ENVIRONMENT_PROVIDER_ID "$(provision register-provider -tenant "$DAC_TENANT_ID" -role environment $(with_id "$(existing "$DAC_ENVIRONMENT_PROVIDER_ID")") | field provider_id)"
provision bind-adapter -tenant "$DAC_TENANT_ID" -provider "$DAC_ARTIFACT_PROVIDER_ID" -role artifact_store -issuer "$issuer" -subject "$artifact_subject" >/dev/null
provision bind-adapter -tenant "$DAC_TENANT_ID" -provider "$DAC_RUNTIME_PROVIDER_ID" -role runtime -issuer "$issuer" -subject "$runtime_subject" >/dev/null
provision bind-adapter -tenant "$DAC_TENANT_ID" -provider "$DAC_ENVIRONMENT_PROVIDER_ID" -role environment -issuer "$issuer" -subject "$environment_subject" >/dev/null
provision qualify-harness -tenant "$DAC_TENANT_ID" -provider "$DAC_RUNTIME_PROVIDER_ID" -mode live >/dev/null

# v0.1.0 has no provisioning command for policy (#115), so the first policy is
# the same direct update the provider qualification uses: commands and file
# writes ask the owner. It runs only while the policy is still the empty
# generation-1 policy that tenant creation wrote.
compose exec -T postgres psql -v ON_ERROR_STOP=1 -U dac_migrate -d dac -v tenant="$DAC_TENANT_ID" <<'SQL'
update policy set generation = generation + 1,
  settings = '{"process.exec":"ask","fs.write":"ask"}'::jsonb,
  ceiling = '{"process.exec":"ask","fs.write":"ask"}'::jsonb,
  updated_at = now()
where tenant_id = :'tenant' and generation = 1 and settings is null and ceiling is null;
SQL

for name in DAC_RUNTIME_PROVIDER_ID DAC_ENVIRONMENT_PROVIDER_ID DAC_ARTIFACT_PROVIDER_ID; do
  grep -q "^$name=" "$env_file" || grep "^$name=" "$ids" >>"$env_file"
done
echo "pilot configured: tenant $DAC_TENANT_ID, owner $DAC_OWNER_PRINCIPAL_ID; identifiers in $ids"
