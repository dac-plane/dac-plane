#!/usr/bin/env bash
# Prints the number of the merged release-please pull request whose merge commit
# is COMMIT, and exits 1 when there is none. A pull request counts only if it was
# opened from a branch of this repository, not a fork, into main: the merge of a
# release-please pull request is the decision to release, so nothing else may stand
# for it. Needs GH_TOKEN.
set -euo pipefail
commit=${1:?usage: merged-release-pr.sh COMMIT}
repository=${GITHUB_REPOSITORY:-dac-plane/dac-plane}
number=$(gh api "repos/$repository/commits/$commit/pulls" 2>/dev/null | jq -r --arg commit "$commit" --arg repo "$repository" \
	'[.[] | select(.merged_at != null and .base.ref == "main" and .base.repo.full_name == $repo and .head.repo.full_name == $repo and .merge_commit_sha == $commit and (.head.ref | startswith("release-please--")))][0].number // empty' 2>/dev/null || true)
if [[ -z "$number" ]]; then
	exit 1
fi
printf '%s\n' "$number"
