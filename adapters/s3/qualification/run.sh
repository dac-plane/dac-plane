#!/usr/bin/env bash
# Qualify the S3 artifact store adapter against a pinned SeaweedFS (ADR-0017).
# Starts a throwaway store with a test CA and per-run keys, runs the live
# checks, records versions and output under .work/s3-qualification and always
# removes the store. Nothing here needs or prints a real credential.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
work="$root/.work/s3-qualification"
image="${DAC_S3_QUALIFY_IMAGE:-chrislusf/seaweedfs:4.51@sha256:f7a49e3f3972ebed5be2ed4befa4a0c68eb7bdddba6e692d42729ceb545134b8}"
container="dac-s3-qualification-$$"
port="${DAC_S3_QUALIFY_PORT:-18443}"

rm -rf "$work"
mkdir -p "$work/certs" "$work/secrets"
chmod 700 "$work/secrets"
cleanup() { docker rm -f "$container" >/dev/null 2>&1 || true; }
trap cleanup EXIT

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=dac-s3-qualification-ca \
  -keyout "$work/certs/ca.key" -out "$work/certs/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=localhost \
  -keyout "$work/certs/tls.key" -out "$work/certs/tls.csr" 2>/dev/null
openssl x509 -req -in "$work/certs/tls.csr" -CA "$work/certs/ca.crt" -CAkey "$work/certs/ca.key" -CAcreateserial -days 1 \
  -extfile <(printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n') -out "$work/certs/tls.crt" 2>/dev/null
chmod 644 "$work/certs/tls.key"

openssl rand -hex 24 > "$work/secrets/operator"
openssl rand -hex 24 > "$work/secrets/adapter"
# The adapter identity is never an admin: SeaweedFS admins bypass governance retention.
jq -n --arg op "$(cat "$work/secrets/operator")" --arg ad "$(cat "$work/secrets/adapter")" '{identities: [
  {name: "operator", credentials: [{accessKey: "dacoperator", secretKey: $op}], actions: ["Admin", "Read", "Write", "List", "Tagging"]},
  {name: "adapter", credentials: [{accessKey: "dacadapter", secretKey: $ad}],
   actions: ["Read:dac-staging", "Write:dac-staging", "List:dac-staging", "Read:dac-objects", "Write:dac-objects", "List:dac-objects"]}]}' \
  > "$work/secrets/s3.json"
chmod 644 "$work/secrets/s3.json"

{
  uname -a
  date -u '+observed_at=%Y-%m-%dT%H:%M:%SZ'
  printf 'store_image=%s\n' "$image"
  printf 'adapter_revision=%s\n' "$(git -C "$root" rev-parse HEAD)"
  printf 'adapter_tree_dirty=%s\n' "$([[ -z "$(git -C "$root" status --porcelain -- adapters/s3)" ]] && echo false || echo true)"
  go version
  docker version --format 'docker_client={{.Client.Version}} docker_server={{.Server.Version}}'
  (cd "$root/adapters/s3" && go list -m github.com/aws/aws-sdk-go-v2 github.com/aws/aws-sdk-go-v2/service/s3 github.com/aws/smithy-go)
} > "$work/host.txt"

docker run -d --name "$container" -p "127.0.0.1:$port:8443" \
  -v "$work/certs:/certs:ro" -v "$work/secrets/s3.json:/etc/dac/s3.json:ro" \
  "$image" mini -dir=/data -master.telemetry=false -s3.config=/etc/dac/s3.json \
  -s3.port.https=8443 -s3.cert.file=/certs/tls.crt -s3.key.file=/certs/tls.key >/dev/null
for _ in $(seq 1 60); do
  status="$(curl -s -o /dev/null -w '%{http_code}' --cacert "$work/certs/ca.crt" "https://localhost:$port/" || true)"
  [[ "$status" == 403 ]] && break
  sleep 1
done
if [[ "$status" != 403 ]]; then
  docker logs "$container" > "$work/store.log" 2>&1 || true
  echo "store did not start; see $work/store.log" >&2
  exit 1
fi
docker exec "$container" weed version 2>/dev/null | head -1 >> "$work/host.txt" || true

cd "$root/adapters/s3"
start="$(date +%s)"
DAC_S3_QUALIFY_ENDPOINT="https://localhost:$port" \
DAC_S3_QUALIFY_CA_FILE="$work/certs/ca.crt" \
DAC_S3_QUALIFY_STAGING_BUCKET=dac-staging \
DAC_S3_QUALIFY_OBJECTS_BUCKET=dac-objects \
DAC_S3_QUALIFY_OPERATOR_KEY_ID=dacoperator \
DAC_S3_QUALIFY_OPERATOR_SECRET_FILE="$work/secrets/operator" \
DAC_S3_QUALIFY_ADAPTER_KEY_ID=dacadapter \
DAC_S3_QUALIFY_ADAPTER_SECRET_FILE="$work/secrets/adapter" \
DAC_S3_QUALIFY_CONTAINER="$container" \
  go test -count=1 -tags=s3qualification -run '^TestQualifyArtifactStore$' -v . 2>&1 | tee "$work/live-test.txt"
printf 'duration_seconds=%s\n' "$(( $(date +%s) - start ))" >> "$work/host.txt"
docker logs "$container" > "$work/store.log" 2>&1 || true
