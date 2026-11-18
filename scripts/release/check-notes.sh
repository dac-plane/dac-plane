#!/usr/bin/env bash
# Warns when a release-please pull request adds a database migration and has no
# release notes (notes-required.sh), and never blocks it: like other projects, a
# release is described by its generated changelog unless someone wrote notes, and
# whether a change needs an upgrade step is the author's call when the change is
# made. Notes that exist must be linked from docs/releases/README.md. They are
# written on main before the pull request merges.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
version=$(jq -er '."."' .release-please-manifest.json)
notes="docs/releases/v$version.md"
if [[ ! -f "$notes" ]]; then
	if "$here/notes-required.sh" "v$version"; then
		echo "::warning::$notes is missing. This release adds a database migration, so a deployer needs upgrade steps: write the release notes on main (copy docs/releases/template.md), or the changelog alone describes it."
	else
		echo "no release notes for v$version: it adds no migration, so the generated changelog describes it"
	fi
	exit 0
fi
if ! grep -Fq -- "- [v$version](v$version.md)" docs/releases/README.md; then
	echo "docs/releases/README.md does not link $notes" >&2
	exit 1
fi
