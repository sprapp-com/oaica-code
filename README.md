# oaica

A thin, CGO-free terminal AI CLI (fork of Ollama's client) backed by our
own hosted API — no local GPU or model download required. It also ships an
optional static site builder (`oaica site new|edit|preview|deploy`).

**Install**

```shell
curl -fsSL https://oaica.com/install.sh | bash    # macOS/Linux
irm https://oaica.com/install.ps1 | iex           # Windows
```

Prefer a manual download? Grab a tarball from
[oaica.com/download](https://oaica.com/download/) or the
[GitHub releases](https://github.com/sprapp-com/oaica-code/releases). The
archive extracts to `bin/oaica`, so either extract into `~/.local`
(`tar -C ~/.local -xzf oaica-*.tar.gz`) or move `bin/oaica` onto your `PATH`
yourself (e.g. `/usr/local/bin`).

**Current release:** 0.4.8 — see https://oaica.com/download/VERSION.txt for
the live pointer.

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
oaica run oaica-35b-a3b-vision "hello"
oaica launch claude --model oaica-35b-a3b-vision    # run Claude Code against it (installs claude with --yes)
```

Hosted models: `oaica-35b-a3b-vision` (262k context, vision, MTP) and
`oaica-nemotron-30b-a3b` (262k context, reasoning + tools).

Multi-model launches (v0.5.0+): a plain interactive
`oaica launch claude` walks a wizard — primary, then Sonnet/execution
tier, then a compaction/oversize model (only models with a probed LARGER
context window are offered), then a route
policy (`--route-policy local-first|remote-first|auto|local-only|remote-only`)
with cross-leg failover via a health circuit breaker — and can save the
whole setup as a named plan. Same knobs exist as flags
(`--sonnet-model`, `--oversize`, `--route-policy`) and are validated by
`oaica doctor`. Details: docs/CLAUDE_TIERS.md.

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
`ollama-cloud` (`OLLAMA_API_KEY`), `z.ai`, `deepseek`, and `opencode-go`.

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
| `oaica signin` / `oaica signout` | Save or remove your OAICA API key |
| `oaica site new\|edit\|preview\|deploy` | Optional static site builder |
| `oaica gpu ps\|clean` | Inspect / clean up local GPU-memory-holding processes |

`oaica list`, `ps`, `rm`, `show`, `cp`, `create`, and `push` are upstream
Ollama daemon commands, kept for compatibility — they only work if you
separately run an Ollama daemon and set `OLLAMA_HOST`; otherwise they print
a hint. Run `oaica --help` or `oaica <command> --help` for full flag
reference on any command.

## Configuration & files

Everything lives under `~/.oaica/`:

| Path | Contents |
|---|---|
| `~/.oaica/api_key` | Saved OAICA API key (`oaica signin`) |
| `~/.oaica/license_key` | Saved license key |
| `~/.oaica/models.json` | Local model manifest (`oaica model`) |
| `~/.oaica/plans.json` | Named tier plans (`oaica plan`) |
| `~/.oaica/remotes.json` | User-defined remotes (`oaica remote`) |
| `~/.oaica/models/` | Downloaded GGUF weights (`oaica pull`) |
| `~/.oaica/update_check.json` | Update-check state |

Environment variables:

| Variable | Purpose |
|---|---|
| `OAICA_API_KEY` | Hosted API key (overrides the saved one) |
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
rm -rf ~/.oaica
```

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
