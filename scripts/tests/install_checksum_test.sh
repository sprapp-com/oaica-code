#!/usr/bin/env bash
# Exercises the download / SHA256SUMS verification / retry helpers of
# scripts/install.sh against a local HTTP server, without installing anything.
#
# The helpers live between the "# --- download helpers (begin|end) ---"
# markers in install.sh; this test extracts that block and runs it under
# plain `sh` (what `#!/bin/sh` and the CI harness use) with stub status/error/
# warning/available functions. A small python http.server serves a fake
# oaica.com/download directory and can truncate or corrupt the first response
# for a path, which is the Cloudflare cache-fill failure this guards against.
#
#   bash scripts/tests/install_checksum_test.sh
set -euo pipefail
# Portable digest for the test's own fixtures: GitHub's macOS runners ship
# shasum, not sha256sum (the installer under test has the same fallback).
sum256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
INSTALL_SH="$ROOT/scripts/install.sh"

WORK="$(mktemp -d)"
SERVER_PID=
cleanup() {
    [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || :
    rm -rf "$WORK"
}
trap cleanup EXIT

PASS=0
FAIL=0
fail() { echo "FAIL: $*" >&2; FAIL=$((FAIL + 1)); }
ok() { echo "ok: $*"; PASS=$((PASS + 1)); }
assert_contains() { # haystack needle description
    case "$1" in
        *"$2"*) ok "$3" ;;
        *) fail "$3 — expected output to contain: $2"; printf '%s\n' "--- output ---" "$1" "--------------" >&2 ;;
    esac
}
assert_not_contains() {
    case "$1" in
        *"$2"*) fail "$3 — expected output NOT to contain: $2"; printf '%s\n' "--- output ---" "$1" "--------------" >&2 ;;
        *) ok "$3" ;;
    esac
}
assert_eq() { # got want description
    if [ "$1" = "$2" ]; then ok "$3"; else fail "$3 — got '$1', want '$2'"; fi
}

###########################################
# 1. The two copies of the installer must stay byte-identical
###########################################
if cmp -s "$INSTALL_SH" "$ROOT/site/install.sh"; then
    ok "scripts/install.sh and site/install.sh are identical"
else
    fail "scripts/install.sh and site/install.sh differ (cp scripts/install.sh site/install.sh)"
fi

###########################################
# 2. Extract the helper block
###########################################
sed -n '/^# --- download helpers (begin) ---/,/^# --- download helpers (end) ---/p' "$INSTALL_SH" > "$WORK/helpers.sh"
grep -q '^fetch_archive()' "$WORK/helpers.sh" || { echo "helper block not found in $INSTALL_SH" >&2; exit 1; }

cat > "$WORK/preamble.sh" <<'EOF'
set -eu
status() { echo ">>> $*" >&2; }
error() { echo "ERROR: $*"; exit 1; }
warning() { echo "WARNING: $*"; }
available() { command -v "$1" >/dev/null; }
VER_PARAM="${VER_PARAM:-}"
SUDO=
EOF

# run_sh BODY: run BODY under `sh` with the preamble + helpers loaded. Prints
# combined stdout/stderr; the exit status is that of the shell.
run_sh() {
    local tmp="$WORK/tmp.$RANDOM$RANDOM"
    mkdir -p "$tmp"
    TEMP_DIR="$tmp" sh -c ". '$WORK/preamble.sh'; . '$WORK/helpers.sh'; $1" 2>&1
}

###########################################
# 3. Fake download directory
###########################################
DL="$WORK/download"
mkdir -p "$DL/nosums" "$WORK/stage/bin"
printf '#!/bin/sh\necho oaica-fake\nexit 0\n' > "$WORK/stage/bin/oaica"
# Pad with incompressible bytes (after the exit) so the archives are ~128 KB
# and truncating them is meaningful.
head -c 131072 /dev/urandom >> "$WORK/stage/bin/oaica"
chmod 755 "$WORK/stage/bin/oaica"

HAVE_ZSTD=0
if command -v zstd >/dev/null; then
    HAVE_ZSTD=1
    tar -C "$WORK/stage" -cf - bin | zstd -q -o "$DL/oaica-linux-amd64.tar.zst"
