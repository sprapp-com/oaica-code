#!/usr/bin/env bash
#
# sync-upstream.sh — pull github.com/ollama/ollama into this fork.
#
# This fork carries real fixes on files upstream also owns (the audit rounds in
# cmd/launch, anthropic/, openai/, progress/, cmd/), so an upstream sync is a
# merge with a known conflict surface, not a fast-forward. The routine is:
#
#   scripts/sync-upstream.sh report     # what is waiting, and what it will cost
#   scripts/sync-upstream.sh merge      # merge it, stopping at the first conflict
#   scripts/sync-upstream.sh verify     # the same gate CI runs
#
# The policy behind the resolutions is in docs/UPSTREAM_SYNC.md; run
# `report` before `merge` every time, because its per-file commit lists are what
# make a conflict resolvable without re-deriving which fork fixes are at stake.
#
# Nothing here pushes, and nothing here rewrites history: 575 fork commits and
# published tags make a rebase onto upstream a non-starter, so the fork merges.
#
# Exit codes: 0 ok, 1 usage/environment, 2 conflicts (the merge stopped), 3 a
# gate failed.

set -euo pipefail

UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
UPSTREAM_BRANCH="${UPSTREAM_BRANCH:-main}"
MAIN_BRANCH="${MAIN_BRANCH:-main}"
GO="${GO:-go}"

repo="."
command="report"
fetch=1
allow_dirty=0
branch=""
depth=5

die() {
	printf 'sync-upstream: %s\n' "$1" >&2
	exit "${2:-1}"
}

usage() {
	cat <<'EOF'
usage: scripts/sync-upstream.sh [-C <repo>] [--no-fetch] [--allow-dirty]
                                [--branch <name>] [report|merge|verify]

  report   (default) divergence counts, the merge-base, and — from a dry-run
           three-way merge — the files that conflict, each with the fork
           commits touching it and upstream's commits touching it.
  merge    merge the upstream branch into the current one. Refuses a dirty
           tree. Stops at the first conflict and prints the resolution recipe;
           runs verify when the merge is clean.
  verify   go build ./..., go vet, go test — the same gate .github/workflows/
           oaica-ci.yaml runs, so a merge is proven locally before it lands.

  -C <repo>        run against this repository (default: .)
  --no-fetch       use the upstream ref as it is, do not contact the network
  --allow-dirty    let merge run with a dirty tree (report is unaffected)
  --branch <name>  the branch merge checks out / verifies (default: main)

Environment: UPSTREAM_REMOTE (upstream), UPSTREAM_BRANCH (main),
MAIN_BRANCH (main), GO (go).
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	-C)
		[ $# -ge 2 ] || die "-C needs a path"
		repo="$2"
		shift 2
		;;
	--no-fetch)
		fetch=0
		shift
		;;
	--allow-dirty)
		allow_dirty=1
		shift
		;;
	--branch)
		[ $# -ge 2 ] || die "--branch needs a name"
		branch="$2"
		shift 2
		;;
	report | merge | verify)
		command="$1"
		shift
		;;
	-h | --help | help)
		usage
		exit 0
		;;
	*)
		printf 'sync-upstream: unknown argument %s\n\n' "$1" >&2
		usage >&2
		exit 1
		;;
	esac
done

[ -d "$repo" ] || die "no such directory: $repo"
repo="$(cd "$repo" && pwd)"

gitq() { git -C "$repo" "$@"; }

gitq rev-parse --git-dir >/dev/null 2>&1 || die "$repo is not a git repository"

upstream_ref="$UPSTREAM_REMOTE/$UPSTREAM_BRANCH"

# merge-tree --write-tree is how the dry run gets a conflict list WITHOUT
# touching the working tree or the index — the whole point of doing this before
# the merge. It needs git 2.38; older git has no equivalent, and a merge that
# silently skipped the report would be worse than an error here.
check_git_version() {
	local version major minor
	version="$(git --version | awk '{print $3}')"
	major="${version%%.*}"
	minor="$(printf '%s' "$version" | cut -d. -f2)"
	if [ "$major" -lt 2 ] || { [ "$major" -eq 2 ] && [ "$minor" -lt 38 ]; }; then
		die "git $version is too old: the dry-run conflict report needs git >= 2.38 (merge-tree --write-tree)"
	fi
}

