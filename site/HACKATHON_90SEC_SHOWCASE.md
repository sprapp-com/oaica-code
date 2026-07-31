# OAICA 90-second showcase plan (7-min hackathon slot)

Not published/linked from oaica.com — reference doc only.

## Constraint
90 seconds, must look alive/real (not slides) — terminal on screen, real
network calls, real GPU boxes. Everything below is proven working today,
nothing scripted/faked.

## Recommended running order (fits ~85s, leaves 5s buffer)

**1. Install, live (0:00–0:15)** — `curl -fsSL https://oaica.com/install.sh | bash`
One command, real binary, real download from our own Cloudflare Pages
domain. Proves it's a real shippable product, not a research demo.

**2. Smart routing, one line (0:15–0:30)** — `oaica run flashplan` then ask
something trivial ("hi") and something hard (a multi-step reasoning
question). Point out: same command, router silently picked the fast model
for the easy one and the reasoning model for the hard one — `flashplan`'s
per-request difficulty classifier, zero user config.

**3. Live multi-provider (0:30–0:45)** — `/model list` shows our own
fine-tuned models AND an external provider (`openrouter-gpt4o-mini`) side
by side through the same CLI, same auth, same interface. One line —
`/model openrouter-gpt4o-mini` — proves the router isn't locked to one
vendor.

**4. Stacked LoRA — the differentiator (0:45–1:15, the centerpiece)**
```
/lora list
/lora use medical_ms nutrition_en
What foods help lower blood pressure?
```
Real-time composition of two independently fine-tuned adapters on one base
model, per-request (not global — doesn't touch other users), verified to
produce a genuinely blended answer, not a coin-flip between the two. This
is the hardest-to-fake, most technically differentiated moment — most
teams show one model; showing two skills compose live is rare.

**5. Close (1:15–1:30)** — one sentence: "All of this — routing, LoRA
composition, multi-provider — runs through one Cloudflare Worker at
api.sprapp.com, backed by our own GPU boxes. No vendor lock-in, and it's
live right now at oaica.com."

## Cut list if time is tight
Drop step 3 (multi-provider) first — it's real but least visually
distinctive. Steps 1, 2, 4 are the load-bearing ones: install → routing →
stacking.

## What NOT to demo live (known issues, don't risk it on stage)
- The global `/lora add tool-caller` adapter — confirmed broken (garbage
  output), pulled from the registry already. Never mention it.
- Multi-replica concurrency scaling — walked back after a real correctness
  bug surfaced under `-np>1` + LoRA. Don't claim horizontal scaling live;
  the router code supports it structurally but it's unverified today.
- macOS/Windows install — binaries are hosted and cross-compile is fixed,
  but only Linux has been fully exercised via a real terminal end-to-end.
  If asked, say "cross-platform binaries are built and hosted," don't
  attempt a live macOS/Windows install unless you have that hardware in
  the room and tested it beforehand.

## Rehearsal note
Time steps 2 and 4 with a stopwatch beforehand — GPU cold-start / network
latency on a live demo box is the single biggest risk to the 90s budget.
If a step is running slow in rehearsal, have the previous output already
on screen as a fallback ("here's what I got a minute ago") rather than
stalling live.
