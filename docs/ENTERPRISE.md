# oaica in an enterprise environment

This document is for the person who has to approve or deploy `oaica`: security
review, platform team, procurement. It states what the client does with data,
what it writes to disk, where it connects, and what has *not* been tested — so
you can decide from facts rather than from the README's enthusiasm.

It describes the software as it is today. Where something is unverified, it
says so instead of implying coverage.

## What it is

A terminal CLI. One statically linked Go binary with no CGO, no daemon, no
local model weights required, no accounts created by the installer, and no
background service registered. Uninstalling is removing the binary (see
[Uninstall](#uninstall)).

It is a fork of [Ollama](https://github.com/ollama/ollama)'s client, MIT
licensed. See [Licensing and fork status](#licensing-and-fork-status) before
you treat upstream's documentation, hardening claims or support channels as
describing this binary.

## Network connections

Ten paths the client itself opens. Nothing else in the client opens a socket.
(One more exists only through a third-party tool you run deliberately: see
row 9.)

| # | Destination | When | What is sent | Off switch |
|---|---|---|---|---|
| 1 | Your model endpoint | Every model call | Your prompts, attachments and tool output — the traffic you asked for | Choose the endpoint (`OAICA_HOST`, a remote, or a local `oaica serve`) |
| 2 | `oaica.com` | Install and upgrade only | Nothing; it serves the install scripts | Install from the GitHub release instead |
| 3 | `github.com/sprapp-com/oaica-code/releases/latest/download/VERSION.txt` | At most once per 20h, from any command that would print the update notice | A plain GET for the release's version file; **sends no body, no identifiers**. The request reveals your IP and that oaica is installed | `OAICA_NO_UPDATE_CHECK=1` |
| 4 | `raw.githubusercontent.com` | Only when you run `oaica remote sync`, `oaica model sync` or `oaica model cloud-limits sync` **without** `--url` | A GET for that catalog's JSON in this repository | Pass `--url` (a `file://` path works) and the default is never contacted; or don't run the sync commands |
| 5 | Agent installers at the vendor's own host — `claude.ai`, `code.kimi.com`, `dev.meta.ai`, `hermes-agent.nousresearch.com`, `opencode.ai`, `qwen-code-assets.oss-cn-hangzhou.aliyuncs.com` | Only when you run `oaica launch <agent>` for an agent that is not installed and you confirm the install | A GET for the vendor's own installer script, which it then runs | Don't launch that agent; install it yourself first, or from your own mirror |
| 6 | `registry.npmjs.org` | Only when you run `oaica launch <agent>` for an agent that ships on npm — `pi`, `omp`, `cline`, `openclaw`, `dsh`, `opencode` — and you confirm the install | A GET for the package's version metadata, then the `npm install -g` it runs (`@earendil-works/pi-coding-agent@latest`, `@ollama/pi-web-search`, `cline@latest`, `openclaw@latest`, `@deepseek-ai/dsh@latest`). `pi` additionally checks its web-search package version, and `omp` installs the same web-search plugin through `omp plugin install` | Decline the prompt, or install the agent yourself first, or from your own npm mirror — none of these installers run unprompted. `PI_OFFLINE=1` additionally skips `pi`'s web-search version check |
| 7 | `api.lemonsqueezy.com` (`/v1/licenses`) | `oaica activate <key>` once, then a revalidation on `oaica launch` when the stored activation is older than 7 days. A key supplied as `OAICA_LICENSE_KEY` instead is validated on every launch (no stored activation to age out, and nothing is written for it) — the env var is the documented alternative to `oaica activate`, for a deployment that injects the licence from a secret manager | The licence key and this machine's activation id; the response says whether the licence is still valid | No switch for a purchased key while the licence gate is in force; a cached activation younger than 7 days makes no call, and an unreachable licence server keeps working for 30 days. **One published key bypasses the gate and this call entirely:** the string `OAICA-TEST-DEV-FREE` is compiled into the binary (`cmd/launch/license.go`), activates locally, and never revalidates. It is in the public source, it is not a secret, and any build-from-source can remove the gate anyway — the licence is a convenience paywall on the prebuilt binary, not a control that resists a determined user. Do not treat it as one when threat-modelling |
| 8 | `huggingface.co`, or the router's own storage | Only when you run `oaica pull <model>` | `GET /v1/manifest/<model>` on your endpoint, then the weight bytes from the URL that manifest names — the router's storage, or HuggingFace when the manifest says `source=hf`. On a HuggingFace URL, and only there, the request carries your HuggingFace token as a bearer header if one is present (`HF_TOKEN`, else `~/.huggingface/token`) — a speed-up for a public repo, not a requirement; a URL naming any other host is fetched anonymously, so a manifest cannot direct that token somewhere else | `oaica pull` is optional: point `OAICA_HOST` at a model already on disk and pull nothing. Unset `HF_TOKEN` and move `~/.huggingface/token` aside and the download still works, unauthenticated |
| 9 | `api.cloudflare.com` (and `*.pages.dev`), **via the `wrangler` CLI** | Only when you run `oaica site deploy DIR` and `wrangler` is installed and authenticated | Your site's build output, uploaded by `wrangler pages deploy` under your own Cloudflare account. The socket is opened by `wrangler`, not by this binary — oaica shells out rather than reimplementing the Pages API | Don't run `oaica site deploy`; the site builder is optional and `oaica site new|edit|preview` stay entirely local |
| 10 | `ollama.com` (Ollama's own cloud — a different host from `oaica.com` in row 2) | Two paths: the picker inventory scrapes `https://ollama.com/search?c=cloud` on a cache miss, to list the `:cloud` catalogue (also on `oaica model refresh`); and `oaica launch claude-desktop` validates the Ollama API key you give it | The scrape is an unauthenticated GET of a public search page — no credential, no payload, the same request your browser makes. The key check is `GET https://ollama.com/v1/models` carrying your `OLLAMA_API_KEY` as a bearer header; it is the only path in this table that sends a credential to a host other than your own endpoint | The scrape is skipped entirely when `OAICA_HOST` is set, and `~/.oaica/cache/models/ollama-cloud.json` serves it within its TTL otherwise (so it is not a per-launch request); skip `oaica launch claude-desktop`, or launch a model through your own endpoint, and no key reaches `ollama.com` |
| 11 | Each built-in catalogue provider's own API host — `api.deepseek.com`, `api.z.ai`, `api.minimax.io`, `api.minimax.cn`, `openrouter.ai`, `opencode.ai`, … The set is exactly the `base_url`s in `cmd/launch/providers/providers.json` and grows as that catalogue does | Whenever the model list is built live: `oaica model refresh`, and any `oaica launch` / `oaica model` invocation that re-probes its sources (a picker cache hit makes none of these calls) — **and only for a provider you hold a credential for**: an env var, an `oaica auth login` entry, or a login another agent CLI already stores | `GET <that provider's base>/models` to list the models it serves, carrying that provider's own credential as its bearer header. A model-list call: no prompt, no attachment, no tool output. The same commands also call `GET /v1/models` on your endpoint (row 1's host, `https://api.oaica.com` by default) to list the router's catalogue | Hold no credential for a provider and its host is never contacted — `oaica auth logout <provider>` (or unset its env var) removes the row entirely, and a remotes.json you wrote yourself is all that is probed otherwise. Running `oaica model refresh` is the way to make these calls on purpose |

The endpoint in row 1 defaults to `https://api.oaica.com` (an
OpenAI-compatible router) and is where the work actually goes. If prompts may
not leave your network, point `oaica` at an endpoint inside it — a remote in
`~/.oaica/remotes.json`, or `oaica pull` + `oaica serve` for a fully local
model — and confirm with `oaica doctor`, which lists every configured remote
with the route policy it would default a launch to, and probes each one live
(the resolved policy still depends on the model and on any `--route-policy`
flag, so doctor narrows the candidates rather than naming one).

Row 5 exists because `oaica launch claude` (and `codex`, `kimi`, `hermes`,
`opencode`, `dsh`, `pi`, …) will offer to install that agent from its
publisher when it is missing, after a prompt that says so. Those agents are not
part of this repository, are not pinned or audited by it, and once installed
they keep their own update and login behaviour — including talking to their own
vendors' backends if you sign in to them natively. A few of them (`pi`, `cline`,
`openclaw`, `dsh`) install from npm rather than from a publisher script, which
is row 6. In a controlled environment,
install the agents from your own package mirror and let `oaica launch` find them
already on `PATH`. Catalog metadata can be synced from an internal mirror
instead of GitHub: `oaica remote sync --url`, `oaica model sync --url` and
`oaica model cloud-limits sync --url` all accept `file://` paths and plain
HTTP URLs (row 4).

### Telemetry

There is none. No analytics, no crash reporting, no usage beacons, no
"anonymous" install ID. Grep the tree for `telemetry`, `analytics`, `sentry`,
`posthog`, `segment` and you will find the TUI's own render helpers and
nothing else. Three outbound requests happen without you asking, and none of
them carries a payload: row 3's version GET; row 7's licence revalidation for
an install that is already activated (at most once per 7 days, and it sends
only the key and activation id you already stored); and row 10's ollama.com
scrape of a public search page, which sends no credential at all. Every other
non-model path is behind a prompt you answer, or behind a command you typed
(`oaica launch <agent>` is the one to know about: it asks before installing an
agent or its packages, and rows 5 and 6 list the hosts that touches).

## Files on disk

All state lives under `~/.oaica/` (created mode `0700`). Individual files are
written `0600`; nothing is world-readable, and `oaica doctor --report` reports
the modes it actually finds so you can verify that on a given host.

| Path | Contents | Secret? |
|---|---|---|
| `~/.oaica/api_key` | Saved oaica API key (`oaica signin`) | **Yes** |
| `~/.oaica/auth.json` | Stored model-provider credentials (`oaica auth`) | **Yes** |
| `~/.oaica/remotes.json` | Your remotes: base URLs, and any inline `api_key` | **Possibly** |
| `~/.oaica/config.json`, `plans.json`, `models.json`, `aliases.json` | Tiers, named plans, model manifest, aliases | No |
| `~/.oaica/model_picks.json`, `picker_cache.json` | Picker frequency and cached inventory | No (no URLs or keys — the cached row is name/metadata only) |
| `~/.oaica/local_servers.json` | Which `oaica serve` instances are running, their ports, and the `--api-key` each was started with | **Possibly** |
| `~/.oaica/requests.log` | One line per launch request **that this client routed and metered**: model name, which backend served it (a label, or the endpoint URL with any credential redacted), message *sizes*, timing, status | No — sizes, not content |
| `~/.oaica/cache/` | Cached catalog and probe answers | No |
| `~/.oaica/update_check.json` | Last update check: when, and which version was newest | No |
| `~/.oaica/models/` | Downloaded GGUF weights | No |
| `~/.oaica/license_key`, `license.json` | Purchased-license state | **Yes** (a licence key, not an API key) |

`requests.log` is worth calling out because it is the one file a network
security team will ask about: it records the byte length of the last message
and of the whole conversation, plus a boolean "would the router have called this
hard", and never message text, headers, or credentials. `oaica usage` summarises
it. Delete the file at any time; nothing else depends on it.

It is also not a complete accounting of your traffic, and should not be read as
one: a leg oaica does not route or meter writes no row. A remote that speaks
the Anthropic wire natively (the `zai-coding-plan`, `minimax-coding-plan` and
`minimax-cn-coding-plan` rows, a raw `api.anthropic.com` entry, or
`oaica launch claude-login`) is passed through end to end, so its requests
appear in neither `requests.log` nor `oaica usage`. Absence of a row is not
evidence that no request was made. What such a leg *does* print — the request
and response themselves — is the same traffic you asked for; it simply never
touches oaica's own ledger.

Integrations write outside `~/.oaica/` too, in their own config locations —
for example a Codex profile, or the launching agent's settings file — when you
launch them through oaica. Those are the agent's files, in the agent's format.

### Credentials

Resolution order, highest first: the environment variable a remote names
(`api_key_env`), the `oaica auth login` store, another agent CLI's credential
store when the remote declares `auth_via`, then an inline `api_key`.

A credential must not reach a log, a cache, a support bundle, or a process
argument list. Concretely, the client:

- never writes an API key into `requests.log`, the picker cache, the catalog
  caches, or `doctor` output;
- prints a credential embedded in a URL as `https://REDACTED@host/v1` — the
  host stays readable so you can still tell *where* a credential is
  configured, which is often what you need in a ticket;
- refuses to print `oaica doctor --report` at all if any secret value would
  appear in it, rather than printing a partially-redacted bundle. The scan
  covers every place a key can sit — the environment (`OAICA_API_KEY`,
  `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `HF_TOKEN`), `remotes.json`,
  `auth.json`, the `api_key`, `license_key` and `license.json` files, the
  `--api-key` values in `local_servers.json`, and `~/.huggingface/token` — so
  a value the report does not currently print is still checked against it;
- promotes a bare `https://<token>@host/v1` remote into a normal bearer token
  and strips it from the URL, so the secret stops travelling in URLs, in
  command lines visible to `ps`, and in `net/http` error text.

A `https://user:password@host/v1` remote is deliberately left intact, because
that is a real Basic-auth credential and rewriting it would change how the
request authenticates; it is still redacted wherever it is printed. The same
applies to an `OAICA_HOST` that carries a credential: the token is promoted to
the bearer and stripped from the URL, so it does not appear in error text, in
`ps`, or in any message naming the host.

`oaica launch kimi` hands the provider key to Kimi Code CLI through the
`KIMI_MODEL_*` environment variables Moonshot documents for that purpose, so it
stays out of the child's argument list. Codex, Claude Code, opencode and the
rest receive their credentials the same way, through the environment or a
config file.

One known exception, stated rather than implied: if `kimi` on `PATH` is the
**archived** Python `kimi-cli` — recognisable by `--config-file` in its `kimi
--help` — it has no environment channel, and oaica falls back to its
`--config <json>` argument, which contains the provider key. That key is then
in the child process's argument list, readable from `/proc/<pid>/cmdline` by any
local user. Install Kimi Code CLI instead (which is what `oaica launch kimi`
offers, from `code.kimi.com/kimi-code/…`), or don't launch Kimi on a shared
host.

## Installing, pinning, air-gapped

Installers are the two files in the release's assets
(`install.sh`, `install.ps1`). Install from the release, which is the artifact
CI built from the tag:

```shell
curl -fsSL https://github.com/sprapp-com/oaica-code/releases/latest/download/install.sh | bash
```

`https://oaica.com/install.sh` serves a published copy of the same script,
refreshed by a manual Cloudflare Pages deploy — it can lag a release, so prefer
the release URL above.

One caveat on that URL, because it is not obvious from the script itself: an
`install.sh` **asset published before 2026-09-26** fetches its archive from
`https://oaica.com/download`, the hand-maintained copy, rather than from the
release it came in. Installing from such a release gets you whatever that copy
holds, which was 0.5.45 while the release tag said 0.5.46. The fix is in the
script as of the commit that switched it to the release URL; if you pin an
earlier release, or install without pinning and find `oaica --version` disagrees
with the tag, that is the reason, and the remedy is to build from source or
install a release whose asset postdates it.

What the installer does, in order: resolve the release for the version being
installed, download the archive **and that release's `SHA256SUMS`**, verify the
archive's SHA-256, extract `bin/oaica` into a temporary directory, then install
it as `/usr/local/bin/oaica` (mode `755`), using `sudo` when that directory is
not writable. The temporary directory is removed on exit via an `EXIT` trap.
It does not touch your shell profile, does not install a service, and writes
nothing outside the temp dir and the binary's destination. (It does *remove*
one thing: a `lib/oaica` directory under the install prefix, left by versions
of this script that unpacked upstream Ollama's server layout there. Nothing in
the client reads that path.)

Three things you should know rather than assume:

- **Pin the version in production — with the release's own installer.** The
  `curl …/releases/latest/download/install.sh | bash` line installs the *latest*
  release's `install.sh`, which then resolves `OAICA_VERSION` to that version's
  archive: `OAICA_VERSION=0.5.46` fetches `oaica-v0.5.46`'s archive and
  `SHA256SUMS`, and the binary reports `0.5.46`. An unpinned install is
  `releases/latest` at the moment you run it. Both spellings of the version are
  accepted (`0.5.46` and the tag `oaica-v0.5.46`). One caveat before you rely on
  the pin: **an `install.sh` published before 2026-09-26 ignores
  `OAICA_VERSION`'s release and fetches its archive from `oaica.com/download`**,
  the hand-maintained copy — `oaica-v0.5.46`'s asset does exactly this, so
  pinning to it yields the 0.5.45 binary. Pin against a release whose asset
  postdates that switch, and check `oaica --version` against the tag you asked
  for.
- **A mirror is supported first-class.** `OAICA_DOWNLOAD_BASE` points the
  installer at your own base URL, which wins over version resolution — that is
  the air-gapped path. Stage the archive and `SHA256SUMS` from a release into
  your artifact store and set this.
- **Verification can fail open, and says so.** If `sha256sum`/`shasum` is
  missing, or `SHA256SUMS` cannot be fetched, or it has no entry for the
  archive, the installer prints a warning and continues. A **mismatch** retries
  three times and then fails. In a high-assurance environment, verify the
  archive yourself before installing — the digests are all in the release.

Releases built by `.github/workflows/release.yaml` also carry a keyless Sigstore
attestation and a CycloneDX SBOM, which is what current `main` builds. The
releases published before this session's changes to that workflow do **not** —
`gh attestation verify` reports no attestations for `oaica-v0.5.46`, and it
ships no `.sbom.cdx.json` asset — so treat the verify command as a property of
the workflow (verify it against a release the workflow built) before relying on
it for a binary you already have:

```shell
gh attestation verify oaica-linux-amd64.tar.zst -R sprapp-com/oaica-code
```

Windows binaries are **not** Authenticode-signed — there is no code-signing
certificate yet. On Windows the checksum and the attestation are the integrity
signals, and if your policy requires a signed binary, this build does not meet
it today.

## Uninstall

Removing the binary is the whole uninstall. Your data stays in `~/.oaica/` —
the API key, plans, remotes, and every pulled model weight. Nothing in the
client deletes it for you, and no documented step in this repository asks you
to delete the directory: it holds credentials and multi-gigabyte weights, so
removing any part of it should be a deliberate act, not a step in an uninstall
procedure. Prefer targeted removal over the directory.

## Platform support, honestly

CI installs and runs the published release on Linux, macOS and Windows, and
every push to `main` builds and tests the Go packages. Beyond that:

- The smoke test proves installation and `oaica --version`. It does not
  exercise a model call, so it does not prove an endpoint inside your network
  is reachable from your hosts.
- Self-hosting (`oaica pull`, `oaica serve`) requires `llama-server` on the
  host; `oaica` does not ship it and does not build it.
- Windows is the least-used path in this codebase. Its installer behaviour has
  been exercised in CI, not by long production use.

## Licensing and fork status

MIT, a fork of Ollama's client, maintained by sprapp-com. Consequences that
matter for procurement:

- **This is not Ollama, and Ollama does not support it.** Upstream's docs,
  model library and release channels describe the upstream daemon, not this
  binary. Security reports go to the addresses in `SECURITY.md`, not to Ollama.
- **There is no support SLA, no SOC 2 report, no ISO certification, no
  penetration-test attestation, and no warranty.** MIT says so explicitly. If
  your process requires those, they do not exist for this project.
- **Only the latest release is supported.** Fixes ship as a new tag, never as a
  patch to an older one — which is why pinning and a mirror should be paired
  with a plan to move forward, not used to freeze indefinitely.
- **Model weights are not covered by this license.** `oaica pull` downloads
  GGUF files from model publishers, each with its own license; self-hosting a
  model is your compliance question, not this project's.
- If you point `oaica` at a vendor's API, that vendor's terms and data-handling
  policy apply to everything you send — this client is a pipe, not a policy.

## Checklist for a security review

Facts you can verify on your own host, without reading the source:

1. `oaica --version` — the version, and whether it matches what you pinned.
2. `oaica doctor` — which remotes exist, which are reachable (a live
   read-only `GET /models` per remote), and the route policy each would
   default a launch to. Read-only; exits non-zero if a configured remote
   fails its probe.
3. `oaica doctor --report` — redacted support bundle: platform, config paths
   and their permissions, and *which* credentials are set (never their values).
   Safe to attach to a ticket; it refuses to print if it would leak.
4. `ls -la ~/.oaica/` — confirm `0700`/`0600` on your chosen disk layout.
5. `grep -iE 'sk-|key|token|bearer' ~/.oaica/requests.log` — no key material
   in the file: it holds sizes, timing and status, not content or credentials.
6. `gh attestation verify …` — that the binary you deployed was built by this
   repository's release workflow from a specific commit.

Questions we cannot answer for you, because they are yours: whether prompts may
leave your network at all, and whether the endpoint you choose is approved to
receive them.
