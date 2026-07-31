#!/bin/bash
# nat/serve.sh — launches the NeMo Agent Toolkit sidecar oaica-code's
# `/agent <task>` command talks to (OAICA_AGENT_HOST env var, defaults
# http://127.0.0.1:8600 — see cmd/oaica_client.go's oaicaAgentHost()).
#
# One-time setup:
#   python3 -m venv nat/venv
#   nat/venv/bin/pip install nvidia-nat nvidia-nat-langchain
#
# Requires OAICA_API_KEY to be set (same key oaica-code itself uses) —
# the sidecar's LLM backend is our own api.sprapp.com router, not a
# separate NVIDIA-hosted model.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ -z "${OAICA_API_KEY:-}" ]; then
  echo "OAICA_API_KEY is not set — the sidecar's LLM calls to api.sprapp.com will fail without it." >&2
  exit 1
fi

VENV="${NAT_VENV:-$DIR/venv}"
if [ ! -x "$VENV/bin/nat" ]; then
  echo "nat CLI not found at $VENV/bin/nat — run the one-time setup (see this script's header) or set NAT_VENV." >&2
  exit 1
fi

exec "$VENV/bin/nat" serve --config_file "$DIR/workflow.yml" --host 127.0.0.1 --port "${OAICA_AGENT_PORT:-8600}"
