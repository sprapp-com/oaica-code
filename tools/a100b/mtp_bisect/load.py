#!/usr/bin/env python3
"""
load.py -- Claude Code-shaped traffic generator for MTP crash-repro testing.
stdlib only (urllib/threading/json). No pip deps.

Usage:
  python3 load.py --port 30130 --concurrency 8 --minutes 6 --seed 1234

Simulates concurrent chat-completion traffic against a local vLLM instance:
  - a shared ~25k-token (~100KB) deterministic pseudo-random system prompt,
    reused verbatim by every request so prefix caching is exercised
  - a ~40-entry `tools` array with nested JSON schemas, also shared/reused
  - per request: 60% "long" (append ~120k-180k chars of synthetic
    conversation history), 40% "short" (~2-8k chars)
  - random max_tokens in {64, 256, 1024, 4096}
  - 70% stream=true (consumed fully via SSE), 30% non-stream
  - temperature 1.0, top_p 0.95
  - total estimated tokens (chars/4) kept under 200k per request

Tracks: total requests, 200s, non-200s by status, connection errors, and
"engine dead" signals (HTTP 500, or a connection error/refused that follows
a prior successful request on this worker). Prints a summary line every 30s
and a final JSON summary. Exit code 2 if any engine-death signal was seen,
0 otherwise.
"""
import argparse
import base64
import json
import random
import socket
import string
import struct
import sys
import threading
import time
import urllib.error
import urllib.request
import zlib

MODEL = "oaica-35b-a3b-vision"
MAX_EST_TOKENS = 200_000  # chars/4 estimate ceiling per request
IMAGE_TOKEN_EST = 1500  # rough chars/4-equivalent budget cost per image

CODE_WORDS = [
    "function", "return", "const", "let", "import", "export", "async", "await",
    "class", "interface", "struct", "impl", "fn", "pub", "match", "if", "else",
    "for", "while", "def", "self", "None", "True", "False", "try", "except",
    "catch", "throw", "new", "this", "super", "extends", "implements",
    "public", "private", "static", "void", "int", "string", "bool", "Vec",
    "HashMap", "Option", "Result", "Ok", "Err", "println", "console", "log",
]
PROSE_WORDS = [
    "the", "system", "should", "handle", "edge", "cases", "carefully", "when",
    "processing", "user", "requests", "across", "distributed", "services",
    "ensure", "consistency", "latency", "throughput", "review", "the", "diff",
    "and", "summarize", "findings", "in", "a", "concise", "report", "before",
    "committing", "any", "changes", "to", "the", "repository", "please",
]


def det_rng(seed, salt):
    """Deterministic per-purpose RNG derived from the base seed + salt string."""
    r = random.Random()
    r.seed(f"{seed}:{salt}")
    return r


def gen_text_block(rng, target_chars, code_ratio=0.5):
    """Deterministic pseudo-random English+code text of roughly target_chars."""
    out = []
    n = 0
    while n < target_chars:
        if rng.random() < code_ratio:
            line_len = rng.randint(3, 12)
            words = [rng.choice(CODE_WORDS) for _ in range(line_len)]
            line = " ".join(words) + rng.choice([";", " {", " }", "()", ":"])
        else:
            line_len = rng.randint(5, 20)
            words = [rng.choice(PROSE_WORDS) for _ in range(line_len)]
            line = " ".join(words) + "."
        out.append(line)
        n += len(line) + 1
    return "\n".join(out)[:target_chars]


