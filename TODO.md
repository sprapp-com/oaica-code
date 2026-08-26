# TODO — oaica (as of 2026-08-26)

Status: core CLI 0.4.0 live at oaica.com; public API api.oaica.com live and
verified; site builder shipped; OpenRouter form answers verified
(`docs/OPENROUTER_FORM_ANSWERS.md`). What remains is split by who can do it.

## You (human) — before submitting the OpenRouter form

- [ ] Confirm `oaica@sprapp.com` exists and is read. OpenRouter emails it and
      invites it to a Slack Connect channel; `/privacy` and `/terms` tell
      customers to write to it.
- [ ] Decide `/models` auth. Today it needs the key (401 without). Either
      write "poll /models with the provided key" in the form notes, or say
      "make /models public" (one line + SIGHUP; listing is public data —
      recommended).
- [ ] Uptime monitor: UptimeRobot / Better Stack (free) on
      `https://api.oaica.com/health`, interval >= 60 s. Paste its public
      status URL into the form if asked, and send it to me for `/status`.
- [ ] Fill the form from `docs/OPENROUTER_FORM_ANSWERS.md`, row by row.
      Key for the `/models` field: `~/.secrets/oaica_openrouter_key` (laptop).
- [ ] Same OpenRouter account: toggle the privacy/data-policy setting only
      if you also want to *consume* stealth/free models (unrelated to listing).

## You — after OpenRouter has the key

- [ ] If the key was handed over anywhere other than the form itself, ask
      for a rotation (runbook: "Rotating the OpenRouter key" in
      `docs/OPENROUTER_PROVIDER.md`, zero downtime).

## You — housekeeping (1 min each, any time)

- [ ] GitHub -> repo -> Actions tab -> "enable workflows". Repo is a fork;
      until this is clicked releases stay manual (`docs/RELEASE.md` §3b).
- [ ] Laptop upgrade: `curl -fsSL https://oaica.com/install.sh | bash`
      (sudo) -> 0.4.0 with `oaica site`.
- [ ] `wrangler pages project delete oaica-site-demo` when done with the demo.

## Me (Claude) — on request, each needs a short 503 window

- [ ] Rotate the gatekeeper upstream key on a100b (it sat in a
      group-readable script until today; other accounts on the box are
      yours, low risk). Touches `/root/gatekeeper.json`,
      `/workspace/gateway_upstream.key`, restarts gatekeeper + gateway.
- [ ] Move gatekeeper under `stack_watchdog.sh` (still launched by the
      legacy `/root/gatekeeper_watchdog.sh` x2); binary + config to
      `/workspace`.
- [ ] Ledger monthly rotation (`ledger.YYYY-MM` + SIGHUP); backup pull to
      the laptop already runs every 30 min.

## Me — low priority (not blocking sale)

- [ ] Dead katlb backends 30101-30104 still in `katlb-kat-awq.json`
      (marked down by the probe; remove for clarity).
- [ ] Hung-but-listening replica detection (probe timeout is the only guard).
- [ ] Stale hardcoded `recommendedModels` in `cmd/launch/models.go`.
- [ ] Cloudflare Browser Integrity Check on the oaica.com zone may 403
      non-browser clients on the landing page (api.oaica.com verified fine).
- [ ] Slow tests: `TestRemoteRoutingHelpers` 78 s + several 12 s (pre-existing).
- [ ] Relaunch kat-vl-mtp when a GPU frees (`/root/vllm_vlmtp_watchdog.sh`).
- [ ] Second region / failover — none today; `/status` says so.

## Done today (for the record)

- 0.3.0 + 0.4.0 released (oaica.com + GitHub); default router api.oaica.com;
  auth-guard errors; reproducible builds; `oaica site` v1.
- Gateway: non-stream clamp 8192 -> 4096 (real 504s), cached `/health`,
  PRIVACY wording, contact email, Website https://oaica.com/.
- Box: swap scripts 0700 without key literals, v1 config deleted,
  cloudflared supervised, `@reboot` cron, ledger backup cron on laptop.
- Verified live: tools, `response_format`, `stop`, image refusal, licences.
