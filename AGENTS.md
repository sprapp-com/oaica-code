# AGENTS.md

## Building

For a full build from the repository root:

```sh
cmake -B build .
cmake --build build --parallel 8
./ollama serve
```

For quick Go-only iteration against an existing native payload:

```sh
go build .
go run . serve
```

The instructions above are inherited upstream Ollama text. This fork's own
binary is built with `scripts/build_oaica.sh` (it writes
`site/download/VERSION.txt`), and the Claude Code launcher lives in
`cmd/launch`.

See `docs/development.md` for prerequisites, platform notes, GPU backends, and
the full development workflow.

## Pulling in upstream Ollama

This is a fork of `github.com/ollama/ollama` (the `upstream` remote) that
carries real fixes on files upstream also owns, so syncing is a merge with a
known conflict surface. Run the report before merging — it lists, per
conflicting file, which fork commits are at stake:

```sh
scripts/sync-upstream.sh report   # what is waiting, and what it will cost
scripts/sync-upstream.sh merge    # merge it, stopping at the first conflict
scripts/sync-upstream.sh verify   # the same gate CI runs
```

Policy — merge never rebase, never `git checkout --theirs` a shared file — and
the per-path resolution recipes are in `docs/UPSTREAM_SYNC.md`.

An LLM agent doing a sync or an adversarial ("red team") audit round should read
`docs/AGENT_PLAYBOOK.md` first: it holds the audit loop, the brief and evidence rules, the
revert-check standard, the deploy steps and the recurring traps.

The picker's provider and model list is ported from models.dev, corrected by
`cmd/launch/providers/oaica.json` and refreshed with `oaica model catalog sync`
— see `docs/CATALOG.md` before touching either file. The script's
tests are `scripts/tests/upstream_sync_test.sh`.
