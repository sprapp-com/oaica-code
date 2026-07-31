# Stacked LoRA — ranked demo results (for presentation reference)

Not published/linked from oaica.com — reference doc only.

All tests real, run via `oaica-code` CLI → `api.sprapp.com` router → bitdeer
llama-server (Qwen3.5-0.8B base + up to 9 verticals loaded simultaneously,
`--lora-init-without-apply` × 9, toggled per-request via the new
`/lora use`/`/lora stack` client-local mechanism — zero effect on other
users). Base model per-adapter: `fitness_en_cpu_v2` etc, ~100-250 training
examples each (demo-quality, not production-hardened — see
`INDUSTRY_LORA_SUBSCRIPTION_DEMO.md`).

## Ranked by demo impact (most → least impressive)

### 1. medical_ms + nutrition_en — best overall
Prompt: *"What foods help lower blood pressure?"*
> Avoid processed meats, excessive salt and sugar. Practice regular exercise
> to maintain healthy weight. Drink plenty of water daily for better
> hydration support your cardiovascular health naturally without
> unnecessary sodium intake. Avoid alcohol consumption entirely as it can
> raise heart rate temporarily during intense activity or increase stress
> levels that may affect blood pressure control over time...

**Why it's #1:** longest, most coherent, genuinely blends medical caution
framing (medical_ms) with concrete nutrition specifics (nutrition_en) —
reads like a single domain expert wrote it, not two adapters awkwardly
glued together. Best "wow, that's really combining two trained skills"
moment.

### 2. career_coach_en + personal_finance_en — cleanest blend
Prompt: *"I want to switch careers but I'm worried about money. What should
I do?"*
> Start by researching salary ranges for the new field and identify
> employers that value your skills over experience level... Network with
> professionals... Consider starting a side project to build credibility
> while maintaining financial stability through part-time work during
> interviews.

**Why it's #2:** directly relatable use case (career + money = real
subscription-app scenario), clean actionable structure, obviously pulling
from both training sets (salary research = career, "financial stability" /
"side project for income" = finance).

### 3. fitness_en + career_coach_en — best soundbite (short, punchy)
Prompt: *"How do I stay motivated during a tough week?"*
> Pick one small task that doesn't require much energy. Focus on progress
> rather than perfection, and celebrate every step forward to build
> momentum immediately after the slump hits hardest.

**Why it's #3:** short enough to read live on stage in ~5 seconds, still
clearly blends fitness "small task / momentum" framing with career-coach
motivational tone. Best choice if the demo needs to be fast.

### 4. legal_ms + agriculture_ms — real but weaker signal
Prompt: *"Apakah hak petani jika tanah mereka diambil kerajaan?"* (What
rights do farmers have if their land is taken by the government?)
> Bagi rakyat Malaysia, tiada sistem yang perlu diubah. Kita boleh
> mensasarkan pengurusan lebih baik dengan mematuhi undang-undang dan
> menjaga kesihatan bumi kita...

Coherent Malay, on-topic (law + land), but doesn't show deep
agriculture-specific knowledge — reads more legal-flavored than a clear
50/50 blend. Usable but not the strongest evidence of composition.

### 5. tourism_ms + language_tutor_en — weakest signal (not recommended for demo)
Prompt: *"Cadangkan tempat menarik di Melaka untuk pelancong."*
> Tempat seperti Batu Caves atau Sentul akan menawarkan pengalaman unik dan
> bersejarah yang pasti mengejutkan bagi anda!

Correct-ish tourism answer, but no visible language-tutor influence
(response is Malay-only, doesn't demonstrate the tutor adapter doing
anything distinguishable). Skip this pairing for the demo — pick #1 or #2
instead.

## Recommended for the 90-second showcase
**#1 (medical_ms + nutrition_en) or #2 (career_coach_en +
personal_finance_en)** — both fully coherent, clearly blended, and land in
one screen of text. Pair with the `/lora list` → `/lora use` → response →
`/lora stack` → response sequence to visually show composition happening
live, not pre-scripted.
