#!/usr/bin/env bash
#
# upstream_sync_test.sh — tests scripts/sync-upstream.sh against real git
# repositories built in a temp directory. The script's whole job is to reason
# about a merge before performing it, and the only way to test that reasoning is
# with a merge that really has the shapes it claims: none waiting, clean,
# conflicting, dirty, and a gate that fails.
#
# Run: scripts/tests/upstream_sync_test.sh
#
# Exit: 0 all checks passed, 1 at least one failed.

set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/../sync-upstream.sh"

[ -x "$script" ] || {
	printf 'missing or not executable: %s\n' "$script" >&2
	exit 1
}

pass=0
fail=0
ok() {
	pass=$((pass + 1))
	printf 'ok   %s\n' "$1"
}
bad() {
	fail=$((fail + 1))
	printf 'FAIL %s\n' "$1"
	[ $# -gt 1 ] && printf '     %s\n' "$2"
}

contains() {
	case "$1" in
	*"$2"*) return 0 ;;
	*) return 1 ;;
	esac
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

gitc() { git -C "$1" -c user.email=t@t -c user.name=t "${@:2}"; }

# make_fixture <name> — a bare "upstream" repo at $work/<name>/upstream.git, a
# seeder that pushes an initial commit to it, and a fork clone whose origin is
# that bare repo (so UPSTREAM_REMOTE=origin and no network is involved).
make_fixture() {
	local name="$1"
	local root="$work/$name"
	mkdir -p "$root"
	# --initial-branch=main: the fixture must not depend on the host git's
	# init.defaultBranch, or "clone" checks out an unborn branch on CI.
	git init -q --bare --initial-branch=main "$root/upstream.git"
	git init -q --initial-branch=main "$root/seed"
	printf 'base\n' >"$root/seed/shared.txt"
	printf 'base\n' >"$root/seed/other.txt"
	gitc "$root/seed" add -A
	gitc "$root/seed" commit -q -m "base"
	gitc "$root/seed" remote add origin "$root/upstream.git"
	gitc "$root/seed" push -q origin HEAD:main

	git clone -q "$root/upstream.git" "$root/fork"
	gitc "$root/fork" checkout -q main
	printf '%s' "$root"
}

# Advance upstream by one commit touching <file>.
upstream_commit() {
	local root="$1" file="$2" body="$3" subject="$4"
	printf '%s\n' "$body" >>"$root/seed/$file"
	gitc "$root/seed" add -A
	gitc "$root/seed" commit -q -m "$subject"
	gitc "$root/seed" push -q origin HEAD:main
}

# Advance the fork by one commit touching <file>.
fork_commit() {
	local root="$1" file="$2" body="$3" subject="$4"
	printf '%s\n' "$body" >>"$root/fork/$file"
	gitc "$root/fork" add -A
	gitc "$root/fork" commit -q -m "$subject"
}

# A `go` that records its arguments and exits with $GO_EXIT (default 0), so
# verify's gate is tested without building anything.
fake_go() {
	local root="$1"
	mkdir -p "$root/bin"
	cat >"$root/bin/go" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$GO_LOG"
exit "${GO_EXIT:-0}"
EOF
	chmod +x "$root/bin/go"
}

run() { # run <repo> [args...] → stdout in $out, exit code in $rc
	local repo="$1"
	shift
	out="$(UPSTREAM_REMOTE=origin UPSTREAM_BRANCH=main "$script" -C "$repo" "$@" 2>&1)"
	rc=$?
}

# run_fake <fixture-root> <repo> [args...] — same as run, with the fake `go` wired
# in, and with GO/GO_LOG/GO_EXIT UNset afterwards. The prefixes on a function
# call persist in bash, so without this the fake go (and GO_EXIT=1 from the
# failing-gate case) would still be in force for every later case that shells
# out to a real build.
run_fake() {
	local root="$1" repo="$2"
	shift 2
	GO_LOG="$root/go.log" GO="$root/bin/go" run "$repo" "$@"
	unset GO GO_LOG GO_EXIT
}

# --- 1. nothing waiting -----------------------------------------------------
root="$(make_fixture uptodate)"
run "$root/fork" report
if [ "$rc" -eq 0 ] && contains "$out" "nothing to merge"; then
	ok "report on an up-to-date fork says nothing is waiting"
else
	bad "report on an up-to-date fork" "rc=$rc out=$out"
fi

# --- 2. clean divergence: different files ----------------------------------
root="$(make_fixture clean)"
fork_commit "$root" other.txt "fork" "fork: other.txt"
upstream_commit "$root" shared.txt "upstream" "upstream: shared.txt"
run "$root/fork" report
if [ "$rc" -eq 0 ] && contains "$out" "the merge is clean"; then
	ok "report says a merge over disjoint files is clean"
else
	bad "report says a merge over disjoint files is clean" "rc=$rc out=$out"
fi

fake_go "$root"
run_fake "$root" "$root/fork" merge
if [ "$rc" -eq 0 ] && contains "$out" "gate green"; then
	ok "merge takes a clean divergence and runs the gate"
