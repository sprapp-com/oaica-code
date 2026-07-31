# DARE-TIES Merge Ablation — Findings for Hackathon Reference

Not published/linked from oaica.com — reference doc only.

All numbers below are real BFCL results pulled directly from
`/mnt/raid0/ornith_merge/*.json` on bitdeer (2026-07-31), not re-estimated
or rounded from memory. Full per-example generations are in the
corresponding `.json` files; subset breakdowns are in each result file's
`per_subset`.

## The core ablation: how many models to DARE-TIES merge?

Base: Qwen3.5-35B-A3B. Merge method: DARE-TIES (mergekit). Eval: BFCL,
1240 total examples across 5 subsets (simple, multiple, parallel,
parallel_multiple, irrelevance).

| Merge | Components | BFCL overall | simple | multiple | parallel | parallel_multiple | irrelevance |
|---|---|---|---|---|---|---|---|
| **4-way (winner)** | Ornith + Agents-A1 | **85.5%** (1060/1240) | 92.25% | 91.5% | 86.0% | 66.5% | 84.6% |
| 5-way | Ornith + Agents-A1 + AgentWorld | 81.4% (1009/1240) | 92.75% | 88.5% | 78.5% | 52.5% | 82.9% |
| Δ (4-way vs 5-way) | — | **+4.1 pts** | −0.5 | +3.0 | +7.5 | **+14.0** | +1.7 |

**Finding: adding a 3rd component (AgentWorld) hurt, it didn't help.**
The 5-way merge lost ground on every subset except `simple` (roughly flat)
— the steepest drop is `parallel_multiple` (−14 points), the hardest
subset (multiple simultaneous function calls). This is the clearest
evidence in the whole ablation: more merge components ≠ better. Two
well-chosen components (Ornith's tool-calling strength + Agents-A1's
agentic behavior) combined better than the same two plus a third.

## Reference points (how far merging moved the needle)

| Model | BFCL overall | Note |
|---|---|---|
| **4-way DARE merge (winner, above)** | **85.5%** | The shipped result |
| Nanbeige base (unmerged) | 44.1% | A different base model family, not a merge input — shown here only as "what an unmerged/untuned base looks like" on this exact eval, not an apples-to-apples ablation arm |
| Malay-SFT-only checkpoint (nb3b winner) | 19.5% | The Malay-language-tuned checkpoint (separate track, see `STACKED_LORA_DEMO_RANKED.md`/oaica-malay-3b) scored on the SAME BFCL harness — confirms it's a language-quality model, not tool-calling, exactly as already documented for that model's ★★★☆☆ rating |

**Honest caveat on the reference points:** these two aren't merge-ablation
arms (they're not DARE-TIES inputs or outputs) — they're included only to
show the scale of the gap between "a model tuned for something else
entirely" and "a merge tuned for BFCL." Don't cite them as "merging beat
these by 40-65 points" — that would misrepresent what was actually
compared.

## What wasn't re-verified this pass

An earlier session (2026-07-27) recorded "MTP +15-16%" for this same merge
work (see project memory `project_ornith_dare_merge_2026-07-27`) — that
number was not independently re-derived from a log file in this pass
(searched `/mnt/raid0/ornith_merge/mtp_test.log` and related files; no
clean before/after MTP-speedup numbers were found there to re-cite). If
that stat is needed for the presentation, it should be re-pulled from
whatever log/notebook originally produced it rather than repeated
secondhand here.

## Recommended framing for the presentation

Lead with the 4-way vs 5-way comparison — it's a real, clean ablation
(same base, same eval, one variable changed: number of merged components)
and the finding ("more isn't better — 2 well-chosen models beat 3") is a
genuinely interesting, defensible claim. Don't lean on the Nanbeige-base
or Malay-SFT reference rows as ablation evidence; use them only if asked
"how much does merging actually help vs a random baseline," with the
caveat stated above.
