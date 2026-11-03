#!/usr/bin/env bash
# Prints the compatibility set of a release (ADR-0018) as JSON. DIGESTS is a
# directory with one file per target, each holding its index digest.
set -euo pipefail
version=${1:?usage: write-manifest.sh VERSION KIT NAMESPACE REVISION DIGESTS}
kit=${2:?}
namespace=${3:?}
revision=${4:?}
digests=${5:?}

images='{}'
for target in dac-plane dac-codex-adapter dac-e2b-adapter dac-s3-adapter; do
	digest=$(<"$digests/$target")
	images=$(jq --arg t "$target" --arg r "$namespace/$target@$digest" '. + {($t): $r}' <<<"$images")
done
control_plane=$(jq -r '."dac-plane"' <<<"$images")
identity=$(docker run --rm "$control_plane" version)
field() { sed -n "s/^$1=//p" <<<"$identity"; }

jq -n --arg version "$version" --arg kit "$kit" --arg revision "$revision" --argjson images "$images" \
	--argjson schema_min "$(field schema_min)" --argjson schema_current "$(field schema_current)" \
	--argjson protocol "$(field protocol)" \
	--arg surface "$(jq -r .info.version contracts/openapi/surface-v1.json)" \
	--arg codex "$(sed -n 's/^const AppServerVersion = "\(.*\)"$/\1/p' adapters/codex/version.go)" \
	'{version: $version, revision: $revision, adapterkit: $kit, images: $images,
	  schema: {min: $schema_min, current: $schema_current}, controlframe_protocol: $protocol,
	  surface_api: $surface, providers: {codex_app_server: $codex}}'
