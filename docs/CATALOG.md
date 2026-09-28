# The model catalog

The picker's provider and model list is built from [models.dev](https://models.dev),
the same source opencode reads (MIT, maintained by sst). Porting it means a new
provider, a new model, a revised context window or a price change reaches the
picker without a code change here — which is the whole point: the alternative is
editing a JSON file in the tree for every vendor that ships a model.

## The three layers

Lowest precedence first. Each layer corrects the one above it, per field:

1. **The ported catalog** — the synced models.dev payload, at
   `~/.oaica/cache/catalog/modelsdev.json`. It is never edited: it is upstream's
   file, a snapshot of what vendors serve at the moment it was fetched.
2. **The embedded overlay** — `cmd/launch/providers/oaica.json`, compiled into
   the binary. Corrections live here (a wrong base URL, our own env-var name, a
   plan label, a window upstream has not learned yet), and providers models.dev
   does not carry at all are declared here. Because it ships with the binary it
   is always present, including on a machine that has never synced.
3. **Your configuration** — `~/.oaica/remotes.json`, `~/.oaica/aliases.json`,
   `~/.oaica/local_servers.json`. A remote you configured always wins for its
   own models: the two layers above are the vendor's list, never a user's box
   that happens to share a vendor's name.

`~/.oaica/cache/providers/oaica.json` holds a *synced copy* of the overlay,
adopted only if it still parses; the embedded copy remains the fallback.

## Syncing

```sh
oaica model catalog sync              # fetch https://models.dev/api.json
oaica model catalog sync --url URL    # any URL, including file:// paths
oaica model catalog status            # what is cached: path, age, sha256, counts
```

`status` never touches the network — it is what answers "what is this box
actually using" on a host with no route out.

**A refusal is a normal outcome, not a crash.** The sync adopts a payload only
after a contract check: if a field oaica reads has changed shape, the sync fails
with an error naming that field, and **the last good catalog stays in use**. The
check exists because the alternative is a silently wrong picker — a vendor id
resolving to the wrong endpoint, a window that shrinks to zero. There is no
drift tooling yet (archiving versions, `check`/`diff`/`drift`, `--accept-drift`,
the CI check are all still to come), so today a refusal is resolved by hand:
look at the field the error names, add or correct it in
`cmd/launch/providers/oaica.json`, and re-sync.

## Offline and staleness

- No catalog cached: the overlay and your own local/remote models still list,
  and one line names `oaica model catalog sync`. Nothing errors.
- A remote that cannot be swept (no key, no model list on that endpoint): its
  rows come from the catalog, each marked **unverified** in the picker — the
  vendor's catalogue says the model exists, but nothing on the wire confirmed it
  is still served. When the sweep does answer, it is the authority and nothing
  is marked.
- Past 30 days, the picker says the catalog is stale rather than pretending a
  snapshot is current. It is a warning, not a refusal: a stale catalog is much
  better than none.

The cache lives under `~/.oaica/cache/catalog/`. Deleting it is always safe —
the next sync recreates it, the overlay is embedded, and nothing else reads it.

## First-party endpoints

oaica's own models (`oaica-default`, `oaica-35b-a3b-1M`, from the overlay's
`models` list) resolve at launch. `OAICA_GATEWAY_URL` points them at one
gateway:

```sh
export OAICA_GATEWAY_URL=http://192.168.0.133:8081      # the gateway's base
export OAICA_GATEWAY_TOKEN=...                          # only if it requires one
```

The override redirects **only** oaica's own ids — a bare `oaica-*` SKU or the
same id written `router/<id>` / `oaica/<id>`. It does not reorder the rest of
the resolution chain: an alias still beats everything, a user remote still wins
for its own models, and `<model>:local` still means the box serving it. When it
is set, `OAICA_GATEWAY_TOKEN` is named to the child-environment scrubber, so the
gateway credential never reaches the agent's own shell.

A first-party model nothing resolved is still listed, badged **unavailable**,
rather than hidden: a model of ours that vanishes from the picker reads as a bug
in oaica, not as a gateway that is down. The badge is about verification — the
router's list is what confirms a model is served, and the gateway override is a
destination rather than a confirmation, so a row added on its account still says
nothing on the wire vouched for it.

That padding has one gate: it happens only when `OAICA_GATEWAY_URL` is set, so a
first-party row is listed only when there is an endpoint it can launch against.
The router's own list is the other way these models reach the picker, and an id
that list did not carry is one the router does not serve. Without either, the
rows are omitted rather than listed dead — the same rule that keeps upstream
Ollama's built-in catalog out of a fork that never serves those models.
