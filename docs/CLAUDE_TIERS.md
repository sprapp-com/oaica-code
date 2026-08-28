# Claude Code model tiers with `oaica launch claude`

Claude Code picks a model *tier* per request and resolves each tier through
an env var the launcher sets:

| Request | Tier | Env var |
|---|---|---|
| Plan mode under `/model opusplan`; `/model opus`; `--model opus` | Opus | `ANTHROPIC_DEFAULT_OPUS_MODEL` |
| Execution under `/model opusplan`; `/model sonnet`; `--model sonnet` | Sonnet | `ANTHROPIC_DEFAULT_SONNET_MODEL` |
| Subagents | — | `CLAUDE_CODE_SUBAGENT_MODEL` (= the Sonnet model) |
| Quick background calls (titles, summaries) | Haiku | `ANTHROPIC_DEFAULT_HAIKU_MODEL` (= the primary) |

The launcher always passes `--model <primary>` to Claude Code, so **with a
plain launch every main-conversation request goes to the primary**. The
`--sonnet-model` backend is reached three ways: `/model opusplan` (plans on
the primary, executes on the secondary), `/model sonnet` / `--model sonnet`
(main conversation on the secondary), and subagents (always the secondary).
Haiku-tier background calls stay on the primary in every mode. `--model
opus` / `--model sonnet` pin the *main conversation* only; subagents and
Haiku calls keep their env-var tiers.

Every request carries the resolved model id. The launcher runs ONE local
Anthropic→OpenAI translation proxy with a **routing table keyed by that id**,
so different tiers can go to different backends.

```bash
# one model everywhere (default)
oaica launch claude --model kat-awq

# plan with a cloud model, execute locally — any mix of sources
oaica launch claude --model deepseek-v4-flash:0731-cloud -- --sonnet-model kat-awq
oaica launch claude --model openrouter/anthropic/claude-sonnet-4 -- --sonnet-model bonsai:local
```

## Where a model name can come from

Resolution order for the **primary** (first match wins), `cmd/launch/tier_routing.go`:

| Name | Source | Endpoint used |
|---|---|---|
| `<remote>/<id>`, or a bare id exactly one remote serves | `~/.oaica/remotes.json` | the remote's `base_url` + key |
| `<model>:local` | a running `oaica serve` | its origin (+ its `--api-key` if it has one) |
| `router/<id>` or `oaica/<id>` — or a bare id the OAICA router lists (`OAICA_HOST`, default api.oaica.com); `<id>+<lora>[+…]` composites resolve by `<id>` and are sent upstream whole | router | `<host>/v1` + `OAICA_API_KEY` |
| `ollama/<id>` or `daemon/<id>` — or anything the local Ollama daemon answers `POST /api/show` for (pulled models **and** `:cloud` aliases) | daemon (`OLLAMA_HOST`) | `<daemon>/v1` |

A bare id that several remotes serve is refused with a hint to write
`<remote>/<id>`. A name found nowhere fails before anything starts, naming
every place that was tried (and the fix when the router rejected the key).

## `--sonnet-model` resolution

- Primary on a **user remote**: an un-namespaced secondary means *on that
  same remote* — the id is passed through even if the remote's `/models`
  does not enumerate it (`muse-spark-1.2` on opencode-go, `openai/gpt-5` on
  an OpenRouter remote). A bare id that other remotes or the router also
  serve is never silently rerouted. To leave the primary's remote, be
  explicit: `<remote>/<id>`, `<model>:local`, `router/<id>`, `ollama/<id>`.
- Primary on the router / daemon / `oaica serve`: the secondary resolves
  with the primary table above.

## Why this exists

- Before 2026-08-26 `--sonnet-model` had to be on the **same** remote as the
  primary (one proxy = one base URL + key).
- Router and daemon models bypassed the translation proxy: Claude Code was
  pointed straight at the host and expected it to speak `/v1/messages`. The
  public gateway only speaks `/v1/chat/completions`, so a fresh install's
  `launch claude --model kat-awq` died with `unrecognized_model`. Now every
  source goes through the proxy.

## Security notes

- The proxy listens on 127.0.0.1 but loopback is shared with every process
  and user on the machine, so it requires a **per-launch random token**
  (`Authorization: Bearer` / `x-api-key`). Claude Code receives that token
  as `ANTHROPIC_AUTH_TOKEN`; the real upstream keys live only inside the
  proxy and never enter the child environment.
- 2026-08-28 audit: every bind site for this proxy (`ListenAnthropicOpenAIProxy`,
  `ServeAnthropicProxyForRemote`) hardcodes `127.0.0.1` — there is no flag or
  parameter anywhere in this codebase that can bind it to a non-loopback
  address. Unlike `oaica serve` (which CAN bind `0.0.0.0` and is gated by
  `--api-key`/`--insecure`), this proxy structurally cannot be exposed to the
  network without a code change first.
- The proxy writes a local-only request log (`~/.oaica/requests.log`:
  model, backend label + redacted URL, sizes, status — never content).
  Backend labels: `daemon:ollama …`, `remote:<name> …`, `router:oaica …`,
  `local-serve:local …`.
- Tool calling is gated per endpoint (`--force-tools` downgrades refusal to
  a warning).
- Upstream streaming is bounded only by connection setup and by Claude Code
  disconnecting — a slow local model may stream as long as it needs.
- Ollama cloud models (`…:cloud`) need `ollama signin` on the daemon.
