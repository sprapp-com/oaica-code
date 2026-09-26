# Pulling upstream Ollama into this fork

This repository is a fork of `github.com/ollama/ollama`, configured as the
`upstream` remote (see `git remote -v`). It is a full fork — the CLI, the
server, the client libraries and the engine are all here — so upstream's work
usually matters, and an upstream sync is a routine operation, not a one-off.

The routine is one script:

```sh
scripts/sync-upstream.sh report     # what is waiting, and what it will cost
scripts/sync-upstream.sh merge      # take it, stopping at the first conflict
scripts/sync-upstream.sh verify     # the same gate CI runs
```

`report` is not optional. It runs a real three-way merge in the index-free
`git merge-tree` mode and prints, for every file that would conflict, the fork
commits touching that file and upstream's commits touching it. That is the
difference between resolving a conflict by reading the code and resolving it by
guessing which of 575 fork commits are at stake.

## Policy: merge, never rebase

The fork carries published tags and 575 commits of its own. Rebasing them onto
upstream would rewrite every one — and every tag — to buy a linear history
nobody reads. `merge` is the operation; the script only ever merges, and never
pushes.

**Never `git checkout --theirs <shared file>`.** Upstream does not contain this
fork's fixes. On a shared file, "take theirs" is a silent deletion of audit
work; the per-file commit list from `report` is the record of exactly what would
be deleted, which is why the merge path prints it.

## Path ownership

| Path | Owner | How to resolve a conflict |
| --- | --- | --- |
| `cmd/launch/**` | shared | ours and upstream's both — read both sides; both trees built launch integrations independently |
| `cmd/cmd.go`, `cmd/interactive.go`, `cmd/tui/**` | shared, fork-heavy | ours wins on rebranding and on the audit fixes; take upstream's new features |
| `anthropic/**`, `openai/**` | shared | both sides are real fixes to the same translation code — reconcile hunk by hunk |
| `cmd/config/**`, `cmd/bench/**`, `progress/**` | shared | check upstream first: it has fixed some of the same bugs (see below) |
| `go.sum` | generated | never merge by hand — take either side, then `go mod tidy` |
| `app/ui/app/public/launch-icons/*.svg` | generated | rebuild/regenerate; an add/add here means both trees added an icon for the same integration |
| `server/`, `llm/`, `ml/`, `runner/`, `discover/`, `fs/`, `model/` | upstream | take upstream; the fork does not change the engine or the local model store |
| `site/`, `docs/superpowers/`, `OAICA_FORK_PLAN.md`, `SERVERLESS_ARCH.md`, `SELF_HOST.md`, `ENTERPRISE.md`, `docs/CLAUDE_TIERS.md` | fork | take ours; upstream has nothing to say about these |
| `*.md` at the root | shared | `README.md` is ours-first (the fork rewrote it); the rest are case by case |

Anything not listed: `git log --oneline <fork-commits> -- <path>` from the
`report` output is the tiebreaker. If the file has no fork commit listed, our
side is a rebrand or a formatting change and upstream's version can be taken
after checking for the string `ollama` that should read `oaica` in user-facing
text.

## Recipes for the conflicts that recur

**`go.sum`.** Take either side, then `go mod tidy`, then `verify`. The file is
a checksum ledger, not a document.

**Upstream fixed the same bug.** This happens, and it is the cheapest possible
resolution: take upstream's fix, drop ours. Two live examples from the 2026-09-27
report:

- `progress/progress.go` and `progress/spinner.go` — upstream's `43983edf1
  progress: fix data races on ticker, states, spinner, and bar state (#17445)`
  and `b5d373f34 fix data races in progress and sched (#18319)` cover the same
  races this fork's `6ef07c4e2` and `8024520eb` fixed. Take upstream's; delete
  the fork hunks. If the fork's `*_integrity_test.go` for that file pins
  behaviour upstream's fix also guarantees, keep the test (it is a fork-only
  file, so it does not conflict) and let it prove upstream's fix.
- Check for this *before* reconciling: `report` puts upstream's commits touching
  each conflicting file directly under the fork's, so a fix that has landed
  upstream is visible as a subject line that says so.

**A file upstream deleted and the fork modified.** Read why upstream deleted it
(usually "removed the built-in agent", "removed dead code"). If the fork's
modification is an audit fix to code upstream deleted, the fix goes; if it is
fork functionality that upstream's deletion missed, the file stays under a
fork-owned name. Never restore a file upstream deleted without a commit message
saying why.

**Generated assets.** `app/ui/app/public/launch-icons/*.svg` and anything else
the build regenerates: take upstream's, then regenerate ours if the fork has an
icon upstream lacks.

## Why the fork's tests are the insurance

Every fork fix on a shared file carries a permanent test in a file the fork
added (`*_integrity_test.go`, and the `*_store_race_test.go` /
`*_foreign_store_concurrency_integrity_test.go` family). Those files are
fork-only: upstream never adds them, so they never conflict — and they are the
durable artifact of the fix, not the diff.

That matters at sync time in both directions:

- If a resolution drops a fork hunk and the test still passes, the behaviour is
  preserved by whatever is left (upstream's own fix, most often). That is the
  evidence that dropping the hunk was safe.
- If a resolution drops a fork hunk and its test fails, the fix was real and the
  hunk must go back. `go test ./cmd/... ./anthropic/... ./tools/...` is where
  that shows up.

So: **write the test as well as the fix, and never delete an
`*_integrity_test.go` to make a merge quiet.** A merge that silences a test is a
merge that reintroduced the bug.

## Keeping the next sync cheap

1. **Fork fixes on shared files stay small and in fork-authored hunks.** A
   fork edit that rewrites upstream's surrounding code costs a conflict on every
   future sync; the same fix appended as a helper called from one changed line
   costs one.
2. **Do not restyle or reformat upstream code.** Comment and string rebranding
   in user-facing text is a product requirement and stays — the root command
   really is `oaica` (`cmd/cmd.go`'s `Use`), so a message telling the user to
   run `ollama launch …` is wrong. But a rebrand inside a code comment, or a
   reflow of upstream's paragraph, buys nothing and conflicts forever.
3. **New fork functionality goes in new files** under a fork-owned name.
   `cmd/launch/` is shared — upstream added its own DeepSeek Harness integration
   (`39df91c98`) while this fork added one, which is an add/add conflict on the
   same filename. When the work is the fork's own idea rather than a fix to
   shared code, give it a file upstream does not have.

## After the merge

1. `scripts/sync-upstream.sh verify` — `go build ./...`, `go vet` and `go test`
   over the same package set `.github/workflows/oaica-ci.yaml` uses.
2. Run the adversarial audit round over the merged areas, per the standing
   doctrine for this repository: an upstream merge is new code, and new code on
   shared files is exactly where a fix and its test can be pulled apart.
3. Re-tag nothing. Releases are cut from the fork's own tagging scheme
   (`oaica-v*`), and the release path is documented in `AGENTS.md`.
