# Releasing oaica

oaica is a thin, CGO-free client; every platform cross-compiles from one
Linux box. Two things ship per release and they must stay in sync:

1. **GitHub release** — built by `.github/workflows/release.yaml` when an
   `oaica-v<semver>` tag is pushed. This is the source of truth for artifacts:
   the archives, `SHA256SUMS`, `VERSION.txt` and both install scripts.
2. **oaica.com** — the landing page plus `oaica.com/install.sh` and
   `oaica.com/install.ps1`, served from the `site/` directory of this repo via
   Cloudflare Pages: project **`oaica-install`** in the unisqu account
   (`125f3856…`), with `oaica.com` CNAMEd to `oaica-install.pages.dev`.
   (There is no `oaica-com` project; `/mnt/ext9/cloudflare-pages/oaica-com`
   on the laptop is a stale copy of the landing page only.)

   Pages serves only the *scripts* now — they fetch the archive and its
   checksum from the GitHub release for the version being installed, so the
   hand-maintained `site/download/` copy is no longer on the install path and
   a Pages deploy can no longer ship a wrong binary.

Why the release is the source of truth: the binary at `oaica.com/download` had
been hand-built on 2026-08-04 from `329de0bf` with a dirty tree
(`vcs.modified=true`), then left for three weeks / ~100 commits while `main`
moved on. No tag, no GitHub release, no way to tell from the binary what it
contained. Linux arm64 — which `install.sh` requests on aarch64 — had never
been built at all. Even after CI took over the release, the Pages copy kept
drifting: on 2026-09-26 a fresh install still produced **0.5.45** while the
newest release was `oaica-v0.5.46`, because `site/download/` is published by a
manual `wrangler pages deploy` that nothing forces anyone to run.

**Current:** see the [latest GitHub
release](https://github.com/sprapp-com/oaica-code/releases/latest) — read the
tag there rather than trusting a number written into this file.

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
VERSION=<semver> scripts/build_oaica.sh

# 2. commit the archives + tag
git add site/download
git commit -m "release: oaica <semver>"
git tag oaica-v<semver>
git push origin main oaica-v<semver>  # tag push triggers the GitHub release

# 3. publish oaica.com (wrangler must be logged into the unisqu account;
#    on the laptop CLOUDFLARE_API_TOKEN in the shell env is. The tunnel/DNS
#    token in ~/.secrets/cloudflare_oaica.env has no Pages permission.)
#    Only needed when a file under site/ changed (the landing page, the
#    install scripts, site/download/*) — a release whose artifacts are
#    unchanged needs no Pages deploy, because the installers read the
#    archives from GitHub.
wrangler pages deploy site --project-name oaica-install --commit-dirty=true

# 3b. GitHub release: the tag push runs release.yaml, which builds the same
#     archives on a runner and creates the release (or, if one already
#     exists, replaces its assets). Actions is live on this fork since
#     2026-08-26 (0.3.0-0.4.1 were created by hand before that; the runner
#     build proved green on the 0.4.1 tag). Watch it:
gh run list --workflow release.yaml -L 3
#     To ship release notes better than --generate-notes, create the release
#     by hand with the tag ALREADY PUSHED (or pass --target <sha>): if the
#     tag does not exist on GitHub, `gh release create` makes one at the
#     remote default branch's HEAD, and your local tag on the archives
#     commit will then be rejected as conflicting (0.4.2, harmless but
#     confusing). The workflow then only refreshes assets:
gh release create oaica-v<semver> --title "oaica <semver>" --notes-file notes.md \
  site/download/oaica-* site/download/SHA256SUMS site/download/VERSION.txt \
  scripts/install.sh scripts/install.ps1

# 4. verify what a new user gets — in a clean container, with the version
#    pinned, so a stale artifact anywhere on the path fails loudly:
docker run --rm -v "$PWD/scripts/install.sh:/tmp/install.sh:ro" ubuntu:24.04 \
  bash -c "apt-get update -qq && apt-get install -y -qq curl zstd ca-certificates &&
           OAICA_VERSION=<semver> sh /tmp/install.sh && oaica --version"
# must print: oaica <semver>   (a mismatch means the release assets are not
# what the tag claims — do not announce the release until this passes)

# and once on the machine running it for real:
oaica --version
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
identical (`cmp` them before deploying). Same for `scripts/install.ps1` and
`site/install.ps1` — `release.yaml` publishes the `scripts/` copy while
oaica.com serves the `site/` one, so drift between them means the two install
paths behave differently for the same version.
`scripts/tests/install_checksum_test.sh` asserts both pairs are byte-identical
and exercises the download/verify/retry helpers against a local server; run it
before any installer change.

## Installing from a release, and offline

The installers take their base URL from the environment, which is what makes
pinned, mirrored and air-gapped installs possible without a different script:

| Variable | Effect |
|---|---|
| `OAICA_VERSION` | install that version (`oaica-v<version>` release); a leading `v` is accepted |
| `OAICA_DOWNLOAD_BASE` | fetch archives and `SHA256SUMS` from here instead of GitHub |
| `OAICA_RELEASE_REPO` | different repository owning the releases (fork / internal mirror) |

`OAICA_DOWNLOAD_BASE` wins over `OAICA_VERSION`, so a mirror serves the pinned
version from its own copy of the assets. `OAICA_DOWNLOAD_URL` (Windows only)
is kept as an alias of `OAICA_DOWNLOAD_BASE`.

To mirror a release for an air-gapped host, copy the archives, `SHA256SUMS`
and `VERSION.txt` from the GitHub release and serve them from any static
directory; the checksum verification then runs against the mirror's own
`SHA256SUMS`.

Every archive is verified against `SHA256SUMS` before extraction. When
verification is impossible — no `sha256sum`/`shasum`, no `SHA256SUMS`, no entry
for that archive — the installers warn and continue (so an offline mirror
without checksums still works), but a *mismatch* is fatal and retried up to
three times.
