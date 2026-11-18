#!/usr/bin/env bash
# Stops a release-please pull request that has no hand-written release notes.
# release-please writes the version bump and the change list; the notes under
# docs/releases (upgrading, known limits, compatibility) are written in the
# pull request, before it merges, because the release workflow publishes a link
# to them and stops without them.
set -euo pipefail
version=$(jq -er '."."' .release-please-manifest.json)
notes="docs/releases/v$version.md"
if [[ ! -f "$notes" ]]; then
	echo "$notes is missing: write the release notes in this pull request (copy docs/releases/template.md)" >&2
	exit 1
fi
if ! grep -Fq -- "- [v$version](v$version.md)" docs/releases/README.md; then
	echo "docs/releases/README.md does not link $notes" >&2
	exit 1
fi
