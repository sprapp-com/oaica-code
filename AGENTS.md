# AGENTS.md

## Building

For a full build from the repository root:

```sh
cmake -B build .
cmake --build build --parallel 8
./ollama serve
```

For quick Go-only iteration against an existing native payload:

```sh
go build .
go run . serve
```

The instructions above are inherited upstream Ollama text. This fork's own
binary is built with `scripts/build_oaica.sh` (it writes
`site/download/VERSION.txt`), and the Claude Code launcher lives in
`cmd/launch`.

See `docs/development.md` for prerequisites, platform notes, GPU backends, and
the full development workflow.