def gen_png_bytes(rng, width, height):
    """Generate a valid 8-bit RGB PNG filled with a deterministic
    gradient/stripe pattern, using only zlib+struct (pure stdlib)."""
    xf = rng.randint(1, 5)
    yf = rng.randint(1, 5)
    stripe = rng.randint(8, 32)
    ro = rng.randint(0, 255)
    go = rng.randint(0, 255)
    bo = rng.randint(0, 255)
    raw = bytearray()
    for y in range(height):
        raw.append(0)  # filter type 0 (None) for this scanline
        for x in range(width):
            r = (x * xf + ro) % 256
            g = (y * yf + go) % 256
            b = (((x + y) // stripe) * 37 + bo) % 256
            raw += bytes((r, g, b))

    def chunk(tag, data):
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    sig = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    idat = zlib.compress(bytes(raw), 6)
    png = sig + chunk(b"IHDR", ihdr) + chunk(b"IDAT", idat) + chunk(b"IEND", b"")
    return png


def gen_image_data_url(rng):
    size = rng.choice([(256, 256), (512, 384), (1024, 768)])
    png = gen_png_bytes(rng, *size)
    b64 = base64.b64encode(png).decode("ascii")
    return f"data:image/png;base64,{b64}"


def build_shared_system_prompt(seed):
    """~25k-token (~100KB) deterministic system prompt, built once and reused
    verbatim by every request (prefix caching)."""
    rng = det_rng(seed, "system_prompt")
    return gen_text_block(rng, 100_000, code_ratio=0.6)


def build_shared_tools(seed):
    """~40 function tools with nested JSON schemas, built once and reused."""
    rng = det_rng(seed, "tools")
    tools = []
    for i in range(40):
        nprops = rng.randint(2, 6)
        props = {}
        for j in range(nprops):
            props[f"param_{j}"] = {
                "type": rng.choice(["string", "integer", "boolean", "object", "array"]),
                "description": gen_text_block(rng, 60, code_ratio=0.1),
            }
        tools.append({
            "type": "function",
            "function": {
                "name": f"tool_{i}_{''.join(rng.choice(string.ascii_lowercase) for _ in range(6))}",
                "description": gen_text_block(rng, 120, code_ratio=0.2),
                "parameters": {
                    "type": "object",
                    "properties": props,
                    "required": list(props.keys())[: max(1, nprops // 2)],
                },
            },
        })
    return tools


def build_messages(rng, system_prompt, is_long, min_history_chars, max_history_chars,
                    attach_image=False):
    messages = [{"role": "system", "content": system_prompt}]
    if is_long:
        target = rng.randint(min_history_chars, max_history_chars)
    else:
        target = rng.randint(2_000, 8_000)

    n_images = rng.choice([1, 2]) if attach_image else 0

    # cap so total estimated tokens (chars/4) stays under MAX_EST_TOKENS,
    # leaving headroom for the system prompt + tool defs + response, and
    # for any attached images (charged at IMAGE_TOKEN_EST*4 chars-equiv each).
    budget_chars = (
        MAX_EST_TOKENS * 4 - len(system_prompt) - 20_000 - n_images * IMAGE_TOKEN_EST * 4
    )
    if target > budget_chars:
        target = max(1000, budget_chars)

    # split history into alternating user/assistant turns of mixed
    # code/JSON/prose content
    remaining = target
    turn_role = "user"
    while remaining > 0:
        chunk = min(remaining, rng.randint(500, 4000))
        content = gen_text_block(rng, chunk, code_ratio=rng.choice([0.2, 0.5, 0.8]))
        messages.append({"role": turn_role, "content": content})
        remaining -= chunk
        turn_role = "assistant" if turn_role == "user" else "user"

    if messages[-1]["role"] != "user":
        messages.append({"role": "user", "content": gen_text_block(rng, 200, code_ratio=0.3)})

    if n_images:
        last = messages[-1]
        text = last["content"]
        content_list = [{"type": "text", "text": text}]
        for _ in range(n_images):
            content_list.append({
                "type": "image_url",
                "image_url": {"url": gen_image_data_url(rng)},
            })
        last["content"] = content_list

    return messages, n_images


class Stats:
    def __init__(self):
        self.lock = threading.Lock()
        self.requests = 0
        self.status_200 = 0
        self.status_other = {}
        self.conn_errors = 0
        self.engine_dead_signals = 0
        self.worker_had_success = {}
        self.aborted = 0
        self.with_images = 0
        self.sessions_started = 0
        self.turns = 0

    def record_session_started(self):
        with self.lock:
            self.sessions_started += 1

    def record_turn(self):
        with self.lock:
            self.turns += 1

    def record(self, worker_id, status, conn_error, aborted=False):
        with self.lock:
            self.requests += 1
            had_success_before = self.worker_had_success.get(worker_id, False)
            if aborted:
                self.aborted += 1
                # aborts are intentional client-side disconnects: not errors,
                # not engine-death signals, but still count as a "success"
                # for engine-death tracking purposes on this worker.
                self.worker_had_success[worker_id] = True
                return
            if conn_error:
                self.conn_errors += 1
                if had_success_before:
                    self.engine_dead_signals += 1
            elif status == 200:
                self.status_200 += 1
                self.worker_had_success[worker_id] = True
            else:
                self.status_other[status] = self.status_other.get(status, 0) + 1
                if status == 500:
                    self.engine_dead_signals += 1

    def record_image(self):
        with self.lock:
            self.with_images += 1

    def snapshot(self):
        with self.lock:
            return {
                "requests": self.requests,
                "status_200": self.status_200,
                "status_other": dict(self.status_other),
                "conn_errors": self.conn_errors,
                "engine_dead_signals": self.engine_dead_signals,
                "aborted": self.aborted,
                "with_images": self.with_images,
                "sessions_started": self.sessions_started,
                "turns": self.turns,
            }


def do_request(port, messages, tools, max_tokens, stream, abort_plan=None, extra_headers=None):
    """abort_plan is None for normal requests, or a dict describing how to
    abort a streaming request early:
      {"mode": "chunks", "n": <int>}          -- close after n SSE chunks
      {"mode": "delay", "seconds": <float>}    -- close after a delay
      {"mode": "prefill", "timeout": <float>}  -- close if no data arrives
                                                    within `timeout` (client
                                                    gives up during prefill)
    Returns (status, conn_error, aborted, content) where content is the
    concatenated assistant reply text actually received (best-effort; may be
    partial for aborted requests, empty string if none was captured).
    """
    url = f"http://127.0.0.1:{port}/v1/chat/completions"
    payload = {
        "model": MODEL,
        "messages": messages,
        "tools": tools,
        "max_tokens": max_tokens,
        "temperature": 1.0,
        "top_p": 0.95,
        "stream": stream,
    }
    data = json.dumps(payload).encode("utf-8")
    headers = {"Content-Type": "application/json"}
    if extra_headers:
        headers.update(extra_headers)
    req = urllib.request.Request(url, data=data, headers=headers, method="POST")

    def extract_stream_content(line):
        try:
            raw = line.strip()
            if not raw.startswith(b"data:"):
                return ""
            raw = raw[len(b"data:"):].strip()
            if raw == b"[DONE]" or not raw:
                return ""
            obj = json.loads(raw)
            delta = obj.get("choices", [{}])[0].get("delta", {})
            return delta.get("content") or ""
        except Exception:
            return ""

    try:
        if abort_plan is not None and stream:
            if abort_plan["mode"] == "prefill":
                resp = urllib.request.urlopen(req, timeout=abort_plan["timeout"])
                content_parts = []
                try:
                    try:
                        resp.fp.raw._sock.settimeout(abort_plan["timeout"])
                    except Exception:
                        pass
                    try:
                        line = next(iter(resp))
                        content_parts.append(extract_stream_content(line))
                    except (socket.timeout, StopIteration, OSError):
                        pass
                finally:
                    resp.close()
                return None, False, True, "".join(content_parts)
            with urllib.request.urlopen(req, timeout=300) as resp:
                content_parts = []
                if abort_plan["mode"] == "chunks":
                    n = abort_plan["n"]
                    count = 0
                    for line in resp:
                        content_parts.append(extract_stream_content(line))
                        count += 1
                        if count >= n:
                            break
                else:  # "delay"
                    deadline = time.time() + abort_plan["seconds"]
                    for line in resp:
                        content_parts.append(extract_stream_content(line))
                        if time.time() >= deadline:
                            break
                # deliberately do not drain the rest of the response;
                # closing here (via context manager exit) aborts the conn.
                return None, False, True, "".join(content_parts)

        with urllib.request.urlopen(req, timeout=300) as resp:
            if stream:
                content_parts = []
                for line in resp:
                    if line.strip() == b"data: [DONE]":
                        break
                    content_parts.append(extract_stream_content(line))
                return resp.getcode(), False, False, "".join(content_parts)
            else:
                body = resp.read()
                content = ""
                try:
                    obj = json.loads(body)
                    content = obj.get("choices", [{}])[0].get("message", {}).get("content") or ""
                except Exception:
                    pass
                return resp.getcode(), False, False, content
    except urllib.error.HTTPError as e:
        try:
            e.read()
        except Exception:
            pass
        return e.code, False, False, ""
    except (urllib.error.URLError, ConnectionError, OSError, TimeoutError):
        return None, True, False, ""


def _independent_worker(worker_id, port, seed, system_prompt, tools, stop_at, stats, args):
    rng = det_rng(seed, f"worker_{worker_id}_{time.time()}")
    max_tokens_choices = args.max_tokens_choices
    while time.time() < stop_at:
        is_long = rng.random() < args.long_frac
        attach_image = rng.random() < args.image_frac
        messages, n_images = build_messages(
            rng, system_prompt, is_long, args.min_history_chars, args.max_history_chars,
            attach_image=attach_image,
        )
        max_tokens = rng.choice(max_tokens_choices)
        stream = rng.random() < 0.7

        abort_plan = None
        if stream and rng.random() < args.abort_frac:
            if rng.random() < (1.0 / 3.0):
                abort_plan = {"mode": "prefill", "timeout": rng.uniform(0.05, 2.0)}
            elif rng.random() < 0.5:
                abort_plan = {"mode": "chunks", "n": rng.randint(1, 8)}
            else:
                abort_plan = {"mode": "delay", "seconds": rng.uniform(0.5, 6.0)}

        status, conn_error, aborted, _content = do_request(
            port, messages, tools, max_tokens, stream, abort_plan=abort_plan
        )
        stats.record(worker_id, status, conn_error, aborted=aborted)
        if n_images:
            stats.record_image()


def session_turn_chars(rng, base_chars):
    """~base_chars, jittered +/-50%; 20% of the time a tiny quick-followup."""
    if rng.random() < 0.2:
        return rng.randint(50, 200)
    lo = max(1, int(base_chars * 0.5))
    hi = int(base_chars * 1.5)
    return rng.randint(lo, hi)


def estimated_tokens(system_prompt, tools, messages):
    body_chars = len(system_prompt) + len(json.dumps(tools))
    for m in messages[1:]:
        content = m.get("content")
        if isinstance(content, str):
            body_chars += len(content)
        elif isinstance(content, list):
            for part in content:
                if part.get("type") == "text":
                    body_chars += len(part.get("text", ""))
                elif part.get("type") == "image_url":
                    body_chars += IMAGE_TOKEN_EST * 4
    return body_chars // 4


def session_worker(worker_id, port, seed, system_prompt, tools, stop_at, stats, args):
    max_tokens_choices = args.max_tokens_choices
    session_n = 0
    session_seed_offset = 0
    while time.time() < stop_at:
        session_n += 1
        stats.record_session_started()
        rng = det_rng(seed, f"session_worker_{worker_id}_{session_seed_offset}_{time.time()}")
        session_seed_offset += 1

        start_text = gen_text_block(rng, args.session_start_chars, code_ratio=rng.choice([0.2, 0.5, 0.8]))
        messages = [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": start_text},
        ]

        while time.time() < stop_at:
            max_tokens = rng.choice(max_tokens_choices)
            stream = rng.random() < 0.7

            abort_plan = None
            if stream and rng.random() < args.abort_frac:
                if rng.random() < (1.0 / 3.0):
                    abort_plan = {"mode": "prefill", "timeout": rng.uniform(0.05, 2.0)}
                elif rng.random() < 0.5:
                    abort_plan = {"mode": "chunks", "n": rng.randint(1, 8)}
                else:
                    abort_plan = {"mode": "delay", "seconds": rng.uniform(0.5, 6.0)}

            headers = {"X-Session-Id": f"sess-{worker_id}-{session_n}"}
            status, conn_error, aborted, content = do_request(
                port, messages, tools, max_tokens, stream,
                abort_plan=abort_plan, extra_headers=headers,
            )
            stats.record(worker_id, status, conn_error, aborted=aborted)
            stats.record_turn()

            # Images are only sent on the turn they were attached to: the
            # server caps images per request (--limit-mm-per-prompt image=2),
            # so leaving them in the growing history 400s every later turn of
            # the session (seen: 2516 x HTTP 400 in one run). Keep the text.
            last = messages[-1]
            if isinstance(last.get("content"), list):
                last["content"] = " ".join(
                    p.get("text", "") for p in last["content"] if p.get("type") == "text"
                )

            if aborted or conn_error or status != 200:
                # Keep user/assistant alternation intact even when the turn
                # failed or was aborted (Claude Code does the same: the
                # partial/failed reply still occupies the assistant slot).
                messages.append({"role": "assistant", "content": "(turn aborted)"})
            else:
                assistant_text = content if content else "(no response captured)"
                messages.append({"role": "assistant", "content": assistant_text})

            turn_chars = session_turn_chars(rng, args.session_turn_chars)
            attach_image = rng.random() < args.image_frac
            new_user = {"role": "user", "content": gen_text_block(rng, turn_chars, code_ratio=rng.choice([0.2, 0.5, 0.8]))}
            if attach_image:
                n_images = rng.choice([1, 2])
                content_list = [{"type": "text", "text": new_user["content"]}]
                for _ in range(n_images):
                    content_list.append({
                        "type": "image_url",
                        "image_url": {"url": gen_image_data_url(rng)},
                    })
                new_user["content"] = content_list
                stats.record_image()
            messages.append(new_user)

            if estimated_tokens(system_prompt, tools, messages) > args.session_max_tokens:
                break


def worker(worker_id, port, seed, system_prompt, tools, stop_at, stats, args):
    if args.session_mode:
        session_worker(worker_id, port, seed, system_prompt, tools, stop_at, stats, args)
        return
    _independent_worker(worker_id, port, seed, system_prompt, tools, stop_at, stats, args)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--concurrency", type=int, default=8)
    ap.add_argument("--minutes", type=float, default=6)
    ap.add_argument("--seed", type=int, default=1234)
    ap.add_argument("--abort-frac", type=float, default=0.0,
                     help="fraction of streaming requests to abort early (close conn without draining)")
    ap.add_argument("--image-frac", type=float, default=0.0,
                     help="fraction of requests that attach 1-2 generated PNG images (OpenAI vision format)")
    ap.add_argument("--max-tokens-choices", type=str, default="64,256,1024,4096",
                     help="comma-separated max_tokens choices")
    ap.add_argument("--long-frac", type=float, default=0.6,
                     help="fraction of requests using the long history variant")
    ap.add_argument("--min-history-chars", type=int, default=120_000)
    ap.add_argument("--max-history-chars", type=int, default=180_000)
    ap.add_argument("--session-mode", action="store_true",
                     help="each worker simulates one long-lived Claude-Code-style "
                          "session (full history resent each turn) instead of "
                          "independent requests")
    ap.add_argument("--session-start-chars", type=int, default=40_000,
                     help="approx chars of the initial user turn in session mode")
    ap.add_argument("--session-turn-chars", type=int, default=6_000,
                     help="approx chars of each new user turn in session mode "
                          "(jittered +/-50%%, occasionally tiny)")
    ap.add_argument("--session-max-tokens", type=int, default=190_000,
                     help="estimated total tokens (chars/4) at which a session "
                          "worker starts a fresh session")
    args = ap.parse_args()
    args.max_tokens_choices = [int(x) for x in args.max_tokens_choices.split(",") if x.strip()]

    print(f"building shared system prompt + tools (seed={args.seed})...", file=sys.stderr)
    system_prompt = build_shared_system_prompt(args.seed)
    tools = build_shared_tools(args.seed)
    print(
        f"system_prompt ~{len(system_prompt)} chars, {len(tools)} tools",
        file=sys.stderr,
    )

    stats = Stats()
    stop_at = time.time() + args.minutes * 60

    threads = []
    for w in range(args.concurrency):
        t = threading.Thread(
            target=worker,
            args=(w, args.port, args.seed, system_prompt, tools, stop_at, stats, args),
            daemon=True,
        )
        t.start()
        threads.append(t)

    start = time.time()
    next_report = start + 30
    while time.time() < stop_at:
        time.sleep(1)
        if time.time() >= next_report:
            snap = stats.snapshot()
            elapsed = int(time.time() - start)
            print(f"[{elapsed}s] {json.dumps(snap)}", flush=True)
            next_report += 30

    for t in threads:
        t.join(timeout=max(1, stop_at + 300 - time.time()))

    final = stats.snapshot()
    print(json.dumps(final, indent=2))

    sys.exit(2 if final["engine_dead_signals"] > 0 else 0)


if __name__ == "__main__":
    main()
