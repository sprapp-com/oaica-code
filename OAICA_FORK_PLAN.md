# oaica-code — Ollama fork plan

> Cloned from github.com/ollama/ollama (shallow, `main`) into this dir per
> user request 2026-07-31: "modify ../oaica-code as OAICA code from Ollama
> and use as our own 'claude code' that interfaces with our own API of
> oaica.com and then serve them using the i-compact versions of KAT-Coder /
> OAICA Malay 35B." Scope confirmed: **full fork** (keep GUI app, model
> mgmt, server — not just a stripped-down CLI).

## What Ollama actually is (grounding, so the plan isn't guesswork)

- `cmd/` — the `ollama` CLI (run/pull/push/list/serve/create, 2591-line
  `cmd.go`).
- `api/` — Go client library the CLI + app use to talk to a local (or
  remote) Ollama server over HTTP (`api/client.go`, default host
  `127.0.0.1:11434`, env override `OLLAMA_HOST`).
- `server/` — the actual HTTP server (model loading, `/api/generate`,
  `/api/chat`, OpenAI-compat `/v1/*` in `openai/`).
- `llm/`, `llama/`, `ml/` — Ollama's own bundled llama.cpp-derived
  inference runtime + GGUF loading.
- `app/` — desktop GUI (systray app, Electron-ish wrapper).
- Cloud/auth: `ollama.com` hardcoded in a few places (`api/client.go:119`,
  `184`; `cmd/cmd.go` sign-in prompts) for Ollama's hosted "Cloud models"
  feature — this is the piece that needs repointing to oaica.com.

## Key decision: which serving backend does oaica-code actually talk to?

Ollama's own `server/`+`llm/`+`ml/` stack is a **separate, parallel
inference engine** to prism-engine/llama.cpp (what we already run
KAT-Coder/Malay-35B on, bitdeer H200 GPU0 port 8501/8502). Two real
options, not a false choice — pick before touching code:

1. **Keep Ollama's own server, point it at our GGUFs.** Ollama's `server/`
   already loads GGUF via its own llama.cpp fork — in principle it could
   load `kat-coder-i-compact.gguf` directly via `ollama create` + a
   Modelfile, no need to touch inference internals. Fork work = rebrand +
   swap the model registry/pull mechanism (Ollama pulls from
   `registry.ollama.ai`; we'd need it to pull from an oaica.com-hosted
   registry or just accept local GGUF paths, which Ollama already supports
   via `ollama create -f Modelfile` with `FROM /path/to.gguf`).
2. **Strip Ollama's server entirely, make the CLI a thin client to our
   existing OpenAI-compatible API** (`api.sprapp.com` / oaica.com,
   `/v1/chat/completions` — the router Worker built earlier this session).
   Fork work = gut `server/`+`llm/`+`ml/`, repoint `api/client.go`'s
   default host to oaica.com, keep `cmd/` CLI UX (the actual "feel" of the
   tool) but make every command a remote call instead of local inference.

**Recommendation: option 2.** We already have a working serving stack
(prism-engine/llama.cpp on bitdeer + a100b, `api.sprapp.com` router). Option
1 means running a SECOND inference engine (Ollama's own) that duplicates
what prism-engine already does — double the maintenance, double the GPU
memory management surface, no benefit. Option 2 treats Ollama purely as a
**CLI/UX skeleton** (command parsing, terminal chat rendering, model-list
UI, the desktop app shell) and makes oaica.com's existing API the only
brain. This matches "interfaces with our own API" in the original ask
literally.

## Concrete rebrand + repoint work (option 2)

1. **`api/client.go`**: change default host from `127.0.0.1:11434` to
   `https://api.oaica.com` (or whichever prod hostname — confirm with user;
   `api.sprapp.com` exists today, `oaica.com` was named in the ask, may be
   the same thing under a product-facing domain, needs the user's DNS/brand
   call). Change `OLLAMA_HOST` env var to `OAICA_HOST` (keep back-compat
   alias if desired). Replace the `ollama.com` hostname checks
   (`c.base.Hostname() == "ollama.com"`) with `oaica.com`.
2. **Strip `server/`, `llm/`, `ml/`, `discover/` (GPU discovery for local
   inference), `runner/`** — these exist to run models locally; not needed
   if oaica.com does all inference. `cmd/cmd.go`'s `serve` subcommand
   either gets removed or repurposed to mean "start a local reverse-proxy
   to oaica.com" if offline/cached behavior is ever wanted (not in scope
   now).
3. **Model list command** (`ollama list`, `ollama pull`) — repoint to hit
   oaica.com's `/v1/models` instead of Ollama's local blob manifest store.
   Model names become `kat-coder-i-compact`, `oaica-malay-35b-i-compact`
   (once its vocab bug is fixed) instead of Ollama's `library/llama3:8b`
   style tags.
4. **Rebrand strings**: `ollama` → `oaica` throughout `cmd/cmd.go`'s
   `Use: "ollama"` root command, help text, `cmd/cmd.go:2257`. Binary name,
   `main.go`, README, LICENSE attribution (Ollama is MIT-licensed — keep
   the license file, add NOTICE crediting upstream per MIT terms).
5. **`app/` (desktop GUI)** — lower priority; the CLI is the primary "our
   own claude code" surface per the ask. Rebrand pass only if/when the
   desktop app is actually wanted; don't block CLI work on it.
6. **Auth**: Ollama's `ollama.com` sign-in flow (`cmd/cmd.go` sign-in
   prompts, `UseAuth()` in envconfig) — replace with oaica.com's own
   license-key/API-key auth. We already have `license.sprapp.com` (D1-backed
   key issue/verify/revoke) — reuse it rather than building parallel auth.

## Explicitly NOT done yet (this session)

- No code changes made — this file is scoping only. The clone itself
  (`git clone --depth 1 https://github.com/ollama/ollama.git oaica-code`)
  is the only action taken.
- Serving backend decision (option 1 vs 2) needs explicit user confirmation
  before real edits start — this doc argues for option 2 but doesn't assume
  it's final.
- Exact prod hostname (`api.oaica.com` vs `api.sprapp.com` vs something
  else) needs the user's call — not guessed.
- OAICA Malay 35B i-compact isn't servable yet (source gguf vocab_size
  metadata bug, confirmed pre-existing, needs a fix at the source before
  this fork can list it as an available model) — see project memory
  `project_prism_ffi_matchbeat_gate_2026-07-31`-adjacent session notes /
  ask the user for the fix-source-or-skip decision.

## Suggested next step

Confirm option 2 (thin-client rebrand) vs option 1 (dual inference
engines) with the user, then start with the smallest end-to-end slice:
rebrand `cmd/cmd.go`'s root command + repoint `api/client.go`'s default
host + get `oaica run kat-coder-i-compact` working end-to-end against the
already-live bitdeer port 8501 server, before touching auth/app/model-list
polish.
