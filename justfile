# dac-plane developer and CI tasks. Run `just` to list them.
# GitHub runs the full checks as parallel jobs. Use `just ci-fast` for the
# local, non-deployment gate; `just ci` runs every CI recipe serially.

set shell := ["bash", "-euo", "pipefail", "-c"]

go_dirs := "cmd internal"
adapterkit_dirs := "adapterkit"
codex_adapter_dir := "adapters/codex"
e2b_adapter_dir := "adapters/e2b"
s3_adapter_dir := "adapters/s3"
tools_dir := ".work/tools"
govulncheck_version := "v1.9.0"
actionlint_version := "v1.7.12"
trivy_version := "0.76.1"
lychee_version := "0.24.2"
redoc_version := "2.5.4"
# The documentation build tools: Zensical and its dependencies, pinned with hashes
# in docs/requirements.txt, installed into a virtualenv named by that file's hash.
zensical_dir := tools_dir / "zensical-" + sha256_file("docs/requirements.txt")
site_dir := ".work/site"
recovery_tests := "Recovery|Reconcil|Replay|Restart|Duplicate|ChangedPayload|Stale|Fence|Lost|Shadow|Redeliver|ReceiptRehydrat|RechecksQueuedAuthority"
migration_tests := "Migration"
hook_lifecycle_tests := "GovernedAction(PostHook|FailureHook|LostPostHook)|HookStageEvidence"
identity_tests := "Schema|Authenticat|Provision|Identity|TenantReader|BearerToken|Config|UnknownKeyID|KeyRotation|UntypedIssuer|NewRefuses|RunRefuses|RunReports"
security_tests := "^Test[^/]*(Security|EventForged|EventBindingOfAnother|GovernedAction|Cancel|EnvironmentStop|RuntimeStop|Artifact|Review|PersistenceTenantIsolation|PersistenceClaimFencing|Migration|Budget|Spend|Authority|Resource)[^/]*$"

# Some macOS Command Line Tools SDKs break the cgo linker used by the
# integration tests; pointing SDKROOT at the active Xcode SDK avoids it.
sdkroot := if os() == "macos" { 'SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" ' } else { "" }

[private]
default:
    @{{ just_executable() }} --list

# Fast local gate: no PostgreSQL-backed, image, stack, or operations runs.
[group('ci')]
ci-fast: fmt-check mod-check notices-check deps-check vet vet-integration test test-race test-codex-adapter test-e2b-adapter test-s3-adapter test-conformance vuln lint-workflows docs-check

# Full local equivalent of the parallel GitHub PR checks. This is intentionally
# slower and is best used for final local verification or CI diagnosis.
[group('ci')]
ci: ci-fast test-integration-other-race test-security-race image-repro test-stack test-operations

# Rewrite Go files into gofmt style
[group('dev')]
fmt:
    gofmt -w {{ go_dirs }}
    cd {{ codex_adapter_dir }} && gofmt -w .
    cd {{ e2b_adapter_dir }} && gofmt -w .
    cd {{ s3_adapter_dir }} && gofmt -w .

# Fail if any Go file or this justfile is not formatted, listing the files
[group('check')]
fmt-check:
    unformatted="$(gofmt -l {{ go_dirs }})"; if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
    unformatted="$(cd {{ codex_adapter_dir }} && gofmt -l .)"; if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
    unformatted="$(cd {{ e2b_adapter_dir }} && gofmt -l .)"; if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
    unformatted="$(cd {{ s3_adapter_dir }} && gofmt -l .)"; if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
    {{ just_executable() }} --unstable --fmt --check

# Verify module checksums and that go.mod and go.sum are tidy
[group('check')]
mod-check:
    go mod verify
    go mod tidy -diff
    cd adapterkit && go mod verify && go mod tidy -diff
    cd {{ codex_adapter_dir }} && go mod verify && go mod tidy -diff
    cd {{ e2b_adapter_dir }} && go mod verify && go mod tidy -diff
    cd {{ s3_adapter_dir }} && go mod verify && go mod tidy -diff

# Regenerate the third-party notices shipped in every published image
[group('dev')]
notices:
    scripts/release/generate-notices.sh > third_party/THIRD_PARTY_NOTICES.txt

# Fail if the notices do not match the modules linked into the released binaries
[group('check')]
notices-check:
    scripts/release/generate-notices.sh | diff -q - third_party/THIRD_PARTY_NOTICES.txt

# Run go vet on every package
[group('check')]
vet:
    go vet ./...
    cd adapterkit && go vet ./...
    cd {{ codex_adapter_dir }} && go vet ./...
    cd {{ e2b_adapter_dir }} && go vet ./...
    cd {{ s3_adapter_dir }} && go vet ./...
    cd {{ s3_adapter_dir }} && go vet -tags=s3qualification ./...

# Unit files build without tags; integration files only build behind the
# integration tag, so they need their own vet pass.
[group('check')]
vet-integration:
    go vet -tags='integration stack operationstest codexqualification' ./...

