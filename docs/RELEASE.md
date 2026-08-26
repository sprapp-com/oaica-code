# Releasing oaica

oaica is a thin, CGO-free client; every platform cross-compiles from one
Linux box. Two things ship per release and they must stay in sync:

1. **GitHub release** — built by `.github/workflows/release.yaml` when an
   `oaica-v<semver>` tag is pushed.
2. **oaica.com** — the landing page plus `oaica.com/download/*` and
   `oaica.com/install.sh`, served from the `site/` directory of this repo via
   Cloudflare Pages: project **`oaica-install`** in the unisqu account
   (`125f3856…`), with `oaica.com` CNAMEd to `oaica-install.pages.dev`.
   (There is no `oaica-com` project; `/mnt/ext9/cloudflare-pages/oaica-com`
   on the laptop is a stale copy of the landing page only.) `install.sh`
   downloads from oaica.com, not from GitHub.

Why this exists: the binary at `oaica.com/download` had been hand-built on
2026-08-04 from `329de0bf` with a dirty tree (`vcs.modified=true`), then left
for three weeks / ~100 commits while `main` moved on. No tag, no GitHub
release, no way to tell from the binary what it contained. Linux arm64 —
which `install.sh` requests on aarch64 — had never been built at all.

## Version stamping

`version/version.go` defaults to `0.0.0`. A build only reports a real
version when `-X github.com/ollama/ollama/version.Version=<semver>` is passed,
which `scripts/build_oaica.sh` does from `VERSION`. A `0.0.0` binary is a dev
build by definition — never publish one.

Tags are `oaica-v<semver>`. Plain `v*` tags are upstream Ollama's and already
exist in this repo (`v0.3.0`, `v0.32.5`, ...), so they cannot name a fork
release.

## Cut a release

```bash
# 0. clean tree on main, tests green
git status --short            # must be empty
go test ./cmd/... ./tools/...

# 1. build every archive into site/download/ (stamps VERSION, verifies the
#    Linux binary reports it, writes SHA256SUMS)
VERSION=0.3.0 scripts/build_oaica.sh

# 2. commit the archives + tag
git add site/download
git commit -m "release: oaica 0.3.0"
git tag oaica-v0.3.0
git push origin main oaica-v0.3.0     # tag push triggers the GitHub release

# 3. publish oaica.com (wrangler must be logged into the unisqu account;
#    on the laptop CLOUDFLARE_API_TOKEN in the shell env is. The tunnel/DNS
#    token in ~/.secrets/cloudflare_oaica.env has no Pages permission.)
wrangler pages deploy site --project-name oaica-install --commit-dirty=true

# 3b. GitHub release. The tag push should run release.yaml, but this repo is
#     a FORK of ollama/ollama and GitHub does not run workflows on forks
#     until someone clicks "enable workflows" once in the repo's Actions tab
#     (the REST permissions endpoint already says enabled=true; that is not
#     the same gate). Until that is done, or if the run does not appear,
#     create the release by hand from the same archives — this is how
#     oaica-v0.3.0 was published:
gh release create oaica-v0.3.0 --title "oaica 0.3.0" --notes "<changes>" \
  site/download/oaica-* site/download/SHA256SUMS site/download/VERSION.txt \
  scripts/install.sh scripts/install.ps1

# 4. verify what a new user gets
curl -fsSL https://oaica.com/install.sh | bash
oaica --version                        # must print 0.3.0
OAICA_API_KEY=<key> oaica run kat-awq 'reply with exactly: pong'
```

Order: commit code → build → commit archives → tag. The build records the
commit it was made from in `site/download/VERSION.txt`; it passes
`-buildvcs=false` because the tree is always dirty at release time (the
tracked archives change during the build itself), which would make Go's
`vcs.modified` stamp read `true` on every release and mean nothing.

## Archive layout (what the installers expect)

| File | Contents | Installed by |
|---|---|---|
| `oaica-linux-{amd64,arm64}.tar.zst` (+ `.tgz` fallback) | `bin/oaica` | `install.sh` |
| `oaica-darwin-{amd64,arm64}.zip` | `bin/oaica` | `install.sh` |
| `oaica-windows-amd64.zip` | `bin/oaica.exe` | `install.ps1` |

`scripts/install.sh` and `site/install.sh` are the same file; keep them
identical (`cmp` them before deploying).
