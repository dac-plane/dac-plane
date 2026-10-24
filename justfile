# dac-plane developer and CI tasks. Run `just` to list them.
#
# CI calls these same recipes, so a green local `just ci` and a green CI run
# check the same things. Tool versions are pinned here and nowhere else.

set shell := ["bash", "-euo", "pipefail", "-c"]

go_dirs := "cmd internal"
adapterkit_dirs := "adapterkit"
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
ci: fmt-check mod-check vet vet-integration test test-race test-adapterkit test-integration-other-race test-security-race test-conformance vuln lint-workflows

# Rewrite Go files into gofmt style
[group('dev')]
fmt:
    gofmt -w {{ go_dirs }}

# Fail if any Go file or this justfile is not formatted, listing the files
[group('check')]
fmt-check:
    unformatted="$(gofmt -l {{ go_dirs }})"; if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
    {{ just_executable() }} --unstable --fmt --check

# Verify module checksums and that go.mod and go.sum are tidy
[group('check')]
mod-check:
    go mod verify
    go mod tidy -diff
    cd adapterkit && go mod verify && go mod tidy -diff

# Run go vet on every package
[group('check')]
vet:
    go vet ./...
    cd adapterkit && go vet ./...

# Unit files build without tags; integration files only build behind the
# integration tag, so they need their own vet pass.
[group('check')]
vet-integration:
    go vet -tags=integration ./...

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

# Report known vulnerabilities in code paths the module calls
[group('check')]
vuln: (_tool "govulncheck" "golang.org/x/vuln/cmd/govulncheck" govulncheck_version)
    {{ tools_dir }}/govulncheck-{{ govulncheck_version }}/govulncheck ./...
    cd adapterkit && ../{{ tools_dir }}/govulncheck-{{ govulncheck_version }}/govulncheck ./...

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
