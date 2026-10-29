#!/usr/bin/env bash
set -euo pipefail

embed_commit=7a69f45621f864495695e698dc5a2b93a56d2d52
runtime_commit=7278c2a380767c9989da73cf4c04b1af1b32da18
root="$(git rev-parse --show-toplevel)"
work="$root/.work/e2b-qualification"
source="$work/runtime"

if [[ "$(uname -s)" != Linux || ! -c /dev/kvm ]]; then
  echo "E2B Embed qualification requires Linux with /dev/kvm" >&2
  exit 2
fi
if [[ "${DAC_E2B_PRIVATE_HOST_ACK:-}" != "I_HAVE_FIREWALLED_THE_PRIVATE_HOST" ]]; then
  echo "Set DAC_E2B_PRIVATE_HOST_ACK only after confirming the host has no public address and blocks E2B port 5008 and all non-client host ports externally." >&2
  exit 2
fi
if ! docker compose version >/dev/null 2>&1; then
  echo "Docker Compose v2 is required" >&2
  exit 2
fi
if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  echo "Node.js and npm are required for the pinned E2B CLI isolation probe" >&2
  exit 2
fi
if ! node -e 'const [major, minor, patch] = process.versions.node.split(".").map(Number); process.exit(major > 22 || (major === 22 && (minor > 19 || (minor === 19 && patch >= 0))) ? 0 : 1)'; then
  echo "The pinned E2B CLI requires Node.js 22.19.0 or newer" >&2
  exit 2
fi

mkdir -p "$work"
if [[ ! -d "$source/.git" ]]; then
  git clone --filter=blob:none --no-checkout https://github.com/e2b-dev/runtime.git "$source"
fi
git -C "$source" fetch --depth=1 origin "$embed_commit"
git -C "$source" checkout --detach "$embed_commit"
[[ "$(git -C "$source" rev-parse HEAD)" == "$embed_commit" ]]
[[ "$(sed -n 's/^RUNTIME_COMMIT=//p' "$source/embed/compose/.env")" == "$runtime_commit" ]]
cp "$source/embed/compose/.env" "$work/compose.env"

compose=(docker compose -p dac-plane-e2b-qualification --env-file "$work/compose.env" -f "$source/embed/compose/compose.yaml")
cleanup() {
  code=$?
  "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "${key_file:-}" "$work/adapter-state.db"
  exit "$code"
}
trap cleanup EXIT

{
  printf 'embed_commit=%s\nruntime_commit=%s\n' "$embed_commit" "$runtime_commit"
  uname -a
  docker version --format 'docker_server={{.Server.Version}} arch={{.Server.Arch}}'
  docker compose version
  node --version
  npm --version
} > "$work/host.txt"

"${compose[@]}" pull
"${compose[@]}" up -d --wait
"${compose[@]}" config --images | sort -u | while IFS= read -r image; do
  docker image inspect --format '{{.Id}} {{join .RepoDigests " "}}' "$image"
done > "$work/image-digests.txt"

umask 077
key_file="$work/api-key"
"${compose[@]}" run --rm --no-deps --entrypoint cat seed /run/e2b/team-api-key > "$key_file"
[[ -s "$key_file" ]]
mkdir -p "$work/cli"
cp "$root/adapters/e2b/qualification/package.json" "$root/adapters/e2b/qualification/package-lock.json" "$work/cli/"
npm ci --prefix "$work/cli" --ignore-scripts --no-audit --no-fund
cli_home="$work/cli-home"
mkdir -p "$cli_home"
env HOME="$cli_home" XDG_CONFIG_HOME="$cli_home/.config" "$work/cli/node_modules/.bin/e2b" --version > "$work/e2b-cli-version.txt"
export DAC_E2B_BASE_URL=http://127.0.0.1:3000
export DAC_E2B_API_KEY_FILE="$key_file"
export DAC_E2B_TEMPLATE_ID=base
export DAC_E2B_STATE="$work/adapter-state.db"
export DAC_E2B_CLI="$work/cli/node_modules/.bin/e2b"
export DAC_E2B_CLI_HOME="$cli_home"
: "${DAC_PROVIDER_ID:?Set DAC_PROVIDER_ID to the registered environment provider UUID}"

cd "$root/adapters/e2b"
go test -count=1 -timeout=10m -tags=e2bqualification -run '^TestE2BEmbedLiveProvider$' ./... | tee "$work/live-test.txt"