# Keep simulator and conformance implementations out of the release process.
[group('check')]
deps-check:
    go list -deps ./cmd/dac-plane | while IFS= read -r dep; do case "$dep" in */internal/simulator|*/internal/testsupport|*/internal/testsupport/*|*/adapterkit/conformance|*/adapterkit/conformance/*) echo "release binary imports forbidden package: $dep"; exit 1;; esac; done
    cd {{ codex_adapter_dir }} && go list -deps ./cmd/dac-codex-adapter | while IFS= read -r dep; do case "$dep" in github.com/dac-plane/dac-plane/internal|github.com/dac-plane/dac-plane/internal/*) echo "Codex adapter imports control-plane internal package: $dep"; exit 1;; esac; done
    cd {{ e2b_adapter_dir }} && go list -deps ./cmd/dac-e2b-adapter | while IFS= read -r dep; do case "$dep" in github.com/dac-plane/dac-plane/internal|github.com/dac-plane/dac-plane/internal/*) echo "E2B adapter imports control-plane internal package: $dep"; exit 1;; esac; done
    cd {{ s3_adapter_dir }} && go list -deps ./cmd/dac-s3-adapter | while IFS= read -r dep; do case "$dep" in github.com/dac-plane/dac-plane/internal|github.com/dac-plane/dac-plane/internal/*|*/adapterkit/conformance|*/adapterkit/conformance/*) echo "S3 adapter imports a forbidden package: $dep"; exit 1;; esac; done

# Run unit tests, including the JSON Schema contract conformance suites
[group('test')]
test:
    go test ./...
    cd adapterkit && go test ./...

# Run unit tests under the race detector
[group('test')]
test-race:
    go test -race ./...
    cd adapterkit && go test -race ./...

# -count=1 bypasses the test cache, which cannot see the container and would
# otherwise report a pass without starting PostgreSQL. Pass package paths to
# run a subset, e.g. `just test-integration ./internal/platform/postgres/`.
[doc('Run integration tests against a real PostgreSQL container (needs Docker)')]
[group('test')]
test-integration *pkgs="./...":
    {{ sdkroot }}go test -count=1 -tags=integration {{ pkgs }}

# Race plus integration for the concurrency-sensitive store paths; CI runs
# this instead of the plain variant so row-lock races fail the build.
[doc('Run integration tests under the race detector (needs Docker)')]
[group('test')]
test-integration-race *pkgs="./...":
    {{ sdkroot }}go test -race -count=1 -timeout=30m -tags=integration {{ pkgs }}

# Complement of test-security-race; together they cover the complete suite.
[group('test')]
test-integration-other-race:
    {{ sdkroot }}go test -race -count=1 -timeout=30m -tags=integration ./... -skip '{{ security_tests }}'

# The adapter kit is a separate module (ADR-0008): unit tests and the codec
# and schema agreement tests live in adapterkit/.
[group('test')]
test-adapterkit:
    cd adapterkit && go test ./...
    cd adapterkit && go test -race ./...

# The Codex adapter is an independent module. Deterministic tests use a fake
# app-server and the published ControlFrame conformance suite; no credential is required.
[group('test')]
test-codex-adapter:
    cd {{ codex_adapter_dir }} && go test ./...
    cd {{ codex_adapter_dir }} && go test -race ./...

# The E2B adapter uses a deterministic fake provider in CI; the real Embed
# qualification is separate and requires a private Linux/KVM host.
[group('test')]
test-e2b-adapter:
    cd {{ e2b_adapter_dir }} && go test ./...
    cd {{ e2b_adapter_dir }} && go test -race ./...
    cd {{ e2b_adapter_dir }} && go test -tags=e2bqualification -run '^$' ./...
    grep -q "CODEX_VERSION=$(sed -n 's/^const AppServerVersion = "\(.*\)"/\1/p' {{ codex_adapter_dir }}/version.go)$" {{ e2b_adapter_dir }}/template/e2b.Dockerfile

# The S3 artifact store adapter uses an in-memory fake store and the published
# ControlFrame conformance suite here; `qualify-artifact-store` runs it against
# a real store. Reply payloads are checked against contracts/jsonschema.
[group('test')]
test-s3-adapter:
    cd {{ s3_adapter_dir }} && go test ./...
    cd {{ s3_adapter_dir }} && go test -race ./...
    cd {{ s3_adapter_dir }} && go test -tags=s3qualification -run '^$' ./...

# Qualify the S3 adapter against a throwaway SeaweedFS pinned by digest (needs Docker).
[doc('Qualify the artifact store against pinned SeaweedFS (needs Docker)')]
[group('test')]
qualify-artifact-store:
    ./adapters/s3/qualification/run.sh

# Run the real E2B Cloud qualification with a bounded, secret-backed sandbox probe.
[doc('Qualify managed E2B Cloud (requires APIKEY_E2B and incurs sandbox usage)')]
[group('test')]
qualify-e2b-environment:
    ./adapters/e2b/qualification/run.sh

# Build the sandbox template that runs Codex commands with the pinned E2B CLI,
# and print its name for DAC_E2B_TEMPLATE_ID. Needs Node 22.19 or newer.
[doc('Build the E2B template for Codex commands (needs E2B_API_KEY)')]
[group('test')]
build-e2b-codex-template name='dac-codex-exec':
    #!/usr/bin/env bash
    set -euo pipefail
    work=.work/e2b-template
    mkdir -p "$work"
    cp adapters/e2b/qualification/package.json adapters/e2b/qualification/package-lock.json "$work/"
    npm ci --prefix "$work" --ignore-scripts --no-audit --no-fund
    env HOME="$PWD/$work/home" XDG_CONFIG_HOME="$PWD/$work/home/.config" "$work/node_modules/.bin/e2b" template create {{ name }} --path adapters/e2b/template --dockerfile e2b.Dockerfile --cpu-count 2 --memory-mb 2048

# Run the secret-backed Codex qualification against the deployed HTTPS and
# ControlFrame boundaries. The disposable artifact service is a test fixture,
# not the production store qualified by #93.
[doc('Qualify pinned Codex app-server against Camer (needs Docker and DAC_CODEX_API_KEY_FILE)')]
[group('test')]
qualify-codex-runtime: stack-certs
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -z "${DAC_CODEX_API_KEY_FILE:-}" || ! -s "$DAC_CODEX_API_KEY_FILE" ]]; then echo "DAC_CODEX_API_KEY_FILE must name a non-empty Camer credential file" >&2; exit 2; fi
    mkdir -p .work/codex-qualification
    compose=(docker compose -p dac-codex-qualification --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml --profile dev --profile qualification)
    cleanup() { code=$?; "${compose[@]}" logs --no-color >.work/codex-qualification/compose.log 2>&1 || true; if credential=$(tr -d '\r\n' <"$DAC_CODEX_API_KEY_FILE") && [[ -n "$credential" ]] && grep -Fq -- "$credential" .work/codex-qualification/compose.log; then echo "provider credential appeared in qualification logs" >&2; code=1; fi; "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; exit "$code"; }
    trap cleanup EXIT
    "${compose[@]}" down -v --remove-orphans
    "${compose[@]}" up -d --build --scale sim-adapter=0
    # A live harness needs an explicit tenant policy before approval-once can
    # issue a grant. Keep this qualification-only policy out of product setup.
    "${compose[@]}" run --rm --no-deps -T provision-tenant provision set-policy -tenant 01a167fd-51d9-76ba-b563-496e2bd89646 -settings '{"process.exec":"ask","fs.write":"ask"}' -ceiling '{"process.exec":"ask","fs.write":"ask"}'
    "${compose[@]}" exec -T dac-plane /dac-plane health ready
    "${compose[@]}" exec -T codex-adapter /codex-app-server --version | tee .work/codex-qualification/version.txt
    docker image inspect dac-codex-adapter:qualification | jq -r '.[0].Id' | tee .work/codex-qualification/image-id.txt
    set -a
    source .work/stack/compose.env
    set +a
    go test -count=1 -timeout=15m -tags=codexqualification -run "${DAC_CODEX_QUALIFICATION_TEST:-TestCodexRuntimeQualification}" ./internal/codexqualification/

# Run the same live Codex qualification with the bundled SeaweedFS as the artifact
# store: Codex's result is uploaded through the S3 adapter and read back from the store.
[doc('Qualify Codex committing through the S3 artifact store (needs Docker and DAC_CODEX_API_KEY_FILE)')]
[group('test')]
qualify-codex-artifact-store: stack-certs
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -z "${DAC_CODEX_API_KEY_FILE:-}" || ! -s "$DAC_CODEX_API_KEY_FILE" ]]; then echo "DAC_CODEX_API_KEY_FILE must name a non-empty Camer credential file" >&2; exit 2; fi
    mkdir -p .work/codex-store-qualification
    compose=(docker compose -p dac-codex-qualification --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml -f deploy/compose/compose.artifacts.yaml -f deploy/compose/compose.artifacts.codex.yaml --profile dev --profile qualification)
    cleanup() { code=$?; "${compose[@]}" logs --no-color >.work/codex-store-qualification/compose.log 2>&1 || true; if credential=$(tr -d '\r\n' <"$DAC_CODEX_API_KEY_FILE") && [[ -n "$credential" ]] && grep -Fq -- "$credential" .work/codex-store-qualification/compose.log; then echo "provider credential appeared in qualification logs" >&2; code=1; fi; "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; exit "$code"; }
    trap cleanup EXIT
    "${compose[@]}" down -v --remove-orphans
    "${compose[@]}" up -d --build --scale sim-adapter=0
    # A live harness needs an explicit tenant policy before approval-once can
    # issue a grant. Keep this qualification-only policy out of product setup.
    "${compose[@]}" run --rm --no-deps -T provision-tenant provision set-policy -tenant 01a167fd-51d9-76ba-b563-496e2bd89646 -settings '{"process.exec":"ask","fs.write":"ask"}' -ceiling '{"process.exec":"ask","fs.write":"ask"}'
    "${compose[@]}" exec -T dac-plane /dac-plane health ready
    "${compose[@]}" exec -T codex-adapter /codex-app-server --version | tee .work/codex-store-qualification/version.txt
    set -a
    source .work/stack/compose.env
    set +a
    DAC_QUALIFICATION_ARTIFACT_STORE="$("${compose[@]}" ps -q artifact-store-seaweedfs)" go test -count=1 -v -timeout=15m -tags=codexqualification -run "${DAC_CODEX_QUALIFICATION_TEST:-TestCodexRuntimeQualification}" ./internal/codexqualification/ 2>&1 | tee .work/codex-store-qualification/test.txt
    # Read the logs first: grep -q closing the pipe would fail it under pipefail.
    logs=$("${compose[@]}" logs --no-color dac-plane)
    if ! grep -q '"msg":"adapter connected".*"role":"artifact_store"' <<<"$logs"; then echo "artifact store adapter did not connect" >&2; exit 1; fi

# Run the secret-backed joint qualification: DAC cancels a live Codex turn with
# the E2B adapter bound to the attempt. E2B is the only environment connection.
[doc('Qualify task cancellation across Codex and E2B Cloud (needs Docker, DAC_CODEX_API_KEY_FILE and DAC_E2B_API_KEY_FILE; incurs usage)')]
[group('test')]
qualify-task-cancellation: stack-certs
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -z "${DAC_CODEX_API_KEY_FILE:-}" || ! -s "$DAC_CODEX_API_KEY_FILE" ]]; then echo "DAC_CODEX_API_KEY_FILE must name a non-empty Camer credential file" >&2; exit 2; fi
    if [[ -z "${DAC_E2B_API_KEY_FILE:-}" || ! -s "$DAC_E2B_API_KEY_FILE" ]]; then echo "DAC_E2B_API_KEY_FILE must name a non-empty E2B credential file" >&2; exit 2; fi
    mkdir -p .work/cancellation-qualification
    export DAC_QUALIFICATION_PROJECT=dac-cancellation-qualification
    compose=(docker compose -p "$DAC_QUALIFICATION_PROJECT" --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml -f deploy/compose/compose.e2b.yaml --profile dev --profile qualification)
    cleanup() { code=$?; "${compose[@]}" logs --no-color >.work/cancellation-qualification/compose.log 2>&1 || true; for file in "$DAC_CODEX_API_KEY_FILE" "$DAC_E2B_API_KEY_FILE"; do if credential=$(tr -d '\r\n' <"$file") && [[ -n "$credential" ]] && grep -Fq -- "$credential" .work/cancellation-qualification/compose.log; then echo "provider credential appeared in qualification logs" >&2; code=1; fi; done; "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; exit "$code"; }
    trap cleanup EXIT
    "${compose[@]}" down -v --remove-orphans
    "${compose[@]}" up -d --build --scale sim-adapter=0
    "${compose[@]}" run --rm --no-deps -T provision-tenant provision set-policy -tenant 01a167fd-51d9-76ba-b563-496e2bd89646 -settings '{"process.exec":"ask","fs.write":"ask"}' -ceiling '{"process.exec":"ask","fs.write":"ask"}'
    "${compose[@]}" exec -T dac-plane /dac-plane health ready
    "${compose[@]}" exec -T codex-adapter /codex-app-server --version | tee .work/cancellation-qualification/codex-version.txt
    for image in dac-codex-adapter:qualification dac-e2b-adapter:qualification; do docker image inspect "$image" | jq -r '.[0].Id'; done | tee .work/cancellation-qualification/image-ids.txt
    set -a
    source .work/stack/compose.env
    set +a
    go test -count=1 -v -timeout=45m -tags=codexqualification -run "${DAC_CANCELLATION_QUALIFICATION_TEST:-TestTaskCancellationQualification}" ./internal/codexqualification/ 2>&1 | tee .work/cancellation-qualification/test.txt

# The containment overlay is only deployed by a secret-backed gate, so CI checks
# that it still composes with the stack.
[group('test')]
check-containment-compose: stack-certs
    DAC_CODEX_API_KEY_FILE=/dev/null DAC_E2B_API_KEY_FILE=/dev/null DAC_ENVIRONMENT_ATTACH_KEY_FILE=/dev/null DAC_E2B_TEMPLATE_ID=template docker compose --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml -f deploy/compose/compose.e2b.yaml -f deploy/compose/compose.containment.yaml --profile dev --profile qualification config -q

# The pilot overlay is only deployed by an operator, so CI checks that it
# composes over the base stack alone and stays production-shaped: no
# development issuer, simulator or local build, every image by digest, the
# deployment mode pinned, only the Surface API published, and no default for a
# required setting.
[group('test')]
check-pilot-compose:
    #!/usr/bin/env bash
    set -euo pipefail
    work=.work/pilot-compose
    mkdir -p "$work"
    digest="sha256:$(printf '0%.0s' {1..64})"
    export DAC_DEPLOYMENT=development
    for name in DAC_IDENTITY_CONFIG_FILE DAC_TLS_CERT_FILE DAC_TLS_KEY_FILE DAC_POSTGRES_MIGRATE_PASSWORD_FILE DAC_POSTGRES_SERVE_PASSWORD_FILE DAC_MIGRATE_DATABASE_URL_FILE DAC_SERVE_DATABASE_URL_FILE DAC_TRUST_BUNDLE_FILE DAC_RUNTIME_CLIENT_SECRET_FILE DAC_ENVIRONMENT_CLIENT_SECRET_FILE DAC_ARTIFACT_CLIENT_SECRET_FILE DAC_ENVIRONMENT_ATTACH_KEY_FILE DAC_CODEX_API_KEY_FILE DAC_E2B_API_KEY_FILE DAC_S3_IDENTITIES_FILE DAC_S3_OPERATOR_SECRET_FILE DAC_S3_ADAPTER_SECRET_FILE; do export "$name=/dev/null"; done
    for name in DAC_PLANE_IMAGE DAC_CODEX_ADAPTER_IMAGE DAC_E2B_ADAPTER_IMAGE DAC_S3_ADAPTER_IMAGE; do export "$name=registry.invalid/image@$digest"; done
    for name in DAC_ADAPTER_TOKEN_URL DAC_CODEX_BASE_URL; do export "$name=https://placeholder.invalid/"; done
    for name in DAC_RUNTIME_CLIENT_ID DAC_ENVIRONMENT_CLIENT_ID DAC_ARTIFACT_CLIENT_ID DAC_RUNTIME_PROVIDER_ID DAC_ENVIRONMENT_PROVIDER_ID DAC_ARTIFACT_PROVIDER_ID DAC_CODEX_MODEL_PROVIDER DAC_CODEX_MODEL DAC_E2B_TEMPLATE_ID; do export "$name=placeholder"; done
    compose=(docker compose -p dac-pilot-check -f deploy/compose/compose.yaml -f deploy/compose/compose.pilot.yaml --profile tools)
    "${compose[@]}" config >"$work/resolved.yaml"
    fail() { echo "pilot overlay: $1" >&2; exit 1; }
    if grep -n -E 'issuer:8443|dev_ca|dac-sim|sim-adapter|^ *build:' "$work/resolved.yaml"; then fail "resolves to a development or build reference"; fi
    if grep -E '^ *image:' "$work/resolved.yaml" | grep -v '@sha256:'; then fail "an image is not named by digest"; fi
    [[ "$(grep -c 'DAC_DEPLOYMENT: production' "$work/resolved.yaml")" == 1 ]] || fail "DAC_DEPLOYMENT is not pinned to production"
    [[ "$("${compose[@]}" config --format json | jq -c '[.services[] | .ports // [] | .[] | {host_ip, target}]')" == '[{"host_ip":"127.0.0.1","target":8443}]' ]] || fail "something besides the Surface API is published, or not on loopback by default"
    for name in DAC_PLANE_IMAGE DAC_TRUST_BUNDLE_FILE DAC_ADAPTER_TOKEN_URL DAC_RUNTIME_PROVIDER_ID DAC_E2B_TEMPLATE_ID DAC_CODEX_MODEL; do
      if (unset "$name"; "${compose[@]}" config -q 2>/dev/null); then fail "$name has a default"; fi
    done
    # The reference issuer composes over the pilot and publishes only its own port beside the Surface API.
    export DAC_KEYCLOAK_DB_PASSWORD_FILE=/dev/null DAC_KEYCLOAK_ADMIN_PASSWORD_FILE=/dev/null
    [[ "$("${compose[@]}" -f deploy/compose/compose.keycloak.yaml config --format json | jq -c '[.services[] | .ports // [] | .[] | {host_ip, target}] | sort_by(.target)')" == '[{"host_ip":"127.0.0.1","target":8443},{"host_ip":"127.0.0.1","target":8443}]' ]] || fail "the Keycloak overlay publishes something unexpected"
    sh -n deploy/compose/generate-pilot-config.sh
    sh -n deploy/compose/configure-pilot.sh
    echo "pilot overlay is production-shaped"

# The artifact store profile needs no provider secret. The simulated runtime
# uploads through the S3 adapter's presigned HTTPS capability, so the deployed
# lifecycle commits real bytes to SeaweedFS. It also checks that only HTTPS is
# exposed, that the store recovers from a crash behind the same proxy, and that
# the profile still composes over the Codex and E2B overlays.
[doc('Deploy the artifact store profile and run the lifecycle through it (needs Docker)')]
[group('test')]
test-artifact-store-stack: stack-certs
    #!/usr/bin/env bash
    set -euo pipefail
    DAC_CODEX_API_KEY_FILE=/dev/null DAC_E2B_API_KEY_FILE=/dev/null docker compose --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml -f deploy/compose/compose.e2b.yaml -f deploy/compose/compose.artifacts.yaml --profile dev --profile qualification config -q
    DAC_CODEX_API_KEY_FILE=/dev/null docker compose --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml -f deploy/compose/compose.artifacts.yaml -f deploy/compose/compose.artifacts.codex.yaml --profile dev --profile qualification config -q
    DAC_CODEX_API_KEY_FILE=/dev/null DAC_E2B_API_KEY_FILE=/dev/null DAC_ENVIRONMENT_ATTACH_KEY_FILE=/dev/null DAC_E2B_TEMPLATE_ID=template docker compose --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml -f deploy/compose/compose.e2b.yaml -f deploy/compose/compose.containment.yaml -f deploy/compose/compose.artifacts.yaml --profile dev --profile qualification config -q
    compose=(docker compose -p dac-artifact-store-stack --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.artifacts.yaml -f deploy/compose/compose.artifacts.dev.yaml --profile dev --profile qualification)
    cleanup() { code=$?; if (( code != 0 )); then "${compose[@]}" logs --no-color --tail=200; fi; "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; exit "$code"; }
    trap cleanup EXIT
    "${compose[@]}" down -v --remove-orphans
    "${compose[@]}" up -d --build --wait
    if ! "${compose[@]}" logs --no-color dac-plane | grep -q '"msg":"adapter connected".*"role":"artifact_store"'; then echo "artifact store adapter did not connect" >&2; exit 1; fi
    exposed="$("${compose[@]}" exec -T artifact-store-seaweedfs netstat -tln | awk 'NR > 2 && $4 !~ /^127\./ { print $4 }')"
    if [[ "$exposed" != "0.0.0.0:8443" ]]; then echo "artifact store exposes more than HTTPS S3: $exposed" >&2; exit 1; fi
    set -a
    source .work/stack/compose.env
    set +a
    go test -count=1 -tags=stack ./internal/stacktest/
    # Read the frozen bytes back through SeaweedFS's loopback filer: exactly
    # one committed version, holding the simulator's fixture.
    stored="$("${compose[@]}" exec -T artifact-store-seaweedfs sh -c 'list() { curl -sf -H "Accept: application/json" "http://127.0.0.1:8888$1/" | grep -o "\"FullPath\":\"[^\"]*\"" | cut -d\" -f4; }; for d in $(list /buckets/dac-objects/01a167fd-51d9-76ba-b563-496e2bd89646); do for f in $(list "$d"); do curl -sf "http://127.0.0.1:8888$f"; echo "--"; done; done')"
    if [[ "$(grep -c '^--$' <<<"$stored")" != 1 ]] || ! grep -q '^+delegated result$' <<<"$stored"; then echo "store does not hold exactly the committed fixture: $stored" >&2; exit 1; fi
    # A store crash must not strand the proxy: SeaweedFS rejoins its namespace.
    docker kill --signal KILL "$("${compose[@]}" ps -q artifact-store-seaweedfs)" >/dev/null
    "${compose[@]}" up -d --wait artifact-store-seaweedfs
    "${compose[@]}" run --rm --no-deps artifact-store-provision
    echo "lifecycle committed through the S3 store; profile recovered from a store crash"

# Run the secret-backed containment qualification: Codex's commands run in the
# task's sandbox, and every proof reads that sandbox directly through E2B.
[doc('Qualify Codex running in its E2B sandbox (needs Docker, DAC_CODEX_API_KEY_FILE, DAC_E2B_API_KEY_FILE and DAC_E2B_TEMPLATE_ID; incurs usage)')]
[group('test')]
qualify-codex-containment: stack-certs
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -z "${DAC_CODEX_API_KEY_FILE:-}" || ! -s "$DAC_CODEX_API_KEY_FILE" ]]; then echo "DAC_CODEX_API_KEY_FILE must name a non-empty Camer credential file" >&2; exit 2; fi
    if [[ -z "${DAC_E2B_API_KEY_FILE:-}" || ! -s "$DAC_E2B_API_KEY_FILE" ]]; then echo "DAC_E2B_API_KEY_FILE must name a non-empty E2B credential file" >&2; exit 2; fi
    if [[ -z "${DAC_E2B_TEMPLATE_ID:-}" ]]; then echo "DAC_E2B_TEMPLATE_ID must name a template from just build-e2b-codex-template" >&2; exit 2; fi
    work=.work/containment-qualification
    mkdir -p "$work"
    keydir=$(mktemp -d "$PWD/$work/attach-key.XXXXXXXX")
    export DAC_ENVIRONMENT_ATTACH_KEY_FILE="$keydir/attach.key"
    openssl rand -hex 32 >"$DAC_ENVIRONMENT_ATTACH_KEY_FILE"
    chmod 0644 "$DAC_ENVIRONMENT_ATTACH_KEY_FILE"
    export DAC_QUALIFICATION_PROJECT=dac-cancellation-qualification
    compose=(docker compose -p "$DAC_QUALIFICATION_PROJECT" --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml -f deploy/compose/compose.e2b.yaml -f deploy/compose/compose.containment.yaml --profile dev --profile qualification)
    cleanup() { code=$?; "${compose[@]}" logs --no-color >"$work/compose.log" 2>&1 || true; for file in "$DAC_CODEX_API_KEY_FILE" "$DAC_E2B_API_KEY_FILE" "$DAC_ENVIRONMENT_ATTACH_KEY_FILE"; do if credential=$(tr -d '\r\n' <"$file") && [[ -n "$credential" ]] && grep -Fq -- "$credential" "$work/compose.log"; then echo "provider credential appeared in qualification logs" >&2; code=1; fi; done; "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; rm -f "$DAC_ENVIRONMENT_ATTACH_KEY_FILE"; rmdir "$keydir"; exit "$code"; }
    trap cleanup EXIT
    "${compose[@]}" down -v --remove-orphans
    "${compose[@]}" up -d --build --scale sim-adapter=0
    "${compose[@]}" run --rm --no-deps -T provision-tenant provision set-policy -tenant 01a167fd-51d9-76ba-b563-496e2bd89646 -settings '{"process.exec":"ask","fs.write":"ask"}' -ceiling '{"process.exec":"ask","fs.write":"ask"}'
    "${compose[@]}" exec -T dac-plane /dac-plane health ready
    "${compose[@]}" exec -T codex-adapter /codex-app-server --version | tee "$work/codex-version.txt"
    for image in dac-codex-adapter:containment dac-e2b-adapter:qualification; do docker image inspect "$image" | jq -r '.[0].Id'; done | tee "$work/image-ids.txt"
    set -a
    source .work/stack/compose.env
    set +a
    go test -count=1 -v -timeout=75m -tags=codexqualification -run "${DAC_CONTAINMENT_QUALIFICATION_TEST:-TestContainmentQualification|TestTaskCancellationQualification}" ./internal/codexqualification/ 2>&1 | tee "$work/test.txt"

# Run the first providers together through the deployed lifecycle: Codex, E2B
# Cloud and SeaweedFS, with no simulator process in the stack.
[doc('Qualify Codex, E2B Cloud and SeaweedFS together (needs Docker, DAC_CODEX_API_KEY_FILE, DAC_E2B_API_KEY_FILE and DAC_E2B_TEMPLATE_ID; incurs usage)')]
[group('test')]
qualify-providers: stack-certs
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -z "${DAC_CODEX_API_KEY_FILE:-}" || ! -s "$DAC_CODEX_API_KEY_FILE" ]]; then echo "DAC_CODEX_API_KEY_FILE must name a non-empty Camer credential file" >&2; exit 2; fi
    if [[ -z "${DAC_E2B_API_KEY_FILE:-}" || ! -s "$DAC_E2B_API_KEY_FILE" ]]; then echo "DAC_E2B_API_KEY_FILE must name a non-empty E2B credential file" >&2; exit 2; fi
    if [[ -z "${DAC_E2B_TEMPLATE_ID:-}" ]]; then echo "DAC_E2B_TEMPLATE_ID must name a template from just build-e2b-codex-template" >&2; exit 2; fi
    work=.work/provider-qualification
    mkdir -p "$work"
    keydir=$(mktemp -d "$PWD/$work/attach-key.XXXXXXXX")
    export DAC_ENVIRONMENT_ATTACH_KEY_FILE="$keydir/attach.key"
    openssl rand -hex 32 >"$DAC_ENVIRONMENT_ATTACH_KEY_FILE"
    chmod 0644 "$DAC_ENVIRONMENT_ATTACH_KEY_FILE"
    export DAC_QUALIFICATION_PROJECT=dac-provider-qualification
    compose=(docker compose -p "$DAC_QUALIFICATION_PROJECT" --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml -f deploy/compose/compose.e2b.yaml -f deploy/compose/compose.containment.yaml -f deploy/compose/compose.artifacts.yaml --profile dev --profile qualification)
    cleanup() { code=$?; "${compose[@]}" logs --no-color >"$work/compose.log" 2>&1 || true; for file in "$DAC_CODEX_API_KEY_FILE" "$DAC_E2B_API_KEY_FILE" "$DAC_ENVIRONMENT_ATTACH_KEY_FILE"; do if credential=$(tr -d '\r\n' <"$file") && [[ -n "$credential" ]] && grep -Fq -- "$credential" "$work/compose.log"; then echo "provider credential appeared in qualification logs" >&2; code=1; fi; done; "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; rm -f "$DAC_ENVIRONMENT_ATTACH_KEY_FILE"; rmdir "$keydir"; exit "$code"; }
    trap cleanup EXIT
    "${compose[@]}" down -v --remove-orphans
    # With every image named by digest this runs what was published, not a local build.
    if [[ -n "${DAC_PROVIDER_PUBLISHED_IMAGES:-}" ]]; then
      for image in "$DAC_PLANE_IMAGE" "$DAC_CODEX_ADAPTER_IMAGE" "$DAC_E2B_ADAPTER_IMAGE" "$DAC_S3_ADAPTER_IMAGE"; do [[ "$image" == *@sha256:* ]] || { echo "published image $image is not named by digest" >&2; exit 2; }; docker pull -q "$image"; done
      "${compose[@]}" up -d --no-build --scale sim-adapter=0
    else
      "${compose[@]}" up -d --build --scale sim-adapter=0
    fi
    "${compose[@]}" run --rm --no-deps -T provision-tenant provision set-policy -tenant 01a167fd-51d9-76ba-b563-496e2bd89646 -settings '{"process.exec":"ask","fs.write":"ask"}' -ceiling '{"process.exec":"ask","fs.write":"ask"}'
    "${compose[@]}" exec -T dac-plane /dac-plane health ready
    "${compose[@]}" exec -T codex-adapter /codex-app-server --version | tee "$work/codex-version.txt"
    # An image ID changes on every local build; its layers identify the content.
    for image in "${DAC_PLANE_IMAGE:-dac-plane:dev}" "${DAC_CODEX_ADAPTER_IMAGE:-dac-codex-adapter:containment}" "${DAC_E2B_ADAPTER_IMAGE:-dac-e2b-adapter:qualification}" "${DAC_S3_ADAPTER_IMAGE:-dac-s3-adapter:qualification}"; do printf '%s %s\n' "$image" "$(docker image inspect "$image" | jq -r '.[0].RootFS.Layers[-1]')"; done | tee "$work/image-layers.txt"
    if docker ps --filter "label=com.docker.compose.project=$DAC_QUALIFICATION_PROJECT" --format '{{{{.Image}}' | grep -q sim-adapter; then echo "a simulator process is running" >&2; exit 1; fi
    set -a
    source .work/stack/compose.env
    set +a
    DAC_QUALIFICATION_ARTIFACT_STORE="$("${compose[@]}" ps -q artifact-store-seaweedfs)" go test -count=1 -v -timeout=75m -tags=codexqualification -run "${DAC_PROVIDER_QUALIFICATION_TEST:-TestContainmentQualification|TestTaskCancellationQualification}" ./internal/codexqualification/ 2>&1 | tee "$work/test.txt"
    # Read the logs first: grep -q closing the pipe would fail it under pipefail.
    logs=$("${compose[@]}" logs --no-color dac-plane)
    for role in runtime environment artifact_store; do if ! grep -q "\"msg\":\"adapter connected\".*\"role\":\"$role\"" <<<"$logs"; then echo "$role adapter did not connect" >&2; exit 1; fi; done

# The conformance suite runs the simulator adapter as a separate OS process,
# the way a real provider adapter runs outside the control plane.
[group('test')]
test-conformance:
    go build -o .work/dac-sim-adapter ./cmd/dac-sim-adapter
    cd adapterkit && go build -o ../.work/adapter-conformance ./cmd/adapter-conformance
    ./.work/adapter-conformance -fixture cmd/dac-sim-adapter/testdata/conformance.json -- ./.work/dac-sim-adapter cmd/dac-sim-adapter/testdata/scenario.json

# End-to-end transport suite: the Surface API, the dispatch worker and the
# simulators talk over real TLS WebSockets (ADR-0008, ADR-0010).
[group('test')]
test-adapter-wire:
    {{ sdkroot }}go test -count=1 -tags=integration ./internal/adapterwire/

# Recovery acceptance and its existing command, cursor and fencing regressions.
[group('test')]
test-recovery:
    {{ sdkroot }}go test -count=1 -tags=integration ./internal/application/ ./internal/worker/ ./internal/platform/postgres/ ./internal/simulator/ -run '{{ recovery_tests }}'

[group('test')]
test-recovery-race:
    {{ sdkroot }}go test -race -count=1 -tags=integration ./internal/application/ ./internal/worker/ ./internal/platform/postgres/ ./internal/simulator/ -run '{{ recovery_tests }}'

# Fenced harness migration command, worker and persistence acceptance.
[group('test')]
test-migration:
    {{ sdkroot }}go test -count=1 -tags=integration ./internal/application/ ./internal/worker/ ./internal/platform/postgres/ ./internal/platform/migrations/ ./internal/simulator/ -run '{{ migration_tests }}'

[group('test')]
test-migration-race:
    {{ sdkroot }}go test -race -count=1 -tags=integration ./internal/application/ ./internal/worker/ ./internal/platform/postgres/ ./internal/platform/migrations/ ./internal/simulator/ -run '{{ migration_tests }}'

# Durable pre/post/failure enforcement hook lifecycle and replay/fencing evidence.
[group('test')]
test-hook-lifecycle:
    {{ sdkroot }}go test -count=1 -tags=integration ./internal/application/ ./internal/worker/ ./internal/platform/postgres/ ./internal/platform/migrations/ ./internal/simulator/ -run '{{ hook_lifecycle_tests }}'

[group('test')]
test-hook-lifecycle-race:
    {{ sdkroot }}go test -race -count=1 -tags=integration ./internal/application/ ./internal/worker/ ./internal/platform/postgres/ ./internal/platform/migrations/ ./internal/simulator/ -run '{{ hook_lifecycle_tests }}'

# Security/lifecycle acceptance, selected by complete top-level test names.
[group('test')]
test-security:
    {{ sdkroot }}go test -count=1 -tags=integration ./... -run '{{ security_tests }}'

[group('test')]
test-security-race:
    {{ sdkroot }}go test -race -count=1 -timeout=30m -tags=integration ./... -run '{{ security_tests }}'

# Actor authentication, identity bindings and trusted provisioning (ADR-0007).
[group('test')]
test-identity:
    {{ sdkroot }}go test -count=1 -tags=integration ./internal/authn/ ./internal/identity/ ./internal/provision/ ./internal/platform/migrations/ -run '{{ identity_tests }}'

[group('test')]
test-identity-race:
    {{ sdkroot }}go test -race -count=1 -tags=integration ./internal/authn/ ./internal/identity/ ./internal/provision/ ./internal/platform/migrations/ -run '{{ identity_tests }}'

# Surface API v1 (ADR-0007): contract conformance, handlers, end-to-end lifecycle
# over HTTP, and commit-ordered event streams.
[group('test')]
test-surface:
    {{ sdkroot }}go test -count=1 -tags=integration ./internal/httpapi/
    {{ sdkroot }}go test -count=1 -tags=integration ./internal/platform/postgres/ ./internal/platform/migrations/ -run 'StreamEvents|RolledBack|SurfaceEvents'

[group('test')]
test-surface-race:
    {{ sdkroot }}go test -race -count=1 -tags=integration ./internal/httpapi/
    {{ sdkroot }}go test -race -count=1 -tags=integration ./internal/platform/postgres/ ./internal/platform/migrations/ -run 'StreamEvents|RolledBack|SurfaceEvents'

# Run only the contract suites: schema conformance and both coverage guards
[group('test')]
test-contracts:
    go test ./... -run 'ContractConformance|SchemaAndGo|SchemasAccept|SchemaEnums|EveryContractSchema|EveryValidatedRecord|NoVendorIdentifiers'

# Build the control plane, simulator and Codex adapter images from pinned bases.
[group('image')]
image-build:
    docker buildx build --load --target dac-plane -t dac-plane:dev .
    docker buildx build --load --target dac-sim-adapter -t dac-sim-adapter:dev .
    docker buildx build --load --target dac-codex-adapter -t dac-codex-adapter:dev .
    docker buildx build --load --target dac-e2b-adapter -t dac-e2b-adapter:dev .

# Build each target twice as OCI archives and compare the resulting manifest
# digest. Timestamps are fixed to the source commit as documented by BuildKit.
[group('image')]
image-repro:
    mkdir -p .work/image-repro
    epoch="$(git log -1 --format=%ct)"; for target in dac-plane dac-sim-adapter dac-codex-adapter dac-e2b-adapter dac-s3-adapter; do for pass in 1 2; do SOURCE_DATE_EPOCH="$epoch" docker buildx build --target "$target" --provenance=false --output "type=oci,dest=.work/image-repro/$target-$pass.tar,rewrite-timestamp=true" --metadata-file ".work/image-repro/$target-$pass.json" .; done; first="$(jq -r '."containerimage.digest"' ".work/image-repro/$target-1.json")"; second="$(jq -r '."containerimage.digest"' ".work/image-repro/$target-2.json")"; test "$first" = "$second"; echo "$target reproducible at $first"; done

# Build one published target for one platform twice, the second time without
# cache, and fail unless the digests match (ADR-0018).
[group('image')]
release-image target platform version kit:
    #!/usr/bin/env bash
    set -euo pipefail
    work=".work/release/{{ target }}-$(basename {{ platform }})"
    mkdir -p "$work"
    export SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)"
    build=(docker buildx build --target {{ target }} --platform {{ platform }} --provenance=false --sbom=false --build-arg VERSION={{ version }} --build-arg REVISION="$(git rev-parse HEAD)" --build-arg KIT_VERSION={{ kit }})
    "${build[@]}" --output "type=oci,dest=$work/image.tar,rewrite-timestamp=true" --metadata-file "$work/1.json" .
    "${build[@]}" --no-cache --output "type=oci,dest=$work/again.tar,rewrite-timestamp=true" --metadata-file "$work/2.json" .
    digest="$(jq -r '."containerimage.digest"' "$work/1.json")"
    test "$digest" = "$(jq -r '."containerimage.digest"' "$work/2.json")"
    rm -f "$work/again.tar"
    echo "$digest" >"$work/digest"
    echo "{{ target }} {{ platform }} reproducible at $digest"

# Stop on any fixable HIGH or CRITICAL finding or any secret in a built image,
# and write the full scan and an SBOM as evidence.
[group('image')]
release-scan target platform: _trivy
    #!/usr/bin/env bash
    set -euo pipefail
    work=".work/release/{{ target }}-$(basename {{ platform }})"
    trivy="{{ tools_dir }}/trivy-{{ trivy_version }}/trivy"
    # Trivy reads an OCI layout as a directory, not as the archive BuildKit writes.
    mkdir -p "$work/oci"
    tar -xf "$work/image.tar" -C "$work/oci"
    "$trivy" image --quiet --input "$work/oci" --scanners vuln,secret --format json --output "$work/scan.json"
    "$trivy" image --quiet --input "$work/oci" --format cyclonedx --output "$work/sbom.cdx.json"
    "$trivy" image --quiet --input "$work/oci" --scanners vuln,secret --severity HIGH,CRITICAL --ignore-unfixed --ignorefile .trivyignore.yaml --exit-code 1

# Push the built image by digest. The workflow joins the platforms into one index.
[group('image')]
release-push target platform version kit repository:
    #!/usr/bin/env bash
    set -euo pipefail
    work=".work/release/{{ target }}-$(basename {{ platform }})"
    export SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)"
    docker buildx build --target {{ target }} --platform {{ platform }} --provenance=false --sbom=false --build-arg VERSION={{ version }} --build-arg REVISION="$(git rev-parse HEAD)" --build-arg KIT_VERSION={{ kit }} --output "type=registry,name={{ repository }},push-by-digest=true,rewrite-timestamp=true" --metadata-file "$work/push.json" .
    test "$(cat "$work/digest")" = "$(jq -r '."containerimage.digest"' "$work/push.json")"

# Sign and push a release's tags from this commit, reusing a kit tag that already exists.
[group('release')]
release-tag version:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ -n "$(git status --porcelain)" ]]; then echo "commit or stash your changes first" >&2; exit 1; fi
    base="{{ version }}"; base="${base%%-*}"
    suffix="{{ version }}"; suffix="${suffix#"$base"}"
    required="$(go mod edit -json | jq -r '.Require[] | select(.Path == "github.com/dac-plane/dac-plane/adapterkit") | .Version')"
    kit="adapterkit/$required$suffix"
    tags=("{{ version }}")
    git tag -s "{{ version }}" -m "dac-plane {{ version }}"
    if [[ -z "$(git ls-remote --tags origin "refs/tags/$kit")" ]]; then
      git tag -s "$kit" -m "adapterkit $required$suffix"
      tags+=("$kit")
    fi
    git push origin "${tags[@]}"

# Run live qualification against a release's published digests, with no local build.
[group('release')]
release-qualify tag:
    #!/usr/bin/env bash
    set -euo pipefail
    work=".work/release/{{ tag }}"
    mkdir -p "$work"
    gh release download "{{ tag }}" --clobber -p release-manifest.json -D "$work"
    image() { jq -er --arg target "$1" '.images[$target]' "$work/release-manifest.json"; }
    export DAC_PROVIDER_PUBLISHED_IMAGES=1
    export DAC_PLANE_IMAGE="$(image dac-plane)"
    export DAC_CODEX_ADAPTER_IMAGE="$(image dac-codex-adapter)"
    export DAC_E2B_ADAPTER_IMAGE="$(image dac-e2b-adapter)"
    export DAC_S3_ADAPTER_IMAGE="$(image dac-s3-adapter)"
    just qualify-providers

# Repeat the clean room against a published release, with no registry login.
[group('release')]
release-verify tag:
    #!/usr/bin/env bash
    set -euo pipefail
    work=".work/release/{{ tag }}"
    mkdir -p "$work"
    gh release download "{{ tag }}" --clobber -p release-manifest.json -D "$work"
    DAC_RELEASE_ANONYMOUS=1 scripts/release/verify-release.sh "$work/release-manifest.json"

# Install the pinned scanner from its release, checking the published checksum.
[private]
_trivy:
    #!/usr/bin/env bash
    set -euo pipefail
    dir="{{ tools_dir }}/trivy-{{ trivy_version }}"
    if [[ -x "$dir/trivy" ]]; then exit 0; fi
    case "$(uname -s)-$(uname -m)" in
      Linux-x86_64) archive=Linux-64bit; sum=c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0 ;;
      Linux-aarch64) archive=Linux-ARM64; sum=e174dff2a6dba5e3ae41f8d0537a6324405bd08132497340304c6b20e0f16a1f ;;
      *) echo "the release scan runs on Linux" >&2; exit 1 ;;
    esac
    mkdir -p "$dir"
    curl --proto '=https' --proto-redir '=https' -fsSL --retry 3 -o "$dir/trivy.tar.gz" "https://github.com/aquasecurity/trivy/releases/download/v{{ trivy_version }}/trivy_{{ trivy_version }}_$archive.tar.gz"
    echo "$sum  $dir/trivy.tar.gz" | sha256sum -c -
    tar -xzf "$dir/trivy.tar.gz" -C "$dir" trivy
    rm "$dir/trivy.tar.gz"

# Generate throwaway development certificates and credentials under .work/.
[group('stack')]
stack-certs:
    ./deploy/compose/generate-dev-config.sh

# Start the development profile: issuer, provisioners, control plane and both
# simulator roles. The base file alone remains the production-shaped stack.
[group('stack')]
stack-up: stack-certs
    docker compose --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml --profile dev up -d --build

[group('stack')]
stack-down:
    test -f .work/stack/compose.env
    docker compose --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml --profile dev down --remove-orphans

# Run the full public-HTTPS lifecycle against a clean development stack.
[doc('Run the deployed lifecycle smoke test (needs Docker and OpenSSL)')]
[group('test')]
test-stack: stack-certs
    #!/usr/bin/env bash
    # Shebang recipes do not inherit `set shell`; without strict mode a failed
    # step is masked by the exit status of the last command.
    set -euo pipefail
    compose=(docker compose --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml --profile dev)
    cleanup() { code=$?; if (( code != 0 )); then "${compose[@]}" logs --no-color --tail=200; fi; "${compose[@]}" down -v --remove-orphans; exit "$code"; }
    trap cleanup EXIT
    "${compose[@]}" down -v --remove-orphans
    "${compose[@]}" up -d --build
    "${compose[@]}" exec -T dac-plane /dac-plane health ready
    set -a
    source .work/stack/compose.env
    set +a
    go test -count=1 -tags=stack ./internal/stacktest/
    "${compose[@]}" exec -T postgres createdb -U dac_migrate dac_out_of_range
    out_dsn="$PWD/.work/stack/secrets/out-of-range-database-url"
    DAC_MIGRATE_DATABASE_URL_FILE="$out_dsn" "${compose[@]}" run --rm migrate migrate up
    DAC_MIGRATE_DATABASE_URL_FILE="$out_dsn" "${compose[@]}" run --rm migrate migrate down-to 11
    if refusal=$(DAC_SERVE_DATABASE_URL_FILE="$out_dsn" "${compose[@]}" run --rm --no-deps dac-plane serve 2>&1); then echo "serve accepted an out-of-range schema"; exit 1; fi
    case "$refusal" in *"schema version is outside the range"*) ;; *) echo "$refusal"; exit 1;; esac

# Build the current, pinned-old, next-release fixture and simulator images used
# only by the #74 operations suite. The archived checkout stays under ignored
# scratch and is removed even when its build fails.
[private]
operations-images:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p .work/operations
    old_src=$(mktemp -d "$PWD/.work/operations/old-src.XXXXXX")
    cleanup() { rm -rf -- "$old_src"; }
    trap cleanup EXIT
    git archive ca20b3e7438707f58d8735251f8ffb8abc5a7a32 | tar -x -C "$old_src"
    docker build --target dac-plane -t dac-plane:operations-old "$old_src"
    docker build --target dac-plane -t dac-plane:operations-current .
    docker build --target dac-plane-operationstest -t dac-plane:operations-next .
    docker build --target dac-sim-adapter -t dac-sim-adapter:operations .

[private]
test-operations-fixture:
    go test -count=1 -tags=operationstest ./internal/platform/migrations/

[private]
_test-operations filter: stack-certs operations-images test-operations-fixture
    #!/usr/bin/env bash
    set -euo pipefail
    set -a
    source .work/stack/compose.env
    set +a
    go test -count=1 -timeout=30m -tags=operationstest ./internal/operationstest/ -run '{{ filter }}'

[doc('Prove multi-replica stale-claim fencing (needs Docker and OpenSSL)')]
[group('test')]
test-operations-fencing: (_test-operations '^TestMultiReplicaFencing$')

[doc('Prove process kill and restart recovery (needs Docker and OpenSSL)')]
[group('test')]
test-operations-restart: (_test-operations '^TestProcessKillAndRestart$')

[doc('Prove PostgreSQL backup/restore with artifact consistency (needs Docker and OpenSSL)')]
[group('test')]
test-operations-backup: (_test-operations '^TestBackupAndRestore$')

[doc('Prove a rolling binary upgrade followed by migration (needs Docker and OpenSSL)')]
[group('test')]
test-operations-upgrade: (_test-operations '^TestRollingUpgradeAcrossMigration$')

[doc('Run every deployed operations scenario (needs Docker and OpenSSL)')]
[group('test')]
test-operations: (_test-operations '^Test(MultiReplicaFencing|ProcessKillAndRestart|BackupAndRestore|RollingUpgradeAcrossMigration)$')

# Report known vulnerabilities in code paths the module calls
[group('check')]
vuln: (_tool "govulncheck" "golang.org/x/vuln/cmd/govulncheck" govulncheck_version)
    {{ tools_dir }}/govulncheck-{{ govulncheck_version }}/govulncheck ./...
    cd adapterkit && ../{{ tools_dir }}/govulncheck-{{ govulncheck_version }}/govulncheck ./...
    cd {{ codex_adapter_dir }} && ../../{{ tools_dir }}/govulncheck-{{ govulncheck_version }}/govulncheck ./...
    cd {{ e2b_adapter_dir }} && ../../{{ tools_dir }}/govulncheck-{{ govulncheck_version }}/govulncheck ./...
    cd {{ s3_adapter_dir }} && ../../{{ tools_dir }}/govulncheck-{{ govulncheck_version }}/govulncheck ./...

# Zensical renders the pages and warnings fail the build.
# Build the documentation site from docs/guides/ into .work/site
[group('docs')]
docs-build: _zensical _docs-generated
    rm -rf {{ site_dir }}
    {{ zensical_dir }}/bin/zensical build --strict

# External links are checked by a scheduled workflow, so an outside site never
# fails a PR. The 404 page links absolutely under the published path /dac-plane/,
# which the remap points back at the build directory.
# Build the documentation site and check every internal link, anchor and image
[group('docs')]
docs-check: docs-build _lychee
    go test -count=1 ./internal/testsupport/docstest/
    {{ tools_dir }}/lychee-{{ lychee_version }}/lychee --offline --no-progress --include-fragments --index-files index.html \
      --root-dir "$PWD/{{ site_dir }}" --remap "file://$PWD/{{ site_dir }}/dac-plane(/|\$) file://$PWD/{{ site_dir }}/" \
      '{{ site_dir }}/**/*.html'

