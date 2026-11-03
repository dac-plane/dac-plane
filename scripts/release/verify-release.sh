#!/usr/bin/env bash
# The clean-room check of a published release (ADR-0018). Everything runs from a
# temporary directory, with an empty Go module cache and no repository-local
# replacements, against what the manifest names. Run it from a checkout of the
# release's tag. With DAC_RELEASE_ANONYMOUS set it also uses no registry login.
set -euo pipefail
manifest=$(cd "$(dirname "${1:?usage: verify-release.sh MANIFEST}")" && pwd)/$(basename "$1")
repo=$(git rev-parse --show-toplevel)
repository=${GITHUB_REPOSITORY:-dac-plane/dac-plane}
kit_module=github.com/dac-plane/dac-plane/adapterkit
version=$(jq -r .version "$manifest")
kit=$(jq -r .adapterkit "$manifest")
revision=$(jq -r .revision "$manifest")

room=$(mktemp -d)
compose=()
cleanup() {
	code=$?
	if ((${#compose[@]})); then
		if ((code != 0)); then "${compose[@]}" logs --no-color --tail=100 || true; fi
		"${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
	fi
	chmod -R u+w "$room" 2>/dev/null || true
	rm -rf "$room"
	exit "$code"
}
trap cleanup EXIT
if [[ -n "${DAC_RELEASE_ANONYMOUS:-}" ]]; then
	# An empty config has no login, but must keep the buildx and compose plugins.
	mkdir "$room/docker"
	if [[ -d "${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins" ]]; then
		ln -s "${DOCKER_CONFIG:-$HOME/.docker}/cli-plugins" "$room/docker/cli-plugins"
	fi
	export DOCKER_CONFIG="$room/docker"
fi
export GOWORK=off GOFLAGS=-mod=mod GOPATH="$room/gopath" GOCACHE="$room/gocache"
unset GOPROXY GONOSUMDB GONOSUMCHECK GOPRIVATE GONOPROXY

echo "== images"
for target in $(jq -r '.images | keys[]' "$manifest"); do
	image=$(jq -r --arg t "$target" '.images[$t]' "$manifest")
	# Each platform's own labels, so a mismatched architecture cannot hide.
	platforms=$(docker buildx imagetools inspect "$image" --format '{{json .Image}}')
	if [[ $(jq 'keys | length' <<<"$platforms") != 2 ]]; then
		echo "$target does not publish two platforms" >&2
		exit 1
	fi
	jq -e --arg v "$version" --arg r "$revision" --arg k "$kit" 'to_entries | all(.value.config.Labels
		| .["org.opencontainers.image.version"] == $v and .["org.opencontainers.image.revision"] == $r
		and .["io.dac-plane.adapterkit.version"] == $k and .["org.opencontainers.image.licenses"] == "Apache-2.0")' \
		<<<"$platforms" >/dev/null
	docker pull -q "$image" >/dev/null
	gh attestation verify "oci://$image" --repo "$repository" >/dev/null
	echo "$target: labels, pull and provenance ok at ${image##*@}"
done

control_plane=$(jq -r '.images."dac-plane"' "$manifest")
identity=$(docker run --rm "$control_plane" version)
grep -qx "version=$version" <<<"$identity"
grep -qx "revision=$revision" <<<"$identity"
grep -qx "schema_current=$(jq -r .schema.current "$manifest")" <<<"$identity"
grep -qx "schema_min=$(jq -r .schema.min "$manifest")" <<<"$identity"
codex=$(docker run --rm --entrypoint /codex-app-server "$(jq -r '.images."dac-codex-adapter"' "$manifest")" --version 2>/dev/null)
[[ "$codex" == "codex-app-server $(jq -r .providers.codex_app_server "$manifest")" ]]
echo "version output ok"

echo "== control plane starts"
tar -xzf "$(dirname "$manifest")/dac-plane-compose-$version.tar.gz" -C "$room"
"$room/deploy/compose/generate-dev-config.sh" >/dev/null 2>&1
export DAC_PLANE_IMAGE="$control_plane"
compose=(docker compose -p dac-release-verify --project-directory "$room/deploy/compose" --env-file "$room/.work/stack/compose.env" -f "$room/deploy/compose/compose.yaml" -f "$room/deploy/compose/compose.dev.yaml" --profile dev)
"${compose[@]}" up -d --no-build --wait dac-plane
"${compose[@]}" exec -T dac-plane /dac-plane health ready
echo "migrated and ready"

echo "== external adapter against the published kit"
# The module proxy's first fetch of a new version can time out, and it answers
# with that failure for some minutes before trying again.
for attempt in {1..30}; do
	go list -m "$kit_module@$kit" >/dev/null 2>&1 && break
	if ((attempt == 30)); then
		echo "the module proxy never served $kit_module@$kit" >&2
		exit 1
	fi
	sleep 30
done
mkdir "$room/external"
cp "$repo/adapterkit/examples/minimal/main.go" "$room/external/"
(
	cd "$room/external"
	go mod init example.test/external-adapter
	go get "$kit_module@$kit"
	go mod tidy
	# Go adds the repository's LICENSE to a nested module; a consumer needs it.
	test -f "$(go list -m -f '{{.Dir}}' "$kit_module")/LICENSE"
	if grep -q '^replace' go.mod; then
		echo "the clean room must not replace the kit" >&2
		exit 1
	fi
	go build -o external-adapter .
	go build -o adapter-conformance "$kit_module/cmd/adapter-conformance"
	DAC_PROVIDER_ID=$(jq -r .provider_id "$repo/adapterkit/examples/minimal/fixture.json") \
		./adapter-conformance -fixture "$repo/adapterkit/examples/minimal/fixture.json" -- ./external-adapter
)
echo "built and conformant"

echo "== in-tree modules against the published kit"
for module in . adapters/codex adapters/e2b adapters/s3; do
	copy="$room/tree/$module"
	mkdir -p "$copy"
	# Only each module's own files: the kit's local copy must not be reachable.
	if [[ "$module" == . ]]; then
		git -C "$repo" archive "$revision" go.mod go.sum cmd contracts internal | tar -x -C "$copy"
	else
		git -C "$repo" archive "$revision" "$module" | tar -x -C "$room/tree"
	fi
	(
		cd "$copy"
		go mod edit -dropreplace="$kit_module" -require="$kit_module@$kit"
		go mod tidy
		go build ./...
	)
	echo "$module ok"
done
echo "clean room passed for $version"
