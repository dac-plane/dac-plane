# dac-plane developer and CI tasks. Run `just` to list them.
#
# CI calls these same recipes, so a green local `just ci` and a green CI run
# check the same things. Tool versions are pinned here and nowhere else.

set shell := ["bash", "-euo", "pipefail", "-c"]

go_dirs := "cmd internal"
adapterkit_dirs := "adapterkit"
codex_adapter_dir := "adapters/codex"
tools_dir := ".work/tools"
govulncheck_version := "v1.9.0"
actionlint_version := "v1.7.12"
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

# Run every check CI runs, in CI's order
[group('ci')]
ci: fmt-check mod-check deps-check vet vet-integration test test-race test-adapterkit test-codex-adapter test-integration-other-race test-security-race test-conformance vuln lint-workflows

# Rewrite Go files into gofmt style
[group('dev')]
fmt:
    gofmt -w {{ go_dirs }}
    cd {{ codex_adapter_dir }} && gofmt -w .

# Fail if any Go file or this justfile is not formatted, listing the files
[group('check')]
fmt-check:
    unformatted="$(gofmt -l {{ go_dirs }})"; if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
    unformatted="$(cd {{ codex_adapter_dir }} && gofmt -l .)"; if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
    {{ just_executable() }} --unstable --fmt --check

# Verify module checksums and that go.mod and go.sum are tidy
[group('check')]
mod-check:
    go mod verify
    go mod tidy -diff
    cd adapterkit && go mod verify && go mod tidy -diff
    cd {{ codex_adapter_dir }} && go mod verify && go mod tidy -diff

# Run go vet on every package
[group('check')]
vet:
    go vet ./...
    cd adapterkit && go vet ./...
    cd {{ codex_adapter_dir }} && go vet ./...

# Unit files build without tags; integration files only build behind the
# integration tag, so they need their own vet pass.
[group('check')]
vet-integration:
    go vet -tags='integration stack operationstest' ./...

# Keep simulator and conformance implementations out of the release process.
[group('check')]
deps-check:
    go list -deps ./cmd/dac-plane | while IFS= read -r dep; do case "$dep" in */internal/simulator|*/internal/testsupport|*/internal/testsupport/*|*/adapterkit/conformance|*/adapterkit/conformance/*) echo "release binary imports forbidden package: $dep"; exit 1;; esac; done
    cd {{ codex_adapter_dir }} && go list -deps ./cmd/dac-codex-adapter | while IFS= read -r dep; do case "$dep" in github.com/AssahBismarkabah/dac-plane/internal|github.com/AssahBismarkabah/dac-plane/internal/*) echo "Codex adapter imports control-plane internal package: $dep"; exit 1;; esac; done

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

# Run the secret-backed Codex qualification against the deployed HTTPS and
# ControlFrame boundaries. The disposable artifact service is a test fixture,
# not the production store qualified by #93.
[doc('Qualify pinned Codex app-server against Camer (needs Docker and DAC_CODEX_API_KEY_FILE)')]
[group('test')]
qualify-codex-runtime: stack-certs
    #!/usr/bin/env bash
    if [[ -z "${DAC_CODEX_API_KEY_FILE:-}" || ! -s "$DAC_CODEX_API_KEY_FILE" ]]; then echo "DAC_CODEX_API_KEY_FILE must name a non-empty Camer credential file" >&2; exit 2; fi
    mkdir -p .work/codex-qualification
    compose=(docker compose -p dac-codex-qualification --env-file .work/stack/compose.env -f deploy/compose/compose.yaml -f deploy/compose/compose.dev.yaml -f deploy/compose/compose.codex.yaml --profile dev --profile qualification)
    cleanup() { code=$?; "${compose[@]}" logs --no-color >.work/codex-qualification/compose.log 2>&1 || true; if credential=$(tr -d '\r\n' <"$DAC_CODEX_API_KEY_FILE") && [[ -n "$credential" ]] && grep -Fq -- "$credential" .work/codex-qualification/compose.log; then echo "provider credential appeared in qualification logs" >&2; code=1; fi; "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true; exit "$code"; }
    trap cleanup EXIT
    "${compose[@]}" down -v --remove-orphans
    "${compose[@]}" up -d --build --scale sim-adapter=0
    # A live harness needs an explicit tenant policy before approval-once can
    # issue a grant. Keep this qualification-only policy out of product setup.
    "${compose[@]}" exec -T postgres psql -v ON_ERROR_STOP=1 -U dac_migrate -d dac -c "update policy set generation = generation + 1, settings = '{\"process.exec\":\"ask\",\"fs.write\":\"ask\"}'::jsonb, ceiling = '{\"process.exec\":\"ask\",\"fs.write\":\"ask\"}'::jsonb where tenant_id = '01a167fd-51d9-76ba-b563-496e2bd89646'"
    "${compose[@]}" exec -T dac-plane /dac-plane health ready
    "${compose[@]}" exec -T codex-adapter /codex-app-server --version | tee .work/codex-qualification/version.txt
    docker image inspect dac-codex-adapter:qualification | jq -r '.[0].Id' | tee .work/codex-qualification/image-id.txt
    set -a
    source .work/stack/compose.env
    set +a
    go test -count=1 -timeout=15m -tags=codexqualification -run "${DAC_CODEX_QUALIFICATION_TEST:-TestCodexRuntimeQualification}" ./internal/codexqualification/

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

# Build both distroless image targets from their pinned bases.
[group('image')]
image-build:
    docker buildx build --load --target dac-plane -t dac-plane:dev .
    docker buildx build --load --target dac-sim-adapter -t dac-sim-adapter:dev .

# Build each target twice as OCI archives and compare the resulting manifest
# digest. Timestamps are fixed to the source commit as documented by BuildKit.
[group('image')]
image-repro:
    mkdir -p .work/image-repro
    epoch="$(git log -1 --format=%ct)"; for target in dac-plane dac-sim-adapter; do for pass in 1 2; do SOURCE_DATE_EPOCH="$epoch" docker buildx build --target "$target" --provenance=false --output "type=oci,dest=.work/image-repro/$target-$pass.tar,rewrite-timestamp=true" --metadata-file ".work/image-repro/$target-$pass.json" .; done; first="$(jq -r '."containerimage.digest"' ".work/image-repro/$target-1.json")"; second="$(jq -r '."containerimage.digest"' ".work/image-repro/$target-2.json")"; test "$first" = "$second"; echo "$target reproducible at $first"; done

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
