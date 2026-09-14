# Hosted model catalog

`models.json` is the canonical model catalog served to
`oaica model sync` (raw URL:
`https://raw.githubusercontent.com/sprapp-com/oaica-code/main/models/models.json`,
hardcoded as `defaultModelSyncURL` in `cmd/launch/model_sync.go`).

Same JSON shape as `~/.oaica/models.json` (`modelManifest`, version 1).
`oaica model sync` upserts its entries into each user's local manifest —
edit this file + push to main, users run `oaica model sync` (or it runs
via `oaica model list`'s auto-scan of local dirs, plus this being fetched
by sync), and no reinstall or Go-code change is ever needed for a model
update.

Rules:
- Remote config wins over local; local `notes` survive unless the entry
  here ships its own.
- `oaica model sync --prune` removes sync-sourced entries that dropped
  out of this file. Hand-added and locally-scanned entries are never
  pruned.
- Entries failing validation are skipped (reported as `! id: reason`),
  so a bad push can't corrupt anyone's manifest.