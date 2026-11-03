#!/usr/bin/env bash
# Stops a release unless its two tags are signed, verified by GitHub and
# consistent with the tree (ADR-0018). Prints GitHub Actions outputs.
set -euo pipefail
tag=${1:?usage: check-tag.sh TAG}
repository=${GITHUB_REPOSITORY:-dac-plane/dac-plane}
kit_module=github.com/dac-plane/dac-plane/adapterkit

if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
	echo "$tag is not a release tag" >&2
	exit 1
fi
suffix=
final=true
if [[ "$tag" == *-rc.* ]]; then
	suffix="-${tag#*-}"
	final=false
fi

# The release links these notes; a rehearsal finds out before a final tag does.
if [[ ! -f "docs/releases/${tag%%-*}.md" ]]; then
	echo "docs/releases/${tag%%-*}.md is missing" >&2
	exit 1
fi

# Every module in the tree must require the same kit version.
required=$(for module in . adapters/codex adapters/e2b adapters/s3; do
	(cd "$module" && go mod edit -json | jq -r --arg kit "$kit_module" '.Require[] | select(.Path == $kit) | .Version')
done | sort -u)
if [[ $(wc -l <<<"$required") != 1 || "$required" == *-* || -z "$required" ]]; then
	echo "modules must require one released kit version, found: $required" >&2
	exit 1
fi
kit_tag="adapterkit/$required$suffix"

# The workflow's checkout rewrites tags, so ask GitHub what it verified.
verified_commit() {
	local ref object
	ref=$(gh api "repos/$repository/git/ref/tags/$1")
	if [[ $(jq -r .object.type <<<"$ref") != tag ]]; then
		echo "$1 is not an annotated tag" >&2
		return 1
	fi
	object=$(gh api "repos/$repository/git/tags/$(jq -r .object.sha <<<"$ref")")
	if [[ $(jq -r .verification.verified <<<"$object") != true ]]; then
		echo "$1 has no verified signature: $(jq -r .verification.reason <<<"$object")" >&2
		return 1
	fi
	jq -r .object.sha <<<"$object"
}
commit=$(verified_commit "$tag")
kit_commit=$(verified_commit "$kit_tag")

if [[ "$commit" != "$(git rev-parse HEAD)" ]]; then
	echo "$tag names $commit, not the checked out commit" >&2
	exit 1
fi
# A kit tag may be older than this release, but must hold exactly this kit.
git fetch --quiet origin "$kit_commit"
if [[ "$(git rev-parse "$commit:adapterkit")" != "$(git rev-parse "$kit_commit:adapterkit")" ]]; then
	echo "adapterkit/ in this tree differs from $kit_tag" >&2
	exit 1
fi
if [[ "$final" == true ]]; then
	git fetch --quiet origin main
	if ! git merge-base --is-ancestor "$commit" FETCH_HEAD; then
		echo "a final release must be on main" >&2
		exit 1
	fi
fi

namespace=ghcr.io/dac-plane/dry-run
environment=rehearsal
if [[ "$final" == true ]]; then
	namespace=ghcr.io/dac-plane
	environment=release
fi
printf 'version=%s\nkit=%s\nnamespace=%s\nenvironment=%s\nfinal=%s\n' \
	"$tag" "$required$suffix" "$namespace" "$environment" "$final"
