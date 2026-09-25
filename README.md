# oaica

A thin, CGO-free terminal AI CLI (fork of Ollama's client) backed by our
own hosted API — no local GPU or model download required. It also ships an
optional static site builder (`oaica site new|edit|preview|deploy`).

**Install**

```shell
curl -fsSL https://oaica.com/install.sh | bash    # macOS/Linux
irm https://oaica.com/install.ps1 | iex           # Windows
```

Both installers fetch the archive **and** its checksum from the GitHub release
for the version being installed, so what you install is always the artifact CI
built from that tag:

```shell
# pin an exact version
OAICA_VERSION=0.5.46 curl -fsSL https://oaica.com/install.sh | bash
# fetch from a mirror instead of GitHub (air-gapped hosts)
OAICA_DOWNLOAD_BASE=https://mirror.internal/oaica curl -fsSL https://oaica.com/install.sh | bash
```

Prefer a manual download? Every archive is attached to the
[GitHub releases](https://github.com/sprapp-com/oaica-code/releases) together
with `SHA256SUMS`, `VERSION.txt`, and the install scripts themselves. The
archive extracts to `bin/oaica`, so either extract into `~/.local`
(`tar -C ~/.local -xzf oaica-*.tar.gz`) or move `bin/oaica` onto your `PATH`
yourself (e.g. `/usr/local/bin`).

**Current release:** see the [latest GitHub
release](https://github.com/sprapp-com/oaica-code/releases/latest) — the tag and
its assets are the release, so reading them cannot go stale the way a number
typed into this file does.

**API base URL:** `https://api.oaica.com` (`OAICA_HOST` defaults here;
OpenAI-compatible).

---

## What it is

`oaica` is a terminal client for running and orchestrating AI models. There
is **no local Ollama daemon** in this fork — every command talks either to
oaica's hosted API, to a self-hosted `llama-server` process it manages for
you, or to any OpenAI-compatible endpoint you point it at. Three ways to
use it, pick one (or mix them):

1. **Hosted** — sign in with an oaica API key, run a model, or drive
   Claude Code / other coding agents against it.
2. **Self-host** — pull a GGUF model and serve it locally with
   `llama-server`, free and offline.
3. **Bring your own provider** — register any OpenAI-compatible endpoint
   (Ollama Cloud, z.ai, DeepSeek, your own gateway) as a named remote.

## Quick start: hosted (default)

```shell
oaica signin                                       # or: export OAICA_API_KEY=...  (key from https://oaica.com)
oaica run kat-awq "hello"
oaica launch claude --model kat-awq                 # run Claude Code against it (prompts to install claude)
```

`--yes` is the launcher's own confirm flag, and the command above does not pass
it: a non-interactive launch without it errors `Claude Code is not installed;
re-run with --yes to install it` when Claude Code is missing.

Hosted models: `kat-awq` and `oaica-nemotron-30b-a3b`, both 262,144 context
(`tools/a100b/gateway.json` — the served config, and so the authority on what
actually runs). The repo catalog `models/models.json` advertises a larger
window (1048576) for `oaica-35b-a3b-vision`, which is why `oaica launch`
budgets against what the live probe returns rather than any number written
down here.

Multi-model launches (v0.5.0+): a plain interactive
`oaica launch claude` walks a wizard — when `~/.oaica/plans.json` has entries
its first prompt offers to reuse the plan last launched from this directory
(Enter), pick another saved plan, or start from scratch — then primary, then
the Sonnet/execution tier, then the Haiku tier (Claude Code's background work:
titles, topic detection — unset, a split launch bills those calls at the
primary's price, while a plain native launch keeps Claude Code's own Haiku),
then a compaction/oversize model (only models whose probed
context window is at least the primary's are offered — an equal
window can still take over when the primary fails, though the size
crossover itself needs strictly larger), then a route
policy (`--route-policy local-first|remote-first|auto|local-only|remote-only|weighted`)
with cross-leg failover via a health circuit breaker — and can save the
whole setup as a named plan (Enter at the save prompt overwrites the last-used
plan name, blank skips). Same knobs exist as flags
(`--sonnet-model`, `--haiku-model`, `--oversize`, `--route-policy`). Set the
tiers once with
`oaica config set sonnet-model <model>` / `oaica config set haiku-model
<model>` and every later `oaica launch claude` uses them (a flag or a plan
still wins; `oaica config show` lists them, `oaica config set <key> -` clears
one). `weighted` splits HEALTHY
traffic across legs by weight
(session-sticky consistent hash) instead of only failing over — set
weights via `remotes.json`'s `"weight"` or the repeatable
`--shard <model>:<weight>` flag. Details: docs/CLAUDE_TIERS.md.

## Self-host quick start (free, offline)

```shell
oaica pull qwen2.5-0.5b        # or oaica-nemotron-30b-a3b (25 GB Q4_K_M GGUF)
oaica serve qwen2.5-0.5b       # runs llama.cpp's llama-server, prints an OpenAI-compatible URL
```

Requires `llama-server` on your `PATH` (or set `OAICA_LLAMA_SERVER=/path/to/llama-server`)
— see [docs/LOCAL_USE.md](docs/LOCAL_USE.md) for install instructions per OS.
Weights land in `~/.oaica/models/<model>.gguf`. Full walkthrough, flags, and
troubleshooting: [docs/LOCAL_USE.md](docs/LOCAL_USE.md).

## Bring your own provider

```shell
oaica remote add mine --base-url https://your-endpoint/v1 --api-key-env MY_API_KEY
oaica remote list
```

Remotes are stored in `~/.oaica/remotes.json`. Built-in providers include
`ollama-cloud` (`OLLAMA_API_KEY`), `zai`, `deepseek`, and `opencode-go`.

## Command overview

| Command | What it does |
|---|---|
| `oaica run MODEL "prompt"` | Chat with a hosted or local model |
| `oaica launch [claude\|codex\|...]` | Launch a coding agent / integration wired to a model |
| `oaica pull MODEL` | Download a model's GGUF for self-hosting |
| `oaica serve MODEL` | Serve a pulled GGUF locally with `llama-server` |
| `oaica remote add\|list\|show\|rm` | Manage user-defined OpenAI-compatible endpoints |
| `oaica model add\|list\|show\|rm\|refresh\|alias` | Manage the local model manifest (context window, engine, launch flags) |
| `oaica plan set\|list\|show\|rm` | Named tier plans (e.g. plan on one model, execute on another) |
| `oaica config show\|set` | Standing launch tiers: `sonnet_model`, `haiku_model` |
| `oaica signin` / `oaica signout` | Save or remove your OAICA API key |
| `oaica site new\|edit\|preview\|deploy` | Optional static site builder |
| `oaica gpu ps\|clean` | Inspect / clean up local GPU-memory-holding processes |
| `oaica agent [PROMPT]` | Run a streaming coding agent |
| `oaica doctor` | Read-only check of launch routing: remote reachability, route policies, daemon leg (exit 1 on a failed remote probe). Add `--report` for a redacted support bundle (version, platform, config paths and permissions, which credentials are set) — safe to paste into a ticket |
| `oaica usage` | Summarize this machine's launch traffic (`~/.oaica/requests.log`): requests, errors, routing per model/backend |
| `oaica auth login\|list\|logout` | Store model-provider credentials in `~/.oaica/auth.json` |
| `oaica provider login\|list\|logout` | Same `~/.oaica/auth.json` store as `oaica auth` (hidden alias) |
| `oaica router login\|list\|logout` | Manage provider backends on the api.oaica.com router (requires `OAICA_ADMIN_KEY`) |
| `oaica claude-login` | Sign in to the real Claude Code on your own Anthropic account, bypassing OAICA entirely |

`oaica list`, `ps`, `rm`, `show`, `cp`, `create`, and `push` are upstream
Ollama daemon commands, kept for compatibility — they only work if you
separately run an Ollama daemon and set `OLLAMA_HOST`; otherwise they print
a hint. Run `oaica --help` or `oaica <command> --help` for full flag
reference on any command.

## Configuration & files

Everything lives under `~/.oaica/` (created owner-only, mode 0700):

| Path | Contents |
|---|---|
| `~/.oaica/api_key` | Saved OAICA API key (`oaica signin`) |
| `~/.oaica/auth.json` | Stored model-provider credentials (`oaica auth`) |
| `~/.oaica/aliases.json` | User-defined model-name shortcuts (`oaica model alias`) |
| `~/.oaica/config.json` | Standing launch tiers — `sonnet_model`, `haiku_model` (`oaica config`) |
| `~/.oaica/license_key` | Saved license key |
| `~/.oaica/license.json` | Activation state for a purchased license (`oaica activate`); distinct from `license_key` |
| `~/.oaica/local_servers.json` | Runtime state of running `oaica serve` instances (rewritten on every start/stop) |
| `~/.oaica/models.json` | Local model manifest (`oaica model`) |
| `~/.oaica/plans.json` | Named tier plans (`oaica plan`) |
| `~/.oaica/remotes.json` | User-defined remotes (`oaica remote`) |
| `~/.oaica/model_picks.json` | Picker frequency/recency state (which models you actually pick) |
| `~/.oaica/picker_cache.json` | Cached picker inventory, so a launch doesn't re-probe everything |
| `~/.oaica/requests.log` | Local launch traffic log — model, backend label, sizes, status, never content (`oaica usage`) |
| `~/.oaica/cache/` | Cached probe answers: `providers/`, `models/`, `cloud_limits/` |
| `~/.oaica/models/` | Downloaded GGUF weights (`oaica pull`) |
| `~/.oaica/update_check.json` | Update-check state |

Environment variables:

| Variable | Purpose |
|---|---|
| `OAICA_API_KEY` | Hosted API key (overrides the saved one) |
| `OAICA_ADMIN_KEY` | Operator admin key — `oaica router`'s provider-registry commands require it, and `OAICA_API_KEY` will not do ("auth commands need the operator admin key"); `oaica auth` writes the local store and needs no admin key |
| `OAICA_LICENSE_KEY` | License key for gated models |
| `OAICA_HOST` | Override the hosted API base URL |
| `OAICA_NO_UPDATE_CHECK` | Set to disable the update-check notice |
| `OAICA_LLAMA_SERVER` | Path to `llama-server` if it's not on `PATH` |
| `OAICA_MODELS_DIR` | Override where pulled GGUFs are stored (default `~/.oaica/models`) |
| `OAICA_REMOTES_FILE` | Override the remotes file path |
| `OLLAMA_HOST` | Address of a separately-running Ollama daemon, for the upstream daemon-only commands |

## Uninstall

```shell
sudo rm /usr/local/bin/oaica   # or: rm ~/.local/bin/oaica
```

Removing the binary is the whole uninstall. Your data stays in `~/.oaica` — the
API key, `config.json` (standing tiers), `plans.json` (named plans),
`remotes.json`, `models.json`, and every pulled GGUF under `models/`. Nothing
here needs to be deleted to uninstall, and no command in this file deletes it
for you. If you do want a clean slate later, know what is at stake first: the
whole directory holds your keys, plans, remotes and downloaded model weights,
so deleting any part of it is meant to be a deliberate act, not a step in
uninstalling. Prefer targeted removal over the directory — and never the
directory while model weights you still want are in it.

## Docs

- [docs/LOCAL_USE.md](docs/LOCAL_USE.md) — self-hosting in detail
- [docs/RELEASE.md](docs/RELEASE.md) — cutting a release
- [docs/SITE_BUILDER.md](docs/SITE_BUILDER.md) — the static site builder
- [docs/CLAUDE_TIERS.md](docs/CLAUDE_TIERS.md) — plan on one model, execute
  on another — any mix of remote, router, local
- [docs/MODELS_AND_PLANS.md](docs/MODELS_AND_PLANS.md) — add your own
  self-hosted model to the manifest, define a named `--plan`, GPU cleanup
- [docs/PRICING.md](docs/PRICING.md) — pricing
- [docs/OPENROUTER_PROVIDER.md](docs/OPENROUTER_PROVIDER.md) — how the API
  is served

## Upstream

`oaica` is a fork of [Ollama](https://ollama.com)'s client. Ollama's own
docs, model library, REST API, and ecosystem of community integrations
apply to the upstream `ollama` daemon, not to this fork — see
[docs/LOCAL_USE.md](docs/LOCAL_USE.md) for what self-hosting looks like in
oaica instead. Credit and thanks to the Ollama team and community.
