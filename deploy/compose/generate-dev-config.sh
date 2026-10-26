#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
output="$repo_root/.work/stack"
secrets="$output/secrets"
mkdir -p "$secrets"
umask 077

write_secret() {
  path=$1
  if [ ! -s "$path" ]; then
    openssl rand -hex 24 >"$path"
  fi
}

write_secret "$secrets/postgres-migrate-password"
write_secret "$secrets/postgres-serve-password"
write_secret "$secrets/surface-client-secret"
write_secret "$secrets/runtime-client-secret"
write_secret "$secrets/artifact-client-secret"

if [ ! -s "$output/ca.crt" ] || [ ! -s "$output/ca.key" ]; then
  openssl req -x509 -newkey rsa:3072 -nodes -days 30 \
    -subj "/CN=dac-plane development CA" \
    -keyout "$output/ca.key" -out "$output/ca.crt"
fi

issue_certificate() {
  name=$1
  subject=$2
  sans=$3
  if [ -s "$output/$name.crt" ] && [ -s "$output/$name.key" ]; then
    return
  fi
  openssl req -new -newkey rsa:2048 -nodes -subj "/CN=$subject" \
    -addext "subjectAltName=$sans" \
    -keyout "$output/$name.key" -out "$output/$name.csr"
  openssl x509 -req -days 30 -in "$output/$name.csr" \
    -CA "$output/ca.crt" -CAkey "$output/ca.key" -CAcreateserial \
    -copy_extensions copy -out "$output/$name.crt"
}

issue_certificate server dac-plane "DNS:dac-plane,DNS:localhost,IP:127.0.0.1"
issue_certificate issuer issuer "DNS:issuer,DNS:localhost,IP:127.0.0.1"

openssl pkcs12 -export -name issuer \
  -inkey "$output/issuer.key" -in "$output/issuer.crt" -certfile "$output/ca.crt" \
  -passout pass:changeit -out "$output/issuer.p12"

migrate_password=$(cat "$secrets/postgres-migrate-password")
serve_password=$(cat "$secrets/postgres-serve-password")
printf 'postgres://dac_migrate:%s@postgres:5432/dac?sslmode=disable\n' "$migrate_password" >"$secrets/migrate-database-url"
printf 'postgres://dac_serve:%s@postgres:5432/dac?sslmode=disable\n' "$serve_password" >"$secrets/serve-database-url"
printf 'postgres://dac_migrate:%s@postgres:5432/dac_out_of_range?sslmode=disable\n' "$migrate_password" >"$secrets/out-of-range-database-url"

cat >"$output/compose.env" <<EOF
DAC_DEPLOYMENT=development
DAC_SURFACE_PORT=18443
DAC_ADAPTER_PORT=19443
DAC_ISSUER_PORT=18080
DAC_IDENTITY_CONFIG_FILE=$script_dir/identity.dev.json
DAC_TLS_CERT_FILE=$output/server.crt
DAC_TLS_KEY_FILE=$output/server.key
DAC_CA_FILE=$output/ca.crt
DAC_ISSUER_KEYSTORE_FILE=$output/issuer.p12
DAC_POSTGRES_MIGRATE_PASSWORD_FILE=$secrets/postgres-migrate-password
DAC_POSTGRES_SERVE_PASSWORD_FILE=$secrets/postgres-serve-password
DAC_MIGRATE_DATABASE_URL_FILE=$secrets/migrate-database-url
DAC_SERVE_DATABASE_URL_FILE=$secrets/serve-database-url
DAC_RUNTIME_CLIENT_SECRET_FILE=$secrets/runtime-client-secret
DAC_ARTIFACT_CLIENT_SECRET_FILE=$secrets/artifact-client-secret
DAC_STACK_SURFACE_CLIENT_SECRET_FILE=$secrets/surface-client-secret
EOF