else
	bad "merge takes a clean divergence" "rc=$rc out=$out"
fi
if [ "$(cat "$root/go.log" 2>/dev/null | wc -l | tr -d ' ')" -eq 3 ] &&
	contains "$(cat "$root/go.log")" "build ./..." &&
	contains "$(cat "$root/go.log")" "vet ./cmd/... ./anthropic/..." &&
	contains "$(cat "$root/go.log")" "test ./cmd/... ./anthropic/... ./tools/..."; then
	ok "verify runs build, vet and test over the same package set as CI"
else
	bad "verify runs the CI gate" "$(cat "$root/go.log" 2>/dev/null)"
fi
if contains "$(cat "$root/fork/shared.txt")" "upstream" && contains "$(cat "$root/fork/other.txt")" "fork"; then
	ok "the merge kept both sides"
else
	bad "the merge kept both sides" "shared=$(cat "$root/fork/shared.txt") other=$(cat "$root/fork/other.txt")"
fi

# --- 3. conflicting divergence ---------------------------------------------
root="$(make_fixture conflicting)"
fork_commit "$root" shared.txt "fork-side" "fork: a fix upstream does not have"
upstream_commit "$root" shared.txt "upstream-side" "upstream: rewrote the same line"
run "$root/fork" report
if [ "$rc" -eq 0 ] && contains "$out" "shared.txt" && contains "$out" "fork: a fix upstream does not have" && contains "$out" "upstream: rewrote the same line"; then
	ok "report names the conflicting file and both sides' commits"
else
	bad "report names the conflict and both sides' commits" "rc=$rc out=$out"
fi

fake_go "$root"
run_fake "$root" "$root/fork" merge
if [ "$rc" -eq 2 ] && contains "$out" "unresolved:" && contains "$out" "the per-file commit lists"; then
	ok "merge stops at the conflict, exits 2, and points at the record of what is at stake"
else
	bad "merge stops at the conflict" "rc=$rc out=$out"
fi
if [ -e "$root/fork/.git/MERGE_HEAD" ]; then
	ok "the conflicted merge is left in the index for the resolver"
else
	bad "the conflicted merge is left in the index"
fi

# --- 4. a merge already in progress ---------------------------------------
run_fake "$root" "$root/fork" merge
if [ "$rc" -eq 1 ] && contains "$out" "a merge is already in progress"; then
	ok "merge refuses to start on top of an unfinished merge"
else
	bad "merge refuses a merge in progress" "rc=$rc out=$out"
fi
gitc "$root/fork" merge --abort 2>/dev/null

# --- 5. dirty tree --------------------------------------------------------
root="$(make_fixture dirty)"
upstream_commit "$root" other.txt "upstream" "upstream: other.txt"
printf 'uncommitted\n' >>"$root/fork/shared.txt"
run "$root/fork" merge
if [ "$rc" -eq 1 ] && contains "$out" "uncommitted changes"; then
	ok "merge refuses a dirty tree"
else
	bad "merge refuses a dirty tree" "rc=$rc out=$out"
fi
if contains "$(cat "$root/fork/other.txt")" "upstream"; then
	bad "the refused merge did not merge anything"
else
	ok "the refused merge merged nothing"
fi

fake_go "$root"
run_fake "$root" "$root/fork" --allow-dirty merge
if [ "$rc" -eq 0 ] && contains "$out" "gate green"; then
	ok "--allow-dirty lets the merge proceed"
else
	bad "--allow-dirty lets the merge proceed" "rc=$rc out=$out"
fi

# --- 6. the gate can fail -------------------------------------------------
root="$(make_fixture badgate)"
upstream_commit "$root" other.txt "upstream" "upstream: other.txt"
fake_go "$root"
GO_EXIT=1 run_fake "$root" "$root/fork" merge
if [ "$rc" -eq 3 ] && contains "$out" "go build ./... failed"; then
	ok "a failing gate stops the sync and exits 3"
else
	bad "a failing gate stops the sync" "rc=$rc out=$out"
fi

# --- 7. --no-fetch uses the ref as it stands ------------------------------
# The offline path: no network, no fetch, the remote-tracking ref as last seen.
run "$root/fork" --no-fetch report
if [ "$rc" -eq 0 ] && contains "$out" "using origin/main as it stands"; then
	ok "--no-fetch reports on the ref as it stands and says so"
else
	bad "--no-fetch reports offline" "rc=$rc out=$out"
fi

# --- 8. argument handling -------------------------------------------------
run "$root/fork" --nonsense
if [ "$rc" -eq 1 ] && contains "$out" "unknown argument"; then
	ok "an unknown argument is a usage error, not a silent no-op"
else
	bad "an unknown argument is a usage error" "rc=$rc out=$out"
fi

run "$root/fork" -C "$work/does-not-exist" report
if [ "$rc" -eq 1 ] && contains "$out" "no such directory"; then
	ok "a missing -C target is an error"
else
	bad "a missing -C target is an error" "rc=$rc out=$out"
fi

printf '\n%s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
