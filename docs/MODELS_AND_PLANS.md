# Adding your own model and defining a plan

Two related pieces, both local to your machine (`~/.oaica/`, never synced
anywhere): the **model manifest** records what a self-hosted model actually
is, and a **plan** names a full opus/sonnet tier split so you don't retype
`--model`/`--sonnet-model` every launch.

## The model manifest

`~/.oaica/models.json` is a declared record per model id — engine, arch,
quantization, real context window, launch flags, and free-text notes for
whatever you had to discover the hard way. It is separate from two other
things that already exist and do NOT replace it:

- `~/.oaica/local_servers.json` — runtime state (is a server up right now,
  on what port). Rewritten every `oaica serve` start/stop.
- The live `/v1/models` probe `oaica launch` makes at launch time — asks a
  *running* backend what it thinks its window is, right now, over the
  network. This always wins when it succeeds (see below); the manifest is
  the fallback for when nothing answered yet.

### Add a model

```shell
oaica model add oaica-35b-a3b-vision \
  --engine vllm \
  --arch Qwen3_5MoeForConditionalGeneration \
  --quant awq-w4a16 \
  --context-window 262144 \
  --max-output-tokens 32000 \
  --gpu-mem-gb 73 \
  --notes "GPU0+GPU2 on a100b behind oaicalb; Mamba align mode needs max-num-batched-tokens>=2096"
```

`--engine` is one of `vllm`, `llama.cpp`, `prism-engine`, `ollama-daemon`,
`user-remote` — informational plus a routing hint for future tooling, not
required by launch today.

`--context-window` and `--max-output-tokens` are the two fields that
actually change launch behavior: they feed `CLAUDE_CODE_MAX_CONTEXT_TOKENS`
and how much of that window auto-compact reserves for output, so Claude
Code doesn't fill input right up to the edge and 400 on the next turn.

`oaica pull <model>` also writes a minimal entry automatically after a
download (engine=llama.cpp, the local weights path) — it can't fill in
arch/quant/context-window (the router's pull manifest doesn't carry that
yet), so follow up with `oaica model add` to fill those in. Never
overwrites a hand-added entry.

### Inspect and remove

```shell
oaica model list                          # table of every model in the manifest
oaica model show oaica-35b-a3b-vision     # full detail for one
oaica model rm oaica-35b-a3b-vision
```

### Live probe vs. manifest — which one wins