# Run by the weekly docs workflow, not on pull requests. Hosts that only exist
# inside a deployment, documentation placeholders and the site's own pages,
# which docs-check covers offline, are excluded.
# Build the documentation site and check its external links online
[group('docs')]
docs-links: docs-build _lychee
    {{ tools_dir }}/lychee-{{ lychee_version }}/lychee --no-progress --max-retries 3 --index-files index.html \
      --root-dir "$PWD/{{ site_dir }}" --remap "file://$PWD/{{ site_dir }}/dac-plane(/|\$) file://$PWD/{{ site_dir }}/" \
      --exclude '^https?://(127\.0\.0\.1|localhost|dac-plane|issuer|keycloak)[:/]' --exclude 'example\.com' --exclude '^https://dac-plane\.github\.io/dac-plane/' \
      '{{ site_dir }}/**/*.html'

# Serve the documentation with live reload at http://localhost:8000/dac-plane/
[group('docs')]
docs-serve: _zensical _docs-generated
    {{ zensical_dir }}/bin/zensical serve

# The Surface API reference page renders contracts/openapi/surface-v1.json and
# the JSON Schemas it references with the pinned Redoc bundle. They are copied
# into docs/guides/, where .gitignore keeps them out of the tree, so a build and
# a live-reloading serve both publish them.
[private]
_docs-generated: _redoc
    rm -rf docs/guides/assets/redoc docs/guides/jsonschema docs/guides/reference/surface-v1.json
    mkdir -p docs/guides/assets/redoc
    cp {{ tools_dir }}/redoc-{{ redoc_version }}/* docs/guides/assets/redoc/
    cp contracts/openapi/surface-v1.json docs/guides/reference/
    cp -R contracts/jsonschema docs/guides/jsonschema

# Re-pin the documentation build tools after editing docs/requirements.in (needs uv)
[group('docs')]
docs-lock:
    uv pip compile docs/requirements.in --generate-hashes --universal --python-version 3.11 --no-header -o docs/requirements.txt

[private]
_zensical:
    #!/usr/bin/env bash
    set -euo pipefail
    dir="{{ zensical_dir }}"
    if [[ -x "$dir/bin/zensical" ]]; then exit 0; fi
    python3 -m venv "$dir"
    "$dir/bin/pip" install --quiet --disable-pip-version-check --require-hashes --only-binary :all: -r docs/requirements.txt

[private]
_lychee:
    #!/usr/bin/env bash
    set -euo pipefail
    dir="{{ tools_dir }}/lychee-{{ lychee_version }}"
    if [[ -x "$dir/lychee" ]]; then exit 0; fi
    case "$(uname -s)-$(uname -m)" in
      Linux-x86_64) target=x86_64-unknown-linux-gnu; sum=1f4e0ef7f6554a6ed33dd7ac144fb2e1bbed98598e7af973042fc5cd43951c9a ;;
      Linux-aarch64) target=aarch64-unknown-linux-gnu; sum=91a7bd65685da41b90ccb9bc867a3d649a7818042dae04ff405e55a25bddee4c ;;
      Darwin-arm64) target=aarch64-apple-darwin; sum=c9d3740ea2d891854d37116c9fba840f37b6e7c89d330e7db84ac333631c4977 ;;
      Darwin-x86_64) target=x86_64-apple-darwin; sum=887503a9cff667d322b8d0892b40bf49976eb9507af8483220a3706cdad55978 ;;
      *) echo "no pinned lychee build for $(uname -s)-$(uname -m)" >&2; exit 1 ;;
    esac
    mkdir -p "$dir"
    curl --proto '=https' --proto-redir '=https' -fsSL --retry 3 -o "$dir/lychee.tar.gz" "https://github.com/lycheeverse/lychee/releases/download/lychee-v{{ lychee_version }}/lychee-$target.tar.gz"
    echo "$sum  $dir/lychee.tar.gz" | shasum -a 256 -c -
    tar -xzf "$dir/lychee.tar.gz" -C "$dir" --strip-components=1 "lychee-$target/lychee"
    rm "$dir/lychee.tar.gz"

# Redoc's standalone bundle, from the npm registry tarball, checked against the
# registry's published sha512 integrity (shown here as its sha256).
[private]
_redoc:
    #!/usr/bin/env bash
    set -euo pipefail
    dir="{{ tools_dir }}/redoc-{{ redoc_version }}"
    if [[ -f "$dir/redoc.standalone.js" && -f "$dir/LICENSE" ]]; then exit 0; fi
    mkdir -p "$dir"
    curl --proto '=https' --proto-redir '=https' -fsSL --retry 3 -o "$dir/redoc.tgz" "https://registry.npmjs.org/redoc/-/redoc-{{ redoc_version }}.tgz"
    echo "44922df4d0eb45c1b066c09ec251e6bd9bd7ded16bc69e87613337b9d911a56e  $dir/redoc.tgz" | shasum -a 256 -c -
    tar -xzf "$dir/redoc.tgz" -C "$dir" --strip-components=2 package/bundles/redoc.standalone.js package/bundles/redoc.standalone.js.LICENSE.txt
    tar -xzf "$dir/redoc.tgz" -C "$dir" --strip-components=1 package/LICENSE
    rm "$dir/redoc.tgz"

# Lint GitHub Actions workflows
[group('check')]
lint-workflows: (_tool "actionlint" "github.com/rhysd/actionlint/cmd/actionlint" actionlint_version)
    {{ tools_dir }}/actionlint-{{ actionlint_version }}/actionlint

# Install a pinned tool into .work/tools/<name>-<version> unless present. The
# checksum database verifies it, it stays out of go.mod, and CI caches the
# directory keyed on this file.
[private]
_tool name package version:
    dir="{{ tools_dir }}/{{ name }}-{{ version }}"; if [ ! -x "$dir/{{ name }}" ]; then GOBIN="$PWD/$dir" go install "{{ package }}@{{ version }}"; fi
