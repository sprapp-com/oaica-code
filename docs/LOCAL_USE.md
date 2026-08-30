# Self-hosting with oaica

Run a model entirely on your own machine, free and offline: `oaica pull`
downloads a GGUF, `oaica serve` runs it through llama.cpp's `llama-server`
and exposes an OpenAI-compatible endpoint. There is no Ollama daemon
involved anywhere in this path.

## Prerequisites: install llama.cpp

`oaica serve` shells out to `llama-server`. It must be on your `PATH`, or
you can point at it directly with `OAICA_LLAMA_SERVER=/path/to/llama-server`.

Install it from the
[llama.cpp releases page](https://github.com/ggml-org/llama.cpp/releases),
or via your package manager:

```shell
# macOS (Homebrew)
brew install llama.cpp

# Arch Linux
pacman -S llama.cpp

# Other Linux / no package: download a prebuilt release archive from
# https://github.com/ggml-org/llama.cpp/releases and put `llama-server`
# on your PATH, or build from source per the llama.cpp README.
```

Verify it's found:

```shell
which llama-server
# or
llama-server --version
```

## Pull a model

```shell
oaica pull qwen2.5-0.5b            # 491 MB smoke-test model
oaica pull oaica-nemotron-30b-a3b  # 25 GB, Q4_K_M GGUF, reasoning + tools
```

The catalog is served from `GET https://api.oaica.com/v1/catalog` and may
grow over time; `oaica pull --help` and the catalog endpoint are the source
of truth for what's currently available. Weights are streamed into
`~/.oaica/models/<model>.gguf` (override the directory with
`OAICA_MODELS_DIR`).

## Serve it

```shell
oaica serve qwen2.5-0.5b
```

Useful flags (run `oaica serve --help` for the full, current list):

| Flag | Purpose |
|---|---|
| `--port INT` | Bind port (default: auto-pick a free port) |
| `--host STRING` | Bind address (default `127.0.0.1`; use `0.0.0.0` to expose on the network — requires `--api-key`) |
| `--api-key STRING` | Bearer token required on every request; mandatory when `--host` is not loopback |
| `--insecure` | Allow a non-loopback `--host` with no `--api-key` (trusted private networks only) |
| `--ctx-size INT` | Context size (default 8192) |
| `--threads INT` | CPU threads (default: physical core count) |
| `--no-cmoe` | Disable CPU-RAM MoE expert offload (needs much more VRAM without it) |
| `--ncmoe INT` | Keep only the first N layers' MoE experts on CPU, overriding `--no-cmoe`; tune per model/GPU |

`oaica serve` prints the local OpenAI-compatible base URL once the server
is ready.

## Pointing Claude Code / other OpenAI clients at it

The simplest path is to let `oaica launch` do the wiring for you once the
model is registered — `oaica serve` registers running local servers in
`~/.oaica/local_servers.json`, and `oaica model` lets you add or inspect
manifest entries (context window, engine) for `launch` to use:

```shell
oaica serve qwen2.5-0.5b &
oaica launch claude --model qwen2.5-0.5b
```

For any other OpenAI-compatible client, point it at the base URL
`oaica serve` printed (typically `http://127.0.0.1:<port>/v1`) and the
`--api-key` value if you set one. See `oaica model --help` and
`oaica remote --help` if you'd rather register the endpoint as a permanent
remote.

## GPU vs CPU notes

- `llama-server` will use available GPU layers automatically where
  supported; MoE models (like `oaica-nemotron-30b-a3b`) can offload expert
  layers to CPU RAM to fit in less VRAM — see `--no-cmoe` / `--ncmoe` above.
- If you don't have a supported GPU, `llama-server` falls back to CPU
  inference; tune `--threads` for your core count.
- `oaica gpu ps` lists processes currently holding GPU memory (read-only);
  `oaica gpu clean` kills orphaned GPU worker processes (PPID=1, known
  worker pattern only) if a previous `serve` was killed uncleanly.

## Where files live

| Path | Contents |
|---|---|
| `~/.oaica/models/<model>.gguf` | Downloaded weights |
| `~/.oaica/models.json` | Local model manifest (used by `oaica launch`) |
| `~/.oaica/local_servers.json` | Registry of currently running local `serve` instances |
| `~/.oaica/license_key` | Saved license key for gated models |

Override the models directory with `OAICA_MODELS_DIR`.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `llama-server: command not found` (or `serve` can't find it) | Install llama.cpp and ensure `llama-server` is on `PATH`, or set `OAICA_LLAMA_SERVER=/full/path/to/llama-server` |
| `pull` errors with an unknown/unrecognized model name | Check the current catalog — model names change over time; `GET https://api.oaica.com/v1/catalog` or `oaica pull --help` |
| A pull was interrupted partway through | Delete the partial download under `~/.oaica/models/` (look for a `.partial` file alongside the target `.gguf`) and re-run `oaica pull MODEL` |
| `serve` fails with "port in use" / address already bound | Pick a free port explicitly with `oaica serve MODEL --port <N>`, or find and stop whatever is already bound to the default port |
| `serve --host 0.0.0.0` refuses to start | Non-loopback hosts require `--api-key` (or pass `--insecure` if you understand the risk, on a trusted network only) |

## Uninstall

```shell
sudo rm /usr/local/bin/oaica   # or: rm ~/.local/bin/oaica
rm -rf ~/.oaica
```

This removes the binary, saved keys, the model manifest, and all
downloaded weights. If you installed `llama-server` separately via a
package manager, remove it with that package manager if you no longer
need it.
