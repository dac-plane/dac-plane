#!/usr/bin/env bash
# Stops a release-please pull request that needs release notes and has none. Notes
# are required when the release adds a database migration (notes-required.sh);
# otherwise the generated changelog describes it. Notes that exist must be
# linked from docs/releases/README.md. The release workflow publishes a link to
# them, so they are written on main before the pull request merges.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
version=$(jq -er '."."' .release-please-manifest.json)
notes="docs/releases/v$version.md"
if [[ ! -f "$notes" ]]; then
	if "$here/notes-required.sh" "v$version"; then
		echo "$notes is missing: this release adds a database migration, so write the release notes on main (copy docs/releases/template.md)" >&2
		exit 1
	fi
	echo "no release notes for v$version: it adds no migration, so the generated changelog describes it"
	exit 0
fi
if ! grep -Fq -- "- [v$version](v$version.md)" docs/releases/README.md; then
	echo "docs/releases/README.md does not link $notes" >&2
	exit 1
fi
