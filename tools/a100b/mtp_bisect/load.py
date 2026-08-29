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
import json
import random
import string
import sys
import threading
import time
import urllib.error
import urllib.request

MODEL = "oaica-35b-a3b-vision"
MAX_EST_TOKENS = 200_000  # chars/4 estimate ceiling per request

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


def build_messages(rng, system_prompt, is_long):
    messages = [{"role": "system", "content": system_prompt}]
    if is_long:
        target = rng.randint(120_000, 180_000)
    else:
        target = rng.randint(2_000, 8_000)

    # cap so total estimated tokens (chars/4) stays under MAX_EST_TOKENS,
    # leaving headroom for the system prompt + tool defs + response.
    budget_chars = MAX_EST_TOKENS * 4 - len(system_prompt) - 20_000
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
    return messages


class Stats:
    def __init__(self):
        self.lock = threading.Lock()
        self.requests = 0
        self.status_200 = 0
        self.status_other = {}
        self.conn_errors = 0
        self.engine_dead_signals = 0
        self.worker_had_success = {}

    def record(self, worker_id, status, conn_error):
        with self.lock:
            self.requests += 1
            had_success_before = self.worker_had_success.get(worker_id, False)
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

    def snapshot(self):
        with self.lock:
            return {
                "requests": self.requests,
                "status_200": self.status_200,
                "status_other": dict(self.status_other),
                "conn_errors": self.conn_errors,
                "engine_dead_signals": self.engine_dead_signals,
            }


def do_request(port, messages, tools, max_tokens, stream):
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
    req = urllib.request.Request(
        url, data=data, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            if stream:
                for line in resp:
                    if line.strip() == b"data: [DONE]":
                        break
            else:
                resp.read()
            return resp.getcode(), False
    except urllib.error.HTTPError as e:
        try:
            e.read()
        except Exception:
            pass
        return e.code, False
    except (urllib.error.URLError, ConnectionError, OSError, TimeoutError):
        return None, True


def worker(worker_id, port, seed, system_prompt, tools, stop_at, stats):
    rng = det_rng(seed, f"worker_{worker_id}_{time.time()}")
    while time.time() < stop_at:
        is_long = rng.random() < 0.6
        messages = build_messages(rng, system_prompt, is_long)
        max_tokens = rng.choice([64, 256, 1024, 4096])
        stream = rng.random() < 0.7
        status, conn_error = do_request(port, messages, tools, max_tokens, stream)
        stats.record(worker_id, status, conn_error)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--concurrency", type=int, default=8)
    ap.add_argument("--minutes", type=float, default=6)
    ap.add_argument("--seed", type=int, default=1234)
    args = ap.parse_args()

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
            args=(w, args.port, args.seed, system_prompt, tools, stop_at, stats),
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
