#!/usr/bin/env bash
# Tests the scripts that decide whether a commit may be released: check-tag.sh
# (both its signed-tag path and the release-please path), merged-release-pr.sh and
# check-notes.sh. A stub gh answers from files, over a small repository built for
# each run, so what is accepted and what is refused is pinned without GitHub.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fixture="$work/repo"
stub="$work/stub"
mkdir -p "$fixture" "$stub" "$work/bin"

repository=dac-plane/dac-plane
kit_module=github.com/dac-plane/dac-plane/adapterkit
failures=0
cases=0

# gh answers the three reads the scripts make from files in $STUB.
cat >"$work/bin/gh" <<'STUB'
#!/usr/bin/env bash
[[ "$1" == api ]] || exit 1
case "$2" in
	*/git/ref/tags/*) file="$STUB/ref-$(printf '%s' "${2##*/git/ref/tags/}" | tr '/' '_').json";;
	*/git/tags/*) file="$STUB/tag-${2##*/git/tags/}.json";;
	*/commits/*/pulls) file="$STUB/pulls.json";;
	*) exit 1;;
esac
[[ -f "$file" ]] || exit 1
cat "$file"
STUB
chmod +x "$work/bin/gh"

# A repository with every module requiring one kit version, the notes, the manifest.
write_modules() {
	local kit=$1
	for module in . adapters/codex adapters/e2b adapters/s3 adapters/github; do
		mkdir -p "$fixture/$module"
		printf 'module example.com/%s\n\ngo 1.26\n\nrequire %s %s\n' "$(printf '%s' "$module" | tr -c 'a-z0-9\n' '_')" "$kit_module" "$kit" >"$fixture/$module/go.mod"
	done
}
# build_fixture [migration|none]: the previous release v0.4.0, then the release
# commit, which adds a database migration unless told not to.
build_fixture() {
	local mode=${1:-migration}
	rm -rf "$fixture" && mkdir -p "$fixture/docs/releases" "$fixture/adapterkit" "$fixture/internal/platform/migrations/migrations"
	cd "$fixture"
	git init -q -b main
	git config user.email test@example.com
	git config user.name test
	printf 'select 1;\n' >internal/platform/migrations/migrations/00001_first.sql
	git add -A && git commit -q -m previous && git tag v0.4.0
	write_modules v0.5.0
	printf '{".": "0.5.0"}\n' >.release-please-manifest.json
	printf '# v0.5.0\n' >docs/releases/v0.5.0.md
	printf -- '- [v0.5.0](v0.5.0.md)\n' >docs/releases/README.md
	printf 'package kit\n' >adapterkit/kit.go
	if [[ "$mode" == migration ]]; then
		printf 'select 2;\n' >internal/platform/migrations/migrations/00002_second.sql
	fi
	git add -A && git commit -q -m release
	git remote add origin "$fixture"
	head=$(git rev-parse HEAD)
}

lightweight() { printf '{"object":{"type":"commit","sha":"%s"}}' "$2" >"$stub/ref-$(printf '%s' "$1" | tr '/' '_').json"; }
annotated() { # tag, tag object sha, commit, verified
	printf '{"object":{"type":"tag","sha":"%s"}}' "$2" >"$stub/ref-$(printf '%s' "$1" | tr '/' '_').json"
	printf '{"object":{"sha":"%s"},"verification":{"verified":%s,"reason":"x"}}' "$3" "$4" >"$stub/tag-$2.json"
}
pulls() { # head branch, head repo, merge commit
	printf '[{"number":7,"merged_at":"2026-01-01T00:00:00Z","base":{"ref":"main","repo":{"full_name":"%s"}},"head":{"ref":"%s","repo":{"full_name":"%s"}},"merge_commit_sha":"%s"}]' "$repository" "$1" "$2" "$3" >"$stub/pulls.json"
}

# run NAME EXPECT(ok|refuse) -- command...
run() {
	local name=$1 expect=$2 status=0 output
	shift 3
	cases=$((cases + 1))
	output=$(cd "$fixture" && PATH="$work/bin:$PATH" STUB="$stub" GITHUB_REPOSITORY=$repository "$@" 2>&1) || status=$?
	if [[ "$expect" == ok && $status != 0 ]] || [[ "$expect" == refuse && $status == 0 ]]; then
		printf 'FAIL %s: expected %s, exit %s\n%s\n' "$name" "$expect" "$status" "$output" >&2
		failures=$((failures + 1))
	fi
}
reset() { rm -f "$stub"/*; (cd "$fixture" && git checkout -q -- . && git clean -fdq); }

check=("$here/check-tag.sh")

build_fixture
good_pr() { pulls release-please--branches--main "$repository" "$head"; }

# --- the release-please path: a final release from a merged release pull request.
merge_env=(env GITHUB_REF_TYPE=branch)
reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; good_pr
run "merge: a release pull request merge" ok -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; pulls release-please--branches--main someone/dac-plane "$head"
run "merge: a pull request from a fork" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; pulls feature/x "$repository" "$head"
run "merge: a pull request from another branch" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; pulls release-please--branches--main "$repository" deadbeef
run "merge: a pull request merged as another commit" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; printf '[]' >"$stub/pulls.json"
run "merge: no pull request at all" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 0000000000000000000000000000000000000000; lightweight adapterkit/v0.5.0 "$head"; good_pr
run "merge: the tag names another commit" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 0000000000000000000000000000000000000000; good_pr
run "merge: the kit tag names another commit" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; good_pr
run "merge: no kit tag" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.6.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; good_pr; cp "$fixture/docs/releases/v0.5.0.md" "$fixture/docs/releases/v0.6.0.md"
run "merge: the manifest names another version" refuse -- "${merge_env[@]}" "${check[@]}" v0.6.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.4.0 "$head"; good_pr; write_modules v0.4.0
run "merge: modules require another kit version" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; good_pr; sed -i.bak 's/v0.5.0/v0.4.0/' "$fixture/adapters/s3/go.mod"; rm -f "$fixture/adapters/s3/go.mod.bak"
run "merge: one module disagrees" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; good_pr; rm "$fixture/docs/releases/v0.5.0.md"
run "merge: no release notes warns and does not stop it" ok -- "${merge_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0-rc.1 "$head"; lightweight adapterkit/v0.5.0-rc.1 "$head"; good_pr
run "merge: a rehearsal tag" refuse -- "${merge_env[@]}" "${check[@]}" v0.5.0-rc.1

# --- the signed path: a tag a maintainer pushed.
signed_env=(env GITHUB_REF_TYPE=tag)
reset; annotated v0.5.0 aaaa "$head" true; annotated adapterkit/v0.5.0 bbbb "$head" true
run "signed: both tags verified" ok -- "${signed_env[@]}" "${check[@]}" v0.5.0

reset; annotated v0.5.0 aaaa "$head" false; annotated adapterkit/v0.5.0 bbbb "$head" true
run "signed: the release tag is unverified" refuse -- "${signed_env[@]}" "${check[@]}" v0.5.0

reset; annotated v0.5.0 aaaa "$head" true; annotated adapterkit/v0.5.0 bbbb "$head" false
run "signed: the kit tag is unverified" refuse -- "${signed_env[@]}" "${check[@]}" v0.5.0

reset; annotated v0.5.0 aaaa 0000000000000000000000000000000000000000 true; annotated adapterkit/v0.5.0 bbbb "$head" true
run "signed: the tag names another commit" refuse -- "${signed_env[@]}" "${check[@]}" v0.5.0

reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"
run "signed: a lightweight tag is not a signature" refuse -- "${signed_env[@]}" "${check[@]}" v0.5.0

reset; annotated v0.5.0-rc.1 aaaa "$head" true; annotated adapterkit/v0.5.0-rc.1 bbbb "$head" true
run "signed: a verified rehearsal tag" ok -- "${signed_env[@]}" "${check[@]}" v0.5.0-rc.1

reset; annotated v0.5 aaaa "$head" true
run "signed: not a release tag" refuse -- "${signed_env[@]}" "${check[@]}" v0.5

# --- notes-required.sh: exit 0 when notes are required, 1 when they are optional.
reset
run "notes-required: a migration since the last release" ok -- "$here/notes-required.sh" v0.5.0
run "notes-required: no earlier release to compare with" ok -- "$here/notes-required.sh" v0.3.0

# --- a release that adds no migration: its generated changelog is enough.
build_fixture none
reset
run "notes-required: no migration since the last release" refuse -- "$here/notes-required.sh" v0.5.0
reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; good_pr; rm "$fixture/docs/releases/v0.5.0.md"
run "merge: no notes and no migration" ok -- "${merge_env[@]}" "${check[@]}" v0.5.0
reset; lightweight v0.5.0 "$head"; lightweight adapterkit/v0.5.0 "$head"; good_pr
run "merge: notes written anyway" ok -- "${merge_env[@]}" "${check[@]}" v0.5.0
reset; rm "$fixture/docs/releases/v0.5.0.md"
run "notes: none needed and none written" ok -- "$here/check-notes.sh"
reset; printf '' >"$fixture/docs/releases/README.md"
run "notes: written but not linked, even when optional" refuse -- "$here/check-notes.sh"
build_fixture
good_pr

# --- merged-release-pr.sh on its own.
reset; good_pr
run "pr: a release pull request merge" ok -- "$here/merged-release-pr.sh" "$head"
reset; pulls release-please--branches--main someone/dac-plane "$head"
run "pr: a fork" refuse -- "$here/merged-release-pr.sh" "$head"
reset; pulls feature/x "$repository" "$head"
run "pr: another branch" refuse -- "$here/merged-release-pr.sh" "$head"

# --- check-notes.sh.
reset
run "notes: present and linked" ok -- "$here/check-notes.sh"
reset; rm "$fixture/docs/releases/v0.5.0.md"
run "notes: missing warns and does not fail" ok -- "$here/check-notes.sh"
reset; printf '' >"$fixture/docs/releases/README.md"
run "notes: not linked" refuse -- "$here/check-notes.sh"

if ((failures > 0)); then
	printf '%d of %d release script cases failed\n' "$failures" "$cases" >&2
	exit 1
fi
printf 'release scripts: %d cases passed\n' "$cases"
