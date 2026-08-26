# Claude Code model tiers with `oaica launch claude`

Claude Code picks a model *tier* per request and resolves each tier through
an env var the launcher sets:

| Request | Tier | Env var |
|---|---|---|
| Plan mode (`/model opusplan`, `--model opus`) | Opus | `ANTHROPIC_DEFAULT_OPUS_MODEL` |
| Normal turns, execution, `--model sonnet` | Sonnet | `ANTHROPIC_DEFAULT_SONNET_MODEL` |
| Subagents | — | `CLAUDE_CODE_SUBAGENT_MODEL` |
| Quick background calls (titles, summaries) | Haiku | `ANTHROPIC_DEFAULT_HAIKU_MODEL` |

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

Then inside Claude Code: `/model opusplan` → plans on the primary, executes
on the `--sonnet-model`. `--model opus` / `--model sonnet` on the command
line force one tier for the whole session.

## Where a model name can come from

Resolution order (first match wins), `cmd/launch/tier_routing.go`:

| Name | Source | Endpoint used |
|---|---|---|
| `<remote>/<id>`, or a bare id exactly one remote serves | `~/.oaica/remotes.json` | the remote's `base_url` + key |
| `<model>:local` | a running `oaica serve` | its origin, no key |
| id listed by the OAICA router (`OAICA_HOST`, default api.oaica.com) | router | `<host>/v1` + `OAICA_API_KEY` |
| anything the local Ollama daemon answers `/api/show` for (pulled or `:cloud`) | daemon (`OLLAMA_HOST`) | `<daemon>/v1` |

Primary and `--sonnet-model` resolve independently, so cross-source splits
are fine. A name found nowhere fails before anything starts, naming every
place that was tried.

## Why this exists

- Before 2026-08-26 `--sonnet-model` had to be on the **same** remote as the
  primary (one proxy = one base URL + key).
- Router and daemon models bypassed the translation proxy: Claude Code was
  pointed straight at the host and expected it to speak `/v1/messages`. The
  public gateway only speaks `/v1/chat/completions`, so a fresh install's
  `launch claude --model kat-awq` died with `unrecognized_model`. Now every
  source goes through the proxy.

## Notes

- Tool calling is gated per endpoint (`--force-tools` downgrades refusal to
  a warning).
- The proxy writes a local-only request log (`~/.oaica/requests.log`:
  model, backend label, sizes, status — never content). The backend column
  shows which route each request took: `daemon:ollama …`, `remote:kat-awq …`,
  `router:oaica …`, `local-serve:local …`.
- `ANTHROPIC_AUTH_TOKEN` is only what Claude Code presents to the local
  proxy; each route's real key is attached upstream by the proxy.
- Ollama cloud models (`…:cloud`) need `ollama signin` on the daemon.