A running backend's `/v1/models` response is asked first and wins whenever
it answers — it reflects the truth of what's actually serving right now,
including an emergency downsized config (smaller `--max-model-len` because
a co-tenant on a shared GPU is using VRAM you'd normally have). The
manifest is only consulted when the live probe returns nothing: cold
start, an unreachable upstream, or an engine whose `/models` doesn't report
`context_length`/`max_model_len` at all.

## Plans — your own `/opusplan`

Claude Code's built-in `--model opusplan` picks (Opus, Sonnet) from
Anthropic's catalog by a fixed name. A plan does the same thing for your
own models:

```shell
oaica plan set oaica-full \
  --model oaica-35b-a3b-vision \
  --sonnet-model oaica-35b-a3b-vision \
  --oversize dsremote/deepseek-v4-flash \
  --route-policy local-first \
  --description "Full-power vision model, 262k ctx"

oaica launch claude --plan oaica-full
```

`--sonnet-model` is optional — omit it and the plan's Sonnet-tier requests
go to the same model as `--model`. `--oversize` is the larger-context leg
that serves requests the primary cannot hold (the auto-compaction call
near the context ceiling: it must have a strictly larger probed context
window); `--route-policy` is one of local-first (default), remote-first,
auto (failover + failure escalation), local-only, remote-only, weighted
(splits HEALTHY traffic across legs by weight instead of only failing over
— see CLAUDE_TIERS.md's "Route policies" section). Policy precedence per
launch: CLI flag > plan > the remote's `route_policy` in remotes.json >
local-first.

`weighted` needs per-leg weights, set either on the remote
(`remotes.json`'s `"weight": N`) or per-launch with the repeatable
`--shard <model>:<weight>` flag — `--shard` is not currently a saved plan
field, so a `--plan` launch that wants weighting still needs `--shard` (or
`remotes.json` weights) passed alongside `--plan` on the command line.

The same setup can be built interactively: a plain `oaica launch claude`
(no flags) walks a wizard — primary, Sonnet tier, compaction model
(offering only models with a probed-larger window), route policy — and
offers to save the result as a plan.

```shell
oaica plan list
oaica plan show oaica-full
oaica plan rm oaica-full
```

A plan is resolved before anything else in `oaica launch claude`: it just
fills in `--model`/`--sonnet-model` if you didn't pass them explicitly. An
explicit `--model` (or `--sonnet-model`) on the command line always wins
over the plan's value for that field — a plan is a default, not an
override.

## GPU housekeeping (self-hosting on your own box)

If you're running vLLM (or another multiprocess engine) yourself: killing
the main server process does **not** always release GPU memory. vLLM's V1
engine runs a separate `VLLM::EngineCore` child that can outlive its
parent — `nvidia-smi` may even show a stale PID for the memory it holds.

```shell
oaica gpu ps      # list processes holding GPU memory (read-only)
oaica gpu clean    # dry run: list orphaned workers found
oaica gpu clean --yes  # actually kill them
```

`gpu clean` only ever kills a process that is BOTH reparented (its real
parent has exited) AND matches a known worker pattern — it will not touch
a process that still has a live parent, even one it doesn't recognize.

## Discovery, drift-safety, and manual refresh

The picker's "Local Models" / "Remote Models" / "Recommended" / "More"
sections are never cached across launches — every `oaica launch`/`oaica
model` invocation is a fresh process with no persistent state, so a plain
`ollama pull <model>` or a `~/.oaica/remotes.json` edit is visible on the
very next launch automatically. There is nothing to "refresh" in the
common case; it already is.

**If Ollama (or a remote) changes its response format**, the picker
degrades safely instead of corrupting: local models are parsed into typed
Go structs (`api.ListModelResponse`), so an added field is silently
ignored, and a renamed/missing field just leaves that one entry's name
empty — which is already filtered out — rather than breaking the rest of
the list. A dead or unreachable source (router down, a remote asleep) is
reported but never empties the other sources; local models keep working
fully offline even if the router is unreachable.

**To confirm a model is discoverable right now**, without launching Claude
Code just to see the picker:

```shell
oaica model refresh
```

Forces a fresh, live probe of the local daemon, every user remote, and the
OAICA router, and prints what each one currently reports — the same three
sources the picker sections are built from.

**To make a memorable shortcut** for a model you don't want to wait on a
rename/config fix for — e.g. `ollama/glm-5.3-flash:cloud` under a short
name:

```shell
oaica model alias glm --target ollama/glm-5.3-flash:cloud
oaica launch claude --model glm    # resolves to the real target first
```

An alias is checked before anything else in model resolution — it always
wins if defined, is stored in `~/.oaica/aliases.json`, and is entirely
local: nothing about it depends on discovery, the router, or waiting for
anyone to fix or rename something upstream.

```shell
oaica model alias list
oaica model alias show glm
oaica model alias rm glm
```

**To get an Ollama `:cloud` model (like `glm-5.3-flash:cloud`) onto every
client box in one shot** — each box's daemon needs its own pull (the pull
only registers a lightweight manifest pointer locally; the real weights
stay hosted), so there's no single fleet-wide action — use
`tools/a100b/pull-ollama-cloud.sh` (edit the `HOSTS=` list for your fleet):

```shell
tools/a100b/pull-ollama-cloud.sh                        # pulls its DEFAULT_MODELS list
tools/a100b/pull-ollama-cloud.sh glm-5.3-flash:cloud kimi-k2.7-code:cloud
```

## See also

- [docs/CLAUDE_TIERS.md](CLAUDE_TIERS.md) — how tier resolution,
  `--sonnet-model`, `--oversize`, route policies, the circuit-breaker
  failover and the interactive wizard work underneath `--plan` (plans
  cover cross-remote secondaries + oversize since v0.5.1).
