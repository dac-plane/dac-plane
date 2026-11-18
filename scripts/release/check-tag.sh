#!/usr/bin/env bash
# Stops a release unless its two tags are consistent with the tree. A tag pushed
# by a maintainer (a rehearsal) must be signed and verified by GitHub (ADR-0018).
# A final release run from the release-please workflow carries no signature: its
# tags must name the merge commit of a merged release-please pull request, whose
# manifest names this version. Prints GitHub Actions outputs.
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
required=$(for module in . adapters/codex adapters/e2b adapters/s3 adapters/github; do
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

# The commit a tag names, whether the tag is lightweight or annotated.
tag_commit() {
	local ref sha
	ref=$(gh api "repos/$repository/git/ref/tags/$1")
	sha=$(jq -r .object.sha <<<"$ref")
	if [[ $(jq -r .object.type <<<"$ref") == tag ]]; then
		sha=$(gh api "repos/$repository/git/tags/$sha" --jq .object.sha)
	fi
	echo "$sha"
}

if [[ "${GITHUB_REF_TYPE:-}" == tag ]]; then
	# Pushed by a maintainer: the signature is the maintainer's decision.
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
else
	# Run from the release-please workflow: the merge of its pull request is the
	# decision to release, so a final tag must be that merge's commit.
	if [[ "$final" != true ]]; then
		echo "$tag: a release from the release-please workflow is a final release" >&2
		exit 1
	fi
	commit=$(tag_commit "$tag")
	kit_commit=$(tag_commit "$kit_tag")
	if [[ "$commit" != "$(git rev-parse HEAD)" ]]; then
		echo "$tag names $commit, not the checked out commit" >&2
		exit 1
	fi
	# The two components are versioned together, so the kit tag is the same commit.
	if [[ "$kit_commit" != "$commit" ]]; then
		echo "$kit_tag names $kit_commit, not $commit" >&2
		exit 1
	fi
	manifest=$(jq -r '."."' .release-please-manifest.json)
	kit_manifest=$(jq -r '.adapterkit' .release-please-manifest.json)
	if [[ "v$manifest" != "$tag" || "v$kit_manifest" != "$required" ]]; then
		echo "the release manifest ($manifest, kit $kit_manifest) does not match $tag and kit $required" >&2
		exit 1
	fi
	if ! "$(dirname "$0")/merged-release-pr.sh" "$commit" >/dev/null; then
		echo "$commit is not the merge of a release-please pull request opened from this repository" >&2
		exit 1
	fi
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
