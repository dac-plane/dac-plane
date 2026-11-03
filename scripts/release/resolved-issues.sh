#!/usr/bin/env bash
# Prints the issues resolved since a release as Markdown, grouped by kind, with
# each issue's area as a tag. With no argument it lists every resolved issue.
set -euo pipefail
since=${1:-}
repository=${GITHUB_REPOSITORY:-dac-plane/dac-plane}

gh issue list --repo "$repository" --state closed --limit 1000 --json number,title,labels,closedAt,stateReason |
	jq -r --arg since "$since" '
	def has($name): any(.labels[]; .name == $name);
	def kind: (.title | capture("^(?<k>[a-z]+)(\\(|:)").k? // "");
	def group:
		if has("security") then "Security fixes"
		elif has("type:feature") or kind == "feat" then "New features"
		elif has("bug") or kind == "fix" then "Bugs"
		elif has("type:test") or kind == "test" then "Tests and qualification"
		else "Documentation and maintenance" end;
	def text: (.title | sub("^[a-z]+(\\([^)]*\\))?!?:\\s*"; "")) | (.[0:1] | ascii_upcase) as $first | $first + .[1:];
	def tags:
		([(.title | capture("^[a-z]+\\((?<s>[^)]+)\\)").s? // empty)]
			+ [.labels[].name | select(startswith("area:")) | ltrimstr("area:")])
		| unique | map("`" + . + "`") | join(" ");
	[.[] | select(.stateReason == "COMPLETED" and (has("type:epic") | not) and .closedAt >= $since)
		| {number, group: group, line: "- #\(.number) \(text) \(tags)" | rtrimstr(" ")}] as $issues
	| "## All resolved issues",
	  (["Security fixes", "New features", "Bugs", "Tests and qualification", "Documentation and maintenance"][] as $group
		| ($issues | map(select(.group == $group)) | sort_by(.number) | map(.line)) as $lines
		| select($lines | length > 0)
		| "", "### \($group)", "", ($lines[]))
	'
