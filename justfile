# dac-plane developer and CI tasks. Run `just` to list them.
#
# CI calls these same recipes, so a green local `just ci` and a green CI run
# check the same things. Tool versions are pinned here and nowhere else.

set shell := ["bash", "-euo", "pipefail", "-c"]

go_dirs := "cmd internal"
tools_dir := ".work/tools"
govulncheck_version := "v1.9.0"
actionlint_version := "v1.7.12"

# Some macOS Command Line Tools SDKs break the cgo linker used by the
# integration tests; pointing SDKROOT at the active Xcode SDK avoids it.
sdkroot := if os() == "macos" { 'SDKROOT="$(xcrun --sdk macosx --show-sdk-path)" ' } else { "" }

[private]
default:
    @{{ just_executable() }} --list

# Run every check CI runs, in CI's order
[group('ci')]
ci: fmt-check mod-check vet test test-race test-integration vuln lint-workflows

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

# Run go vet on every package
[group('check')]
vet:
    go vet ./...

# Run unit tests, including the JSON Schema contract conformance suites
[group('test')]
test:
    go test ./...

# Run unit tests under the race detector
[group('test')]
test-race:
    go test -race ./...

# -count=1 bypasses the test cache, which cannot see the container and would
# otherwise report a pass without starting PostgreSQL.
[doc('Run integration tests against a real PostgreSQL container (needs Docker)')]
[group('test')]
test-integration:
    {{ sdkroot }}go test -count=1 -tags=integration ./...

# Run only the contract suites: schema conformance and both coverage guards
[group('test')]
test-contracts:
    go test ./... -run 'ContractConformance|SchemaAndGo|SchemasAccept|SchemaEnums|EveryContractSchema|EveryValidatedRecord|NoVendorIdentifiers'

# Report known vulnerabilities in code paths the module calls
[group('check')]
vuln: (_tool "govulncheck" "golang.org/x/vuln/cmd/govulncheck" govulncheck_version)
    {{ tools_dir }}/govulncheck-{{ govulncheck_version }}/govulncheck ./...

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
