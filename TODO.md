# TODO — oaica (as of 2026-08-26, evening)

Status: CLI 0.4.1 live at oaica.com (laptop upgraded); api.oaica.com live and
re-verified after today's control-plane window; site builder shipped;
cross-source Claude Code tiers shipped; OpenRouter form answers verified
(`docs/OPENROUTER_FORM_ANSWERS.md`). GitHub Actions now runs on the fork.

## You (human) — before submitting the OpenRouter form

- [ ] Confirm `oaica@sprapp.com` exists and is read. OpenRouter emails it and
      invites it to a Slack Connect channel; `/privacy` and `/terms` tell
      customers to write to it.
- [x] `/models` auth — public since 2026-08-26 (200 without a key);
      completions stay key-protected. Nothing to note in the form.
- [ ] Uptime monitor: UptimeRobot / Better Stack (free) on
      `https://api.oaica.com/health`, interval >= 60 s. Paste its public
      status URL into the form if asked, and send it to me for `/status`.
- [ ] Fill the form from `docs/OPENROUTER_FORM_ANSWERS.md`, row by row.
      Key for the API credential: `~/.secrets/oaica_openrouter_key` (laptop).
- [ ] Same OpenRouter account: toggle the privacy/data-policy setting only
      if you also want to *consume* stealth/free models (unrelated to listing).

## You — after OpenRouter has the key

- [ ] If the key was handed over anywhere other than the form itself, ask
      for a rotation (runbook: "Rotating the OpenRouter key" in
      `docs/OPENROUTER_PROVIDER.md`, zero downtime).

## Housekeeping — done

- [x] GitHub Actions on the fork: live. The release workflow built and
      verified 0.4.1 on a runner (it only failed at "create release" because
      the release already existed; the step is idempotent now). Upstream's
      `latest.yaml` disabled.
- [x] Laptop: 0.4.1 installed to `~/.local/bin/oaica` (precedes the stale
      0.0.0 dev build in /usr/local/bin on your PATH).
- [x] Demo Pages project `oaica-site-demo` deleted.

## Releases

- [x] 0.4.1 shipped 2026-08-26 (oaica.com + GitHub): fixes the fresh-install
      `launch claude --model kat-awq` failure; adds cross-source
      `--sonnet-model` tiers (docs/CLAUDE_TIERS.md). All four combos verified
      live with the downloaded binary.
- [ ] Versioning: stay on 0.x; cut **1.0.0** as the official-launch marker
      when you announce. (Ollama itself has stayed 0.x for years; our
      `oaica-v*` tags are independent of upstream's.)

## a100b control plane — done 2026-08-26

- [x] Gateway upstream key (gatekeeper `openrouter` tier) rotated; old key
      retired. Procedure in `tools/a100b/README.md`.
- [x] gatekeeper moved under `/workspace` + `stack_watchdog.sh`; legacy
      `/root/gatekeeper_watchdog.sh` loops stopped, script disabled.
- [x] katlb config pruned to the two live replicas (30199, 30105).
- [x] Monthly ledger rotation (`/workspace/ledger-rotate.sh`, cron
      `5 0 1 * *`); laptop pull covers rotated files.
- [x] `/dev/shm/gpus.md` section updated (GPU0/GPU2 kat-awq, GPU5
      malay35b-offload, kat-vl-mtp retired).

## Code items — in flight (worktree agents, see this file's next revision)

- [ ] `install.sh`: verify `SHA256SUMS` + retry (one truncated CDN download
      in three seen).
- [ ] katlb: hung-but-listening replica detection (`stall_sec`).
- [ ] Slow `cmd/launch` tests (78 s / 12 s ones) — stub the network.
- [ ] Stale upstream `recommendedModels` catalog out of the picker.

## Blocked

- [ ] kat-vl-mtp relaunch: no GPU on a100b has >= 75 GB free (all 52-75 GB
      used by other sessions' jobs). Relaunch with
      `/root/vllm_vlmtp_watchdog.sh` when one frees.
- [ ] Second region / failover: none; `/status` says so. Needs a second box.