do_fetch() {
	[ "$fetch" -eq 1 ] || {
		printf 'sync-upstream: --no-fetch, using %s as it stands\n' "$upstream_ref" >&2
		return 0
	}
	printf 'sync-upstream: fetching %s...\n' "$UPSTREAM_REMOTE"
	if ! gitq fetch --no-tags "$UPSTREAM_REMOTE" "$UPSTREAM_BRANCH" >&2; then
		die "fetch failed; if this machine is offline, re-run with --no-fetch to use the last fetched $upstream_ref"
	fi
}

require_upstream_ref() {
	gitq rev-parse --verify --quiet "$upstream_ref^{commit}" >/dev/null ||
		die "no such ref: $upstream_ref (is the '$UPSTREAM_REMOTE' remote configured? see docs/UPSTREAM_SYNC.md)"
}

# merge_tree_output runs the dry-run three-way merge. `merge-tree` EXITS 1 when
# there are conflicts — which is the case this whole script exists for — so the
# status is swallowed deliberately: under `set -o pipefail` a bare call would
# abort the report the moment it had something to report.
#
# The output is the resulting tree oid, then the conflicting paths, then a blank
# line, then the CONFLICT messages.
merge_tree_output() {
	gitq merge-tree --write-tree --name-only HEAD "$upstream_ref" 2>/dev/null || true
}

# conflict_paths prints just the conflicting paths from that output.
conflict_paths() {
	merge_tree_output | awk 'NR == 1 { next } NF == 0 { exit } NF { print }'
}

merge_base() { gitq merge-base HEAD "$upstream_ref"; }

report() {
	local base ours theirs
	base="$(merge_base)"
	ours="$(gitq rev-list --count "$upstream_ref"..HEAD)"
	theirs="$(gitq rev-list --count HEAD.."$upstream_ref")"

	printf 'sync-upstream: %s is %s commit(s) behind %s, and carries %s of its own\n' \
		"$(gitq symbolic-ref --short -q HEAD || echo HEAD)" "$theirs" "$upstream_ref" "$ours"
	printf 'sync-upstream: merge base %s (%s)\n\n' \
		"$(gitq log -1 --format=%h "$base")" "$(gitq log -1 --format=%cs "$base")"

	if [ "$theirs" -eq 0 ]; then
		printf 'sync-upstream: nothing to merge\n'
		return 0
	fi

	local paths
	paths="$(conflict_paths)"
	if [ -z "$paths" ]; then
		printf 'sync-upstream: the merge is clean — no conflicts. `merge` will take it and run verify.\n'
		return 0
	fi

	local count
	count="$(printf '%s\n' "$paths" | wc -l | tr -d ' ')"
	printf 'sync-upstream: %s file(s) will conflict. For each, these are the fork commits at stake:\n\n' "$count"
	while IFS= read -r path; do
		[ -n "$path" ] || continue
		printf '  %s\n' "$path"
		# Our side: the fork fixes that a careless "take theirs" would delete.
		if gitq log --oneline -"$depth" "$base"..HEAD -- "$path" | sed 's/^/      fork: /' | grep . ; then :; else
			printf '      fork: (none — our side is a rebrand or a deletion, see the policy)\n'
		fi
		# Their side: what upstream did, which is what the hunk is now.
		if gitq log --oneline -3 "$base".."$upstream_ref" -- "$path" | sed 's/^/      upstream: /' | grep . ; then :; else
			printf '      upstream: (none — the conflict is against a file upstream deleted or renamed)\n'
		fi
		printf '\n'
	done <<<"$paths"

	# The two mechanical ones, called out because they are the ones a resolver
	# gets wrong by hand. go.sum is regenerated, not merged.
	if printf '%s\n' "$paths" | grep -qx 'go.sum'; then
		printf 'sync-upstream: go.sum conflicts — resolve with `go mod tidy` after the merge picks up go.mod\n'
	fi
	if merge_tree_output | grep -q 'modify/delete'; then
		printf 'sync-upstream: at least one file is deleted upstream and modified here — read the policy before keeping either side\n'
	fi
	printf 'sync-upstream: policy and per-file recipes: docs/UPSTREAM_SYNC.md\n'
}