fi
tar -C "$WORK/stage" -czf "$DL/oaica-linux-amd64.tgz" bin
# arm64: tgz only, so download_and_extract must take the tgz fallback path.
tar -C "$WORK/stage" -czf "$DL/oaica-linux-arm64.tgz" bin
python3 - "$WORK/stage" "$DL/oaica-darwin-arm64.zip" <<'EOF'
import os, sys, zipfile
stage, out = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    z.write(os.path.join(stage, "bin", "oaica"), "bin/oaica")
EOF
# Served correctly on every request; the server mangles the first response.
cp "$DL/oaica-linux-amd64.tgz" "$DL/short-once.tgz"
cp "$DL/oaica-linux-amd64.tgz" "$DL/corrupt-once.tgz"
# Listed with the real hash but the file on disk is truncated: permanent mismatch.
cp "$DL/oaica-linux-amd64.tgz" "$DL/corrupt-always.tgz"
# Not listed in SHA256SUMS at all.
cp "$DL/oaica-linux-amd64.tgz" "$DL/nosum.tgz"
# Directory without a SHA256SUMS file.
cp "$DL/oaica-linux-amd64.tgz" "$DL/nosums/oaica-linux-amd64.tgz"

(
    cd "$DL"
    listed=(oaica-*.tgz oaica-*.zip short-once.tgz corrupt-once.tgz corrupt-always.tgz)
    if [ "$HAVE_ZSTD" -eq 1 ]; then listed+=(oaica-*.tar.zst); fi
    sum256 "${listed[@]}" > SHA256SUMS
)
head -c 4096 "$DL/oaica-linux-amd64.tgz" > "$DL/corrupt-always.tgz.trunc"
mv "$DL/corrupt-always.tgz.trunc" "$DL/corrupt-always.tgz"
[ "$(wc -c < "$DL/corrupt-always.tgz")" -lt "$(wc -c < "$DL/oaica-linux-amd64.tgz")" ] || { echo "test setup: corrupt-always.tgz was not truncated" >&2; exit 1; }
GOOD_SUM="$(sum256 "$DL/oaica-linux-amd64.tgz" | cut -d ' ' -f1)"

###########################################
# 4. Local server: python http.server that can break the first response
###########################################
cat > "$WORK/server.py" <<'EOF'
import http.server, os, sys

# path -> number of remaining broken responses
broken = {}

