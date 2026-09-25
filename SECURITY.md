# Security

`oaica` is a fork of [Ollama](https://github.com/ollama/ollama)'s client,
maintained by sprapp-com. Upstream's security policy does not cover this fork:
report issues here, not to hello@ollama.com, so they do not sit in a queue for
code we do not share.

## Reporting a vulnerability

Email **security@sprapp.com** with a description, reproduction steps, the
version (`oaica --version`) and platform, and your impact assessment. If you
prefer, open a [private security
advisory](https://github.com/sprapp-com/oaica-code/security/advisories/new)
instead — either reaches the same maintainers.

Please do not open a public issue for a vulnerability, and give us time to fix
it before disclosing publicly.

**What to expect:** we acknowledge within 3 business days, aim to have a fix or
a mitigation in the next release for anything rated high or critical, and
credit reporters in the release notes unless asked not to.

## Supported versions

Only the latest release is supported — fixes ship as a new tag, never as a
patch to an older one. Releases are at
[releases/latest](https://github.com/sprapp-com/oaica-code/releases/latest);
`oaica --version` tells you what you are running.

## What this software handles

Relevant when assessing an issue, and when deciding what to report:

- **Credentials.** Your API key (`OAICA_API_KEY`) is read from the environment
  or from a config file written by `oaica provider login`. It is sent as a
  bearer token to the endpoint you are talking to, and is not written to logs,
  usage records, or caches: that was audited across `requests.log`, the picker
  and catalog caches, and every command that prints a remote. A credential
  configured inside a URL prints as `https://REDACTED@host/v1`, and a bare
  `https://<token>@host/v1` remote is promoted to an ordinary bearer so the
  secret stops travelling in URLs and process arguments. `oaica doctor
  --report` prints an explicitly redacted support bundle, and refuses to print
  at all if a secret value would appear in it. See
  [docs/ENTERPRISE.md](docs/ENTERPRISE.md) for the full data-handling picture.
- **What leaves the machine.** Prompts, attachments and tool output go to the
  endpoint you selected (`api.oaica.com` by default, or a remote you
  configured). There is no telemetry, no crash reporting, and no analytics.
  Two requests happen without you asking, and neither carries a payload: an
  update check — a plain GET of the release's `VERSION.txt`, at most once per
  20 hours, with no body and no identifiers, which tells that host your IP and
  that oaica is installed (`OAICA_NO_UPDATE_CHECK=1` turns it off) — and, on an
  install that has been activated, a revalidation of that licence at most once
  every 7 days, sending only the key and activation id already stored. Beyond
  those: `oaica launch <agent>` offers to run an agent's own vendor installer
  when that agent is missing, `oaica launch pi` asks before installing its
  npm-hosted web-search package (`PI_OFFLINE=1` suppresses that entirely), the
  `oaica … sync` commands read a catalog from GitHub unless you pass `--url`,
  and `oaica pull` fetches weights from the URL your router's manifest names.
  Every one of those asks first or is a command you typed; hosts, payloads and
  off switches for all eight paths are in
  [docs/ENTERPRISE.md](docs/ENTERPRISE.md#network-connections).
- **Files written.** Configuration and state live under `~/.oaica/` (config,
  plans, remotes, caches, usage counters). Installers write the binary into a
  directory on `PATH` and nothing else, and remove a `lib/oaica` directory left
  by older versions of the script; the macOS/Linux installer cleans up `$TMPDIR`
  on exit.
- **Installer trust chain.** Archives are fetched from the GitHub release named
  by the version being installed and verified against that release's
  `SHA256SUMS` before extraction. When verification is impossible (no
  `sha256sum`/`shasum`, no `SHA256SUMS`, no entry) the installers warn and
  continue; a **mismatch** retries three times and then fails. Windows binaries
  are not Authenticode-signed (no code-signing certificate yet) — the checksum,
  and the build provenance attestation below, are the integrity signals.
- **Build provenance.** Releases built by this repository's `release.yaml`
  carry a Sigstore (keyless) attestation that each asset was built by that
  workflow from a specific commit. Releases published before the attestation
  step was added do not, so verify before you rely on it:
  ```
  gh attestation verify oaica-linux-amd64.tar.zst -R sprapp-com/oaica-code
  ```
  Those releases also ship a CycloneDX SBOM (`oaica-<version>.sbom.cdx.json`)
  listing the Go modules and versions in the binaries. Both are produced by
  `release.yaml`, so a release predating that step has neither; the
  [installation doc](docs/ENTERPRISE.md#installing-pinning-air-gapped) says
  which ones do.

## Supply-chain practices in this repository

- Actions in this repo's own workflows are pinned to commit SHAs, not tags.
  Upstream's workflows are not (they are replaced on merge), so pinning the
  fork's is the boundary that matters.
- `oaica-ci.yaml` builds and tests every push to `main` and every PR touching
  `cmd/`, `anthropic/`, `tools/` or `scripts/`, including each `tools/` module.
- `install-smoke.yaml` installs the published release on Linux, macOS and
  Windows and fails if the installed binary is not the version the release
  claims.

## Security best practices

- Keep to the latest release (`OAICA_VERSION` pins a version when you need one;
  the installers fetch it from that version's release assets).
- Treat an API key like a password: keep it out of shared shell history and
  CI logs, and prefer the environment over committing a config file.
- When pointing `oaica` at a self-hosted endpoint, secure that endpoint
  yourself — this client authenticates to it but does not protect it.
