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

Exactly five outbound paths exist. Nothing else in the client opens a socket.

| # | Destination | When | What is sent | Off switch |
|---|---|---|---|---|
| 1 | Your model endpoint | Every model call | Your prompts, attachments and tool output — the traffic you asked for | Choose the endpoint (`OAICA_HOST`, a remote, or a local `oaica serve`) |
| 2 | `oaica.com` | Install and upgrade only | Nothing; it serves the install scripts | Install from the GitHub release instead |
| 3 | `github.com/sprapp-com/oaica-code/releases/latest/download/VERSION.txt` | At most once per 20h, from any command that would print the update notice | A plain GET for the release's version file; **sends no body, no identifiers**. The request reveals your IP and that oaica is installed | `OAICA_NO_UPDATE_CHECK=1` |
| 4 | Vendor installers for optional integrations | Only when you run `oaica launch <agent>` for an agent that is not installed and you confirm the install | The vendor's own installer download | Don't launch that agent; install it yourself first |
| 5 | `registry.npmjs.org` | Only when you run `oaica launch pi`, and you confirm the prompt | A GET for the `@ollama/pi-web-search` package version, and the `pi install`/`pi update` it then runs | Decline the prompt, or set `PI_OFFLINE=1`. The check is skipped in offline mode, and `oaica launch pi` installs Pi itself only after its own prompt |

The endpoint in row 1 defaults to `https://api.oaica.com` (an
OpenAI-compatible router) and is where the work actually goes. If prompts may
not leave your network, point `oaica` at an endpoint inside it — a remote in
`~/.oaica/remotes.json`, or `oaica pull` + `oaica serve` for a fully local
model — and confirm with `oaica doctor`, whose output shows which remote each
launch would use.

Row 4 exists because `oaica launch claude` (and `codex`, `kimi`, `hermes`,
`opencode`, `dsh`, `pi`, …) will offer to install that agent from its
publisher when it is missing, after a prompt that says so. Those agents are not
part of this repository, are not pinned or audited by it, and once installed
they keep their own update and login behaviour — including talking to their own
vendors' backends if you sign in to them natively. In a controlled environment,
install the agents from your own package mirror and let `oaica launch` find them
already on `PATH`. Catalog metadata can be synced from an internal mirror
instead of GitHub: `oaica remote sync --url`, `oaica model sync --url` and
`oaica model cloud-limits sync --url` all accept `file://` paths and plain
HTTP URLs.

### Telemetry

There is none. No analytics, no crash reporting, no usage beacons, no
"anonymous" install ID. Grep the tree for `telemetry`, `analytics`, `sentry`,
`posthog`, `segment` and you will find the TUI's own render helpers and
nothing else. Row 3 is the only unprompted outbound request the client makes,
and it carries no payload — every other non-model path is behind a prompt you
answer, or behind a command you typed (`oaica launch pi` is the one to know
about: it asks before installing its web-search package, and row 5 lists what
that touches).

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
| `~/.oaica/local_servers.json` | Which `oaica serve` instances are running, and their ports | No |
| `~/.oaica/requests.log` | One line per launch request: model name, which backend served it (a label, or the endpoint URL with any credential redacted), message *sizes*, timing, status | No — sizes, not content |
| `~/.oaica/cache/` | Cached catalog and probe answers | No |
| `~/.oaica/update_check.json` | Last update check: when, and which version was newest | No |
| `~/.oaica/models/` | Downloaded GGUF weights | No |
| `~/.oaica/license_key`, `license.json` | Purchased-license state | **Yes** (a licence key, not an API key) |

`requests.log` is worth calling out because it is the one file a network
security team will ask about: it records the byte length of the last message
and of the whole conversation, plus a boolean "would the router have called this
hard", and never message text, headers, or credentials. `oaica usage` summarises
it. Delete the file at any time; nothing else depends on it.

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
  covers every place a key can sit — the environment, `remotes.json`,
  `auth.json`, and the `api_key`, `license_key` and `license.json` files — so
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

One known exception, stated rather than implied: `oaica launch kimi` passes its
generated configuration to Kimi's own CLI as a `--config <json>` argument, and
that JSON contains the provider key — so for that one integration the key is in
the child process's argument list, readable from `/proc/<pid>/cmdline` by any
local user. Codex, Claude Code, opencode and the rest receive their credentials
through the environment or a config file instead. If that matters on a shared
host, don't launch Kimi there, or run it on a machine where you are the only
user.

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

What the installer does, in order: resolve the release for the version being
installed, download the archive **and that release's `SHA256SUMS`**, verify the
archive's SHA-256, extract `bin/oaica` into a temporary directory, then install
it as `/usr/local/bin/oaica` (mode `755`), using `sudo` when that directory is
not writable. The temporary directory is removed on exit via an `EXIT` trap.
It does not touch your shell profile, does not install a service, and writes
nothing outside the temp dir and the binary's destination.

Three things you should know rather than assume:

- **Pin the version in production.** `OAICA_VERSION=0.5.46` makes the installer
  fetch `oaica-v0.5.46`'s assets, and the binary then reports that version. An
  unpinned install is `releases/latest` at the moment you run it.
- **A mirror is supported first-class.** `OAICA_DOWNLOAD_BASE` points the
  installer at your own base URL, which wins over version resolution — that is
  the air-gapped path. Stage the archive and `SHA256SUMS` from a release into
  your artifact store and set this.
- **Verification can fail open, and says so.** If `sha256sum`/`shasum` is
  missing, or `SHA256SUMS` cannot be fetched, or it has no entry for the
  archive, the installer prints a warning and continues. A **mismatch** retries
  three times and then fails. In a high-assurance environment, verify the
  archive yourself before installing — the digests are all in the release.

Every release asset also carries a keyless Sigstore attestation and a
CycloneDX SBOM:

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
2. `oaica doctor` — which remotes exist, which are reachable, and which remote
   a launch would actually use. Read-only; exits non-zero if a configured
   remote fails.
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