class Handler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        base = os.path.basename(path)
        mode = None
        if base.startswith("short-"):
            mode = "short"
        elif base.startswith("corrupt-once"):
            mode = "corrupt"
        if mode and broken.setdefault(path, 1) > 0:
            broken[path] -= 1
            with open(self.translate_path(path), "rb") as f:
                data = f.read()
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            if mode == "short":
                # HTTP 200 with Content-Length for the full body, then hang up
                # a third of the way through: the Cloudflare cache-fill bug.
                self.wfile.write(data[: len(data) // 3])
            else:
                # Full-length body with the last byte flipped: length is
                # right, checksum is not.
                self.wfile.write(data[:-1] + bytes([data[-1] ^ 0xFF]))
            self.wfile.flush()
            return
        return super().do_GET()

os.chdir(sys.argv[1])
srv = http.server.HTTPServer(("127.0.0.1", 0), Handler)
print(srv.server_address[1], flush=True)
srv.serve_forever()
EOF
python3 "$WORK/server.py" "$DL" > "$WORK/port" &
SERVER_PID=$!
for _ in $(seq 1 50); do
    [ -s "$WORK/port" ] && break
    sleep 0.1
done
PORT="$(cat "$WORK/port")"
[ -n "$PORT" ] || { echo "server did not start" >&2; exit 1; }
BASE="http://127.0.0.1:$PORT"

###########################################
# 5. Cases
###########################################

# expected_sha256 parses "<sha256>  <filename>" (and "*filename" markers).
out=$(run_sh "expected_sha256 '$DL/SHA256SUMS' oaica-linux-amd64.tgz")
assert_eq "$out" "$GOOD_SUM" "expected_sha256 finds the entry for a filename"
printf '%s *starred.tgz\n' "$GOOD_SUM" > "$WORK/sums-star"
out=$(run_sh "expected_sha256 '$WORK/sums-star' starred.tgz")
assert_eq "$out" "$GOOD_SUM" "expected_sha256 strips a leading '*' binary marker"
rc=0; out=$(run_sh "expected_sha256 '$DL/SHA256SUMS' missing.tgz") || rc=$?
assert_eq "$rc" "1" "expected_sha256 returns 1 for an unlisted archive"

# Happy path: correct archive verifies on the first attempt.
rc=0; out=$(run_sh "fetch_archive '$BASE' oaica-linux-amd64.tgz \"\$TEMP_DIR/a.tgz\" && cmp -s \"\$TEMP_DIR/a.tgz\" '$DL/oaica-linux-amd64.tgz' && echo DOWNLOADED") || rc=$?
assert_eq "$rc" "0" "good archive: exit 0"
assert_contains "$out" "Checksum OK: oaica-linux-amd64.tgz" "good archive: checksum reported OK"
assert_contains "$out" "DOWNLOADED" "good archive: file content matches the served file"
assert_not_contains "$out" "retrying" "good archive: no retry"

# The macOS zip goes through the same verification.
rc=0; out=$(run_sh "fetch_archive '$BASE' oaica-darwin-arm64.zip \"\$TEMP_DIR/oaica-darwin.zip\" && unzip -q \"\$TEMP_DIR/oaica-darwin.zip\" -d \"\$TEMP_DIR\" && \"\$TEMP_DIR/bin/oaica\"") || rc=$?
assert_eq "$rc" "0" "darwin zip: exit 0"
assert_contains "$out" "Checksum OK: oaica-darwin-arm64.zip" "darwin zip: verified against SHA256SUMS"
assert_contains "$out" "oaica-fake" "darwin zip: extracted binary runs"

# ?version= query strings (OAICA_VERSION) are appended to both archive and SHA256SUMS URLs.
rc=0; out=$(VER_PARAM='?version=9.9.9' run_sh "fetch_archive '$BASE' oaica-linux-amd64.tgz \"\$TEMP_DIR/a.tgz\"") || rc=$?
assert_eq "$rc" "0" "OAICA_VERSION query string: exit 0"
assert_contains "$out" "Checksum OK: oaica-linux-amd64.tgz" "OAICA_VERSION query string: still verified"

# Truncated body (HTTP 200, Content-Length says more): retried, then succeeds.
rc=0; out=$(run_sh "fetch_archive '$BASE' short-once.tgz \"\$TEMP_DIR/s.tgz\" && cmp -s \"\$TEMP_DIR/s.tgz\" '$DL/short-once.tgz' && echo DOWNLOADED") || rc=$?
assert_eq "$rc" "0" "short body once: exit 0"
assert_contains "$out" "short body (partial download), retrying (2/3)" "short body once: retry message"
assert_contains "$out" "Checksum OK: short-once.tgz" "short body once: second attempt verified"
assert_contains "$out" "DOWNLOADED" "short body once: final file is the complete one"
assert_not_contains "$out" "(3/3)" "short body once: no third attempt"

# Full-length but corrupted body: checksum mismatch, retried, then succeeds.
rc=0; out=$(run_sh "fetch_archive '$BASE' corrupt-once.tgz \"\$TEMP_DIR/c.tgz\" && cmp -s \"\$TEMP_DIR/c.tgz\" '$DL/corrupt-once.tgz' && echo DOWNLOADED") || rc=$?
assert_eq "$rc" "0" "corrupt once: exit 0"
assert_contains "$out" "Checksum mismatch for corrupt-once.tgz: expected $GOOD_SUM, got " "corrupt once: mismatch details printed"
assert_contains "$out" "checksum mismatch, retrying (2/3)" "corrupt once: retry message"
assert_contains "$out" "Checksum OK: corrupt-once.tgz" "corrupt once: second attempt verified"
assert_contains "$out" "DOWNLOADED" "corrupt once: final file is the good one"

# Permanent mismatch: three attempts, clear error, non-zero exit, nothing installed.
rc=0; out=$(run_sh "fetch_archive '$BASE' corrupt-always.tgz \"\$TEMP_DIR/x.tgz\"; echo UNREACHABLE") || rc=$?
assert_eq "$rc" "1" "permanent mismatch: exit 1"
assert_contains "$out" "checksum mismatch, retrying (2/3)" "permanent mismatch: retry 2/3"
assert_contains "$out" "checksum mismatch, retrying (3/3)" "permanent mismatch: retry 3/3"
assert_contains "$out" "ERROR: checksum mismatch for corrupt-always.tgz after 3 attempts" "permanent mismatch: clear final error"
assert_not_contains "$out" "UNREACHABLE" "permanent mismatch: script stops"
assert_not_contains "$out" "(4/3)" "permanent mismatch: exactly 3 attempts"

# 404: download failure is retried and then fails clearly.
rc=0; out=$(run_sh "fetch_archive '$BASE' does-not-exist.tgz \"\$TEMP_DIR/n.tgz\"; echo UNREACHABLE") || rc=$?
assert_eq "$rc" "1" "404: exit 1"
assert_contains "$out" "download failed (curl exit 22), retrying (2/3)" "404: retry message"
assert_contains "$out" "ERROR: download failed (curl exit 22) for does-not-exist.tgz after 3 attempts" "404: clear final error"
assert_not_contains "$out" "UNREACHABLE" "404: script stops"

# Archive not listed in SHA256SUMS: warn and continue.
rc=0; out=$(run_sh "fetch_archive '$BASE' nosum.tgz \"\$TEMP_DIR/u.tgz\" && echo CONTINUED") || rc=$?
assert_eq "$rc" "0" "unlisted archive: exit 0"
assert_contains "$out" "WARNING: SHA256SUMS has no entry for nosum.tgz; skipping checksum verification" "unlisted archive: warning"
assert_contains "$out" "CONTINUED" "unlisted archive: install continues"

# No SHA256SUMS on the server: warn and continue.
rc=0; out=$(run_sh "fetch_archive '$BASE/nosums' oaica-linux-amd64.tgz \"\$TEMP_DIR/u.tgz\" && echo CONTINUED") || rc=$?
assert_eq "$rc" "0" "missing SHA256SUMS: exit 0"
assert_contains "$out" "WARNING: Could not download SHA256SUMS; skipping checksum verification of oaica-linux-amd64.tgz" "missing SHA256SUMS: warning"
assert_contains "$out" "CONTINUED" "missing SHA256SUMS: install continues"

# Neither sha256sum nor shasum available: warn and continue (even for a bad file).
rc=0; out=$(run_sh "available() { case \$1 in sha256sum|shasum) return 1 ;; esac; command -v \"\$1\" >/dev/null; }; fetch_archive '$BASE' corrupt-always.tgz \"\$TEMP_DIR/t.tgz\" && echo CONTINUED") || rc=$?
assert_eq "$rc" "0" "no sha256 tool: exit 0"
assert_contains "$out" "WARNING: Neither sha256sum nor shasum is available; skipping checksum verification of corrupt-always.tgz" "no sha256 tool: warning"
assert_contains "$out" "CONTINUED" "no sha256 tool: install continues"

# shasum -a 256 (macOS) is used when sha256sum is absent.
if command -v shasum >/dev/null; then
    rc=0; out=$(run_sh "available() { [ \"\$1\" = sha256sum ] && return 1; command -v \"\$1\" >/dev/null; }; fetch_archive '$BASE' oaica-linux-amd64.tgz \"\$TEMP_DIR/a.tgz\"") || rc=$?
    assert_eq "$rc" "0" "shasum fallback: exit 0"
    assert_contains "$out" "Checksum OK: oaica-linux-amd64.tgz" "shasum fallback: verified with shasum -a 256"
fi

# Full Linux download_and_extract: verified .tar.zst, and the .tgz fallback.
DAE="$(sed -n '/^download_and_extract() {/,/^}/p' "$INSTALL_SH")"
[ -n "$DAE" ] || { echo "download_and_extract not found in $INSTALL_SH" >&2; exit 1; }
printf '%s\n' "$DAE" > "$WORK/dae.sh"
if [ "$HAVE_ZSTD" -eq 1 ]; then
    rc=0; out=$(run_sh ". '$WORK/dae.sh'; mkdir -p \"\$TEMP_DIR/inst\"; download_and_extract '$BASE' \"\$TEMP_DIR/inst\" oaica-linux-amd64 && \"\$TEMP_DIR/inst/bin/oaica\"") || rc=$?
    assert_eq "$rc" "0" "download_and_extract zst: exit 0"
    assert_contains "$out" "Checksum OK: oaica-linux-amd64.tar.zst" "download_and_extract zst: verified before extraction"
    assert_contains "$out" "oaica-fake" "download_and_extract zst: extracted binary runs"
else
    echo "skip: zstd not installed, download_and_extract .tar.zst case not run"
fi
rc=0; out=$(run_sh ". '$WORK/dae.sh'; mkdir -p \"\$TEMP_DIR/inst\"; download_and_extract '$BASE' \"\$TEMP_DIR/inst\" oaica-linux-arm64 && \"\$TEMP_DIR/inst/bin/oaica\"") || rc=$?
assert_eq "$rc" "0" "download_and_extract tgz fallback: exit 0"
assert_contains "$out" "Downloading oaica-linux-arm64.tgz" "download_and_extract tgz fallback: took the tgz path"
assert_contains "$out" "Checksum OK: oaica-linux-arm64.tgz" "download_and_extract tgz fallback: verified before extraction"
assert_contains "$out" "oaica-fake" "download_and_extract tgz fallback: extracted binary runs"

###########################################
echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