verify() {
	local want_branch="${branch:-$MAIN_BRANCH}"
	local current
	current="$(gitq symbolic-ref --short -q HEAD || echo '')"
	if [ -n "$current" ] && [ "$current" != "$want_branch" ]; then
		die "on branch '$current', not '$want_branch' — verify the merge on the branch it lands on, or pass --branch $current" 1
	fi

	# Same three commands as .github/workflows/oaica-ci.yaml. A merge is only
	# done when these are green here: CI would find it, but after it landed.
	printf 'sync-upstream: go build ./...\n'
	(cd "$repo" && "$GO" build ./...) || die "go build ./... failed after the merge" 3

	printf 'sync-upstream: go vet ./cmd/... ./anthropic/...\n'
	(cd "$repo" && "$GO" vet ./cmd/... ./anthropic/...) || die "go vet failed after the merge" 3

	printf 'sync-upstream: go test ./cmd/... ./anthropic/... ./tools/...\n'
	(cd "$repo" && "$GO" test ./cmd/... ./anthropic/... ./tools/...) || die "go test failed after the merge" 3

	printf 'sync-upstream: gate green\n'
}

require_clean() {
	# The merge-in-progress check comes first: a conflicted merge IS a dirty tree,
	# so the order decides which of the two the user is told. Telling them to
	# commit or stash while a merge sits half-resolved points them at the wrong
	# repair.
	if [ -e "$repo/.git/MERGE_HEAD" ]; then
		die "a merge is already in progress — finish it or \`git merge --abort\` first"
	fi
	[ "$allow_dirty" -eq 1 ] && return 0
	if [ -n "$(gitq status --porcelain)" ]; then
		die "the working tree has uncommitted changes — commit or stash them first, so a conflicted merge is the only thing in the tree (--allow-dirty overrides)"
	fi
}

do_merge() {
	require_clean
	do_fetch
	require_upstream_ref

	if [ -z "$(conflict_paths)" ]; then
		printf 'sync-upstream: merging %s (dry run says clean)\n' "$upstream_ref"
		gitq merge --no-ff --no-edit "$upstream_ref" || die "the merge failed even though the dry run was clean — inspect the tree" 2
		verify
		return 0
	fi

	# Conflicted: merge anyway, so the resolver works in the real index with the
	# real hunks — the dry run only said which files, and the per-file commit
	# lists above say why. This is expected to exit non-zero.
	printf 'sync-upstream: merging %s — conflicts expected, this stops in the index\n' "$upstream_ref"
	if gitq merge --no-ff --no-edit "$upstream_ref"; then
		printf 'sync-upstream: merged cleanly after all\n'
		verify
		return 0
	fi
	printf '\n'
	conflict_paths_after_merge() { gitq diff --name-only --diff-filter=U; }
	conflict_paths_after_merge | sed 's/^/  unresolved: /'
	cat <<'EOF'

sync-upstream: resolve each file per docs/UPSTREAM_SYNC.md, then:
    git add <files> && git commit           # keep the merge commit
    scripts/sync-upstream.sh verify         # build, vet, test

Do NOT `git checkout --theirs` a shared file wholesale: upstream does not have
this fork's fixes, and the per-file commit lists from `report` are the record of
what would be deleted.
EOF
	exit 2
}

case "$command" in
report)
	check_git_version
	do_fetch
	require_upstream_ref
	report
	;;
merge)
	check_git_version
	do_merge
	;;
verify)
	verify
	;;
esac
