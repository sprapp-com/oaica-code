# NeMo Agent Toolkit sidecar

Gives oaica-code's `/agent <task>` command real tool-calling via [NVIDIA
NeMo Agent Toolkit](https://github.com/NVIDIA/NeMo-Agent-Toolkit)
(`nvidia-nat`, formerly AIQ Toolkit). The agent's LLM backend is our own
`api.sprapp.com` router (model `flashplan`) — this sidecar adds the
ReAct tool-loop on top, it does not add a new model.

## Setup (one-time)

```bash
python3 -m venv nat/venv
nat/venv/bin/pip install nvidia-nat nvidia-nat-langchain
```

## Run

```bash
export OAICA_API_KEY=<your-key>
./nat/serve.sh
```

Then from oaica-code: `/agent What's the current time?` (TTY REPL or
piped mode, both work).

## What's wired

`workflow.yml` configures a single real tool (`current_datetime`, NAT's
built-in) as a proof of the integration. Add more tools by extending the
`functions:`/`tool_names:` sections — see NAT's own docs for its tool
catalog and how to write custom `_type: python` functions.
