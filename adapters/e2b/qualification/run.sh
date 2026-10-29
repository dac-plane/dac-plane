#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
work="$root/.work/e2b-qualification"
if [[ -z "${APIKEY_E2B:-}" ]]; then
  echo "APIKEY_E2B must contain the E2B Cloud API key" >&2
  exit 2
fi
if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  echo "Node.js and npm are required for the pinned E2B CLI probes" >&2
  exit 2
fi
if ! node -e 'const [major, minor, patch] = process.versions.node.split(".").map(Number); process.exit(major > 22 || (major === 22 && (minor > 19 || (minor === 19 && patch >= 0))) ? 0 : 1)'; then
  echo "The pinned E2B CLI requires Node.js 22.19.0 or newer" >&2
  exit 2
fi

mkdir -p "$work"
{
  uname -a
  date -u '+observed_at=%Y-%m-%dT%H:%M:%SZ'
  printf 'e2b_api_surface=v2\ne2b_base_url=%s\n' "${DAC_E2B_BASE_URL:-https://api.e2b.app}"
  printf 'adapter_revision=%s\n' "$(git -C "$root" rev-parse HEAD)"
  go version
  node --version
  npm --version
} > "$work/host.txt"

mkdir -p "$work/cli"
cp "$root/adapters/e2b/qualification/package.json" "$root/adapters/e2b/qualification/package-lock.json" "$work/cli/"
npm ci --prefix "$work/cli" --ignore-scripts --no-audit --no-fund
curl -kfsS --connect-timeout 3 --max-time 8 -o /dev/null https://1.1.1.1
cli_home="$work/cli-home"
mkdir -p "$cli_home"
env HOME="$cli_home" XDG_CONFIG_HOME="$cli_home/.config" E2B_API_KEY="$APIKEY_E2B" "$work/cli/node_modules/.bin/e2b" --version > "$work/e2b-cli-version.txt"
export DAC_E2B_BASE_URL="${DAC_E2B_BASE_URL:-https://api.e2b.app}"
export DAC_E2B_TEMPLATE_ID="${DAC_E2B_TEMPLATE_ID:-base}"
export DAC_E2B_STATE="$work/adapter-state.db"
export DAC_E2B_CLI="$work/cli/node_modules/.bin/e2b"
export DAC_E2B_CLI_HOME="$cli_home"
export DAC_E2B_API_KEY="$APIKEY_E2B"
: "${DAC_PROVIDER_ID:=019a0d5b-f74c-7000-8000-000000000125}"
export DAC_PROVIDER_ID

cd "$root/adapters/e2b"
go test -v -count=1 -timeout=10m -tags=e2bqualification -run '^TestE2BCloudLiveProvider$' ./... | tee "$work/live-test.txt"
