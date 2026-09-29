# Agent playbook: merging upstream Ollama and red-team auditing

For an LLM agent (or a person) working in this fork. It records what the audit
rounds so far have taught. It does not replace `docs/UPSTREAM_SYNC.md` (the merge
mechanics and per-path recipes) or `AGENTS.md` (build and release); read those first.

## 1. What this repository is

`oaica` is a Go CLI forked from Ollama. Three code areas carry the fork's own
behaviour, and every audit is split along them:

| Leg | Paths | Job |
|---|---|---|
| 1 | `server/`, `middleware/`, `openai/`, `api/`, `anthropic/`, `llm/`, `manifest/` | local server, wire translation (OpenAI chat, Responses, Anthropic) |
| 2 | `cmd/launch/` | client proxy, launcher for third-party CLIs, catalog and provider sync, credential and config files |
| 3 | `tools/gateway/` (a **separate Go module**) | metered gateway: keys, quotas, ledger, meterhub reports |

**The root `main` links `anthropic` and `cmd/launch` but not `server`, `openai`,
`middleware` or `llm`** (`go list -deps .`). A fix in the unlinked packages does not
change the shipped thin client, so an identical binary hash after such a round is
correct. The gateway ships separately.

## 2. Merging upstream where it makes sense

1. `scripts/sync-upstream.sh report`. Read the per-file fork-commit list before touching
   anything. Merge, never rebase; never `git checkout --theirs` a shared file.
2. Decide per upstream change, not per merge: take what is a genuine fix or a feature the
   fork wants; skip what conflicts with a fork decision. Fork decisions live in pinned tests
   (for example `TestSchedAlreadyCanceled`, the argument-less-call fold). **A red test you did
   not write after a merge is a pinned decision, not a stale test.** Reconcile the code, not
   the test.
3. New fork behaviour goes in new files; keep fork hunks on shared files small.
4. After the merge run `scripts/sync-upstream.sh verify`, then the audit loop below over the
   merged areas. Upstream code is not audited by upstream for this fork's invariants (the
   round 115 manifest write and `keep_alive` findings were inherited from upstream).

## 3. The audit loop ("bmad audit")

One round = three read-only auditors (one per leg) + adjudication + fixes + deploy. Repeat
until a round comes back with nothing to fix.

**Setup.** `git archive <HEAD>` into three read-only copies under the session scratchpad; write
one brief there (not in `/tmp`, which gets cleaned). Launch three agents in the background.
The brief must contain: the invariant, the scope per leg, the already-known list, the rules of
evidence, and the report format. Rotate the theme each round (router, request conversion,
policy, secrets, hostile clients, regression and fuzz, time and concurrency, siblings and
operational readiness, failure and shutdown, state that survives restarts). Always include
"regressions in the previous round's code" first.

**The invariant.** One client body plus one upstream body gives the same verdict, bytes, ids
and block order on every arm and every door. Policy (caps, auth, redaction) holds on every
door. Nothing leaks. Hostile input never panics or hangs. Behaviour is correct over time, under
concurrency, on failure and at shutdown.

**Rules of evidence (for auditors).**
- Read-only on the repo. Every finding needs a probe the auditor ran that went RED, with the
  command and output. Prefer a whole-turn comparison across arms to code reading.
- Rank a = live producer, b = a wire shape the fixtures state, c = no producer.
- No style, dead-code or test-gap findings.
- Run one test at a time with `-run`; never a whole slow package. Do not `go build` binaries.

**Adjudication (for you).**
1. Re-verify each finding with your own fail-first probe. Do not trust the report.
2. Fix it, and give every fix a permanent pin (`roundNNN_legN_*_test.go`, comment naming the
   round and finding id).
3. **Revert-check every element of the fix to a behavioural RED.** A revert that fails to
   compile is not a check; redo it (for example `if false && cond`, not deleting a variable).
   An element that stays green when reverted is not load-bearing: drop it, do not ship it.
4. A finding with no producer, or one that would reverse a pinned decision, is RECORDED with a
   pin that states the current behaviour, not fixed.
5. Ask for a control test for every refusal: the same shape that must still be served (the
   round 115 catalog offline same-source test caught a missing write that the refusal test did
   not).

**Commit discipline.** Run the suite to a file, read it, then commit. Never chain
`go test; git commit`. When a fix reddens an old pin, the old pin usually states a deliberate
decision; the fix was wrong.

**Deploy.** Build with the release flags
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "-s -w"`
(a plain `go build` gives a 25 MB binary; do not install it). Copy with scp to a temp name, verify
`sha256sum`, back up the live binary, `mv -f` over the live path (atomic; avoids ETXTBSY), copy
to `~/.local/bin`, verify all paths, restart the service, verify `/proc/<pid>/exe`, smoke both the
document and the streamed arm. Compare by sha256, never `--version` (dev builds report `0.0.0`).
The gateway needs its own checklist: `oaica-gateway --check --config <live>`,
`upstream_key_env` for any per-model upstream, and ledger disk space.

## 4. Lessons that recur (read before writing a fix)

- **Decode, never prefix-match** JSON. Round 107's error-frame watch missed key-sorted output.
- **A new allowlist or comparison needs the same-shape-different-input probe on day one**
  (uppercase digest, trailing-dot host, same name at another slot).
- **After patching N call sites, re-grep and diff.** Rounds 109 and 111 both left a sibling.
- **Python-scripted edits must `assert count == 1`.** A silent no-match shipped half a fix in
  round 115.
- **Append-only logs**: a single-writer log may truncate back after a short write; a
  multi-writer log must recover in the reader, because a writer-side newline check races another
  process's half-written row.
- **Truncate-then-write is a bug for any file another reader can open** (`os.WriteFile` on a
  manifest). Write a temp file outside the reader's glob depth, then rename.
- **Lax upstream typing**: if the gateway reads a field as bool and the upstream coerces
  `"true"`, the two disagree. Refuse or normalise; never read one field two ways.
- **Time**: a stamp in the future is not fresh. Use monotonic time for ages.
- **Shutdown**: a drain that ends at the HTTP server leaves queues (reports, ledgers) behind.
- **Test traps**: `t.Cleanup` is LIFO (register a held request's release after the server); a
  `_<goos>_test.go` or `_arm_test.go` suffix is an implicit build constraint; ledger rows are
  written after the response, so wait for them; a short fixed-length body cannot show appended
  frames (use chunked); loopback zero-window timers can stall hundreds of ms; never
  `pkill -f <pattern>` from a shell whose own command line contains it.
- Redirect long suite output to a file so a rare red can be identified afterwards.
- Never kill a process you did not launch; a shared GPU host needs its claim rules.
