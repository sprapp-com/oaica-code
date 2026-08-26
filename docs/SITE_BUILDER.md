# `oaica site` — static site builder (v1, optional add-on)

Turn a one-line brief into a single-page static website, preview it in a
sandbox, and publish it to Cloudflare Pages. Lives in `internal/sitebuilder`
plus `cmd/site.go`; the core `launch` / `run` / `serve` path does not depend
on it.

```bash
oaica site new ./clinic --prompt "Landing page for Bright Smile Dental, a family dental clinic in Johor Bahru: same-day appointments, kids' dentistry, prices in MYR"
oaica site sections ./clinic
oaica site edit ./clinic --prompt "hero should mention we open 7 days"      # model picks the section
oaica site edit ./clinic --section pricing --prompt "add a student discount"
oaica site preview ./clinic            # http://127.0.0.1:4173, Ctrl-C to stop
oaica site deploy ./clinic --project bright-smile-jb
```

`new`/`edit` need `OAICA_API_KEY` (or `oaica signin`). Default model is
`kat-awq` on the router; `--model` picks any model the router lists,
`OAICA_HOST` points at a different OpenAI-compatible endpoint.

## Why it is built the way it is

The first probe ("write me a landing page") on kat-awq never emitted
`</html>` in 8k tokens, wrapped the page in a ```` ```html ```` fence, repeated
one line 161 times and produced a runaway inline SVG. Asking a model for a
whole page in one shot is the wrong unit of work. v1 therefore:

1. **Plans first.** One JSON call returns title, tagline, palette and 3–7
   sections with concrete briefs. Prose/fences around the JSON are
   tolerated; a bad reply is retried once with the parse error fed back.
2. **Generates per section, in parallel (×4).** Each call is bounded
   (1.5k tokens, `stop: ["</section>"]`) and independent, so a runaway in one
   section cannot eat the page. The planned `id` and `kind` are forced onto
   the fragment so nav anchors always resolve.
3. **Never lets the model write CSS or JS.** Fragments use a small documented
   class vocabulary; the page gets one built-in stylesheet whose tokens come
   from the plan's palette. That is what makes independently generated
   sections look like one site.
4. **Sanitizes every fragment** through an element/attribute allowlist
   (`x/net/html` tokenizer): no `<script>`, `<style>`, `<svg>`, `<iframe>`,
   event handlers, `javascript:`/`data:` URLs; `target="_blank"` gets
   `rel="noopener noreferrer"`; lines repeated more than twice are dropped;
   output is capped at 12 KB with open tags closed. A reply with no
   `<section>` is retried once, then fails loudly.
5. **Keeps state per section** in `<dir>/.oaica-site/` (`site.json` +
   `sections/<id>.html`) so `edit` regenerates one section with the
   existing HTML and the instruction as context, and `index.html` is
   re-assembled from all fragments.

## Preview

`oaica site preview` serves the directory on 127.0.0.1 and shows
`index.html` inside an iframe with `sandbox=""` (no scripts, no forms, no
same-origin access). The state directory is never served. Desktop / tablet /
phone width toggles are in the wrapper bar.

## Deploy

`oaica site deploy` shells out to `wrangler pages deploy` on an **export**
of the directory (everything except `.oaica-site/`, `.git`, `node_modules`),
so the brief and plan stay local. It creates the Pages project on first use
(`Project not found` → `wrangler pages project create` → retry) and prints
the `*.pages.dev` URL. Requires `wrangler` in `PATH` and either
`CLOUDFLARE_API_TOKEN` or `wrangler login`. Custom domains are attached in
the Cloudflare dashboard as with any Pages project.

## Not in v1 (deliberately)

- Multi-page sites, images (placeholders only — `div.ph`), forms that
  actually submit, analytics, i18n beyond a `lang` attribute.
- Model-authored CSS/JS. If a design needs it, edit the exported files by
  hand; `edit` will keep regenerating only the section you name.
- Any other hosting target. `Export` produces plain files; deploy them
  anywhere.

## Tests

`go test ./internal/sitebuilder/` — plan parsing/normalization, sanitizer
(fences, scripts, SVG, handlers, repeats, truncation), forced ids, parallel
build + assembly, save/load/edit/export roundtrip, sandboxed preview, deploy
with a stubbed wrangler (project-creation retry, state dir excluded).
