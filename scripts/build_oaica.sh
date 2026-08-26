#!/usr/bin/env bash
# scripts/build_oaica.sh — build the release archives that oaica.com/install.sh
# and install.ps1 download, for every platform they support.
#
# oaica is a thin client (no local inference engine, CGO_ENABLED=0), so all
# targets cross-compile from one Linux box. This replaces the ad-hoc builds
# that produced site/download/ by hand: the shipped 0.2.0 binary drifted three
# weeks / ~100 commits behind main and reported vcs.modified=true, and
# linux-arm64 — which install.sh requests on aarch64 — was never built at all.
#
# Layout inside every archive is bin/oaica (bin/oaica.exe on Windows); that
# is what the installers extract.
#
# Usage:
#   VERSION=0.3.0 scripts/build_oaica.sh            # -> site/download/
#   VERSION=0.3.0 OUT=dist scripts/build_oaica.sh   # -> dist/
#
# VERSION is required and must be a bare semver (no "v"): it is stamped into
# `oaica --version` via version.Version. Local dev builds without it report
# 0.0.0, which is deliberate — an unstamped binary must not look released.
set -euo pipefail

cd "$(dirname "$0")/.."

: "${VERSION:?set VERSION=x.y.z (bare semver, what 'oaica --version' will print)}"
case "$VERSION" in
  v*) echo "VERSION must not start with 'v' (got $VERSION)" >&2; exit 1 ;;
  *[!0-9.a-z-]*|"") echo "VERSION must be a bare semver like 0.3.0 (got $VERSION)" >&2; exit 1 ;;
esac

OUT="${OUT:-site/download}"
LDFLAGS="-s -w -X github.com/ollama/ollama/version.Version=${VERSION}"

for tool in zstd zip tar; do
  command -v "$tool" >/dev/null 2>&1 || { echo "ERROR: $tool is required" >&2; exit 1; }
done

mkdir -p "$OUT"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# os/arch -> archive kind. Must stay in sync with scripts/install.sh
# (linux amd64/arm64 tar.zst with tgz fallback; darwin amd64/arm64 zip) and
# scripts/install.ps1 (windows amd64 zip).
TARGETS=(
  "linux/amd64/tar"
  "linux/arm64/tar"
  "darwin/amd64/zip"
  "darwin/arm64/zip"
  "windows/amd64/zip"
)

for t in "${TARGETS[@]}"; do
  IFS=/ read -r goos goarch kind <<<"$t"
  stage="$WORK/$goos-$goarch"
  mkdir -p "$stage/bin"
  bin="oaica"; [ "$goos" = windows ] && bin="oaica.exe"

  echo "building $goos/$goarch ($VERSION)"
  # -buildvcs=false: the tree is always dirty at release time (the tracked
  # archives under site/download/ change during this very build), so Go's
  # vcs.modified stamp would read "true" on every release and mean nothing.
  # The commit is recorded in VERSION.txt below instead.
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o "$stage/bin/$bin" .

  name="oaica-$goos-$goarch"
  case "$kind" in
    tar)
      tar -C "$stage" -cf - bin | zstd -19 -T0 -q -o "$OUT/$name.tar.zst" -f
      tar -C "$stage" -czf "$OUT/$name.tgz" bin
      ;;
    zip)
      rm -f "$OUT/$name.zip"
      (cd "$stage" && zip -q -r -X "$OLDPWD/$OUT/$name.zip" bin)
      ;;
  esac
done

# Verify the stamp on the archive a Linux host would actually install, using
# the host's own binary so this works on any amd64/arm64 builder.
host_arch="$(uname -m)"; case "$host_arch" in x86_64) host_arch=amd64 ;; aarch64|arm64) host_arch=arm64 ;; esac
if [ "$(uname -s)" = Linux ] && [ -f "$OUT/oaica-linux-$host_arch.tar.zst" ]; then
  chk="$WORK/chk"; mkdir -p "$chk"
  zstd -dc "$OUT/oaica-linux-$host_arch.tar.zst" | tar -xf - -C "$chk"
  got="$("$chk/bin/oaica" --version 2>&1 | tail -1)"
  case "$got" in
    *"$VERSION"*) echo "stamp OK: $got" ;;
    *) echo "ERROR: built binary reports '$got', expected version $VERSION" >&2; exit 1 ;;
  esac
fi

commit="$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
printf 'version=%s\ncommit=%s\n' "$VERSION" "$commit" > "$OUT/VERSION.txt"
(cd "$OUT" && sha256sum oaica-*.tar.zst oaica-*.tgz oaica-*.zip > SHA256SUMS)
echo; ls -l "$OUT"
