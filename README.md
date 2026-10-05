# oaica

Install and download page for **oaica**, a terminal AI CLI (a fork of Ollama's
client) for running models you already have — a local Ollama or `llama.cpp`
server, a self-hosted `oaica serve`, or any OpenAI-compatible endpoint.

This repository is the **public distribution surface** for oaica. It contains
only:

- these install instructions,
- the **release binaries** (attached to each release),
- the **model and provider configuration** the client reads at runtime
  (`models/models.json`, `cmd/launch/providers/oaica.json`),
- the licence notices (`LICENSE`, `NOTICE`, `EULA.md`).

The oaica source code is proprietary and is **not** published here.

## Install

**macOS / Linux**

```sh
curl -fsSL https://github.com/sprapp-com/oaica-code/releases/latest/download/install.sh | bash
```

**Windows** (PowerShell)

```powershell
irm https://github.com/sprapp-com/oaica-code/releases/latest/download/install.ps1 | iex
```

The installers verify the archive against `SHA256SUMS` before extracting, ask
you to accept the EULA, and offer to install [Ollama](https://ollama.com) — the
recommended local engine — but never install an engine on their own.
Non-interactive installs must accept explicitly with `OAICA_ACCEPT_TERMS=1`.

Pin a version, or point at a mirror:

```sh
OAICA_VERSION=0.6.1 curl -fsSL https://github.com/sprapp-com/oaica-code/releases/latest/download/install.sh | bash
```

Landing page, download table and checksums: **https://oaica.com**.

## Uninstall

```sh
curl -fsSL https://github.com/sprapp-com/oaica-code/releases/latest/download/install.sh | OAICA_UNINSTALL=1 bash          # macOS / Linux
$env:OAICA_UNINSTALL=1; irm https://github.com/sprapp-com/oaica-code/releases/latest/download/install.ps1 | iex            # Windows
```

## Licence

oaica is proprietary software. The prebuilt binary is licensed under the oaica
End User Licence Agreement in `EULA.md`. It is free for 14 days with no
activation code; the free tier — local models, `oaica serve`, and your own
endpoints and API keys — is never gated and does not expire.

oaica is a derivative of [Ollama](https://github.com/ollama/ollama); the
upstream Ollama code keeps its MIT licence, reproduced in `LICENSE` and
applying to that code only. See `NOTICE` for the division.

## Update notifications

Installed clients read `VERSION.txt` from the latest release to tell you when a
newer version is out.
