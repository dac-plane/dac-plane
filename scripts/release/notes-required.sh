#!/usr/bin/env bash
# Exits 0 when a release needs hand-written notes and 1 when they are optional.
# They are required when the release adds a database migration since the previous
# final release, because that is when a deployer must act: the upgrade order and
# what runs between the steps. They are also required when there is no earlier
# release to compare with, or when it cannot be found. Any other release is
# described by its generated changelog.
set -euo pipefail
version=${1:?usage: notes-required.sh vX.Y.Z}
sorted=$({
	git tag --list 'v[0-9]*' | grep -Ev -- '-rc\.' || true
	echo "$version"
} | sort -uV)
previous=$(grep -Fx -B1 -- "$version" <<<"$sorted" | head -1)
if [[ "$previous" == "$version" ]] || ! git rev-parse -q --verify "$previous^{commit}" >/dev/null; then
	exit 0
fi
if git diff --quiet "$previous" HEAD -- internal/platform/migrations/migrations; then
	exit 1
fi
exit 0
