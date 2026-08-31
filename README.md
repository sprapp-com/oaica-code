# oaica.com

Static site, zero build step, zero framework. Plain HTML/CSS in `dist/`.

Rate card matches `oaica-code/docs/PRICING.md` — update both together.

## Deploy

```bash
wrangler pages deploy dist --project-name oaica-com
```

First deploy needs `wrangler pages project create oaica-com` and the domain
attached in the Cloudflare dashboard (Pages → oaica-com → Custom domains →
oaica.com), assuming the domain is registered and its nameservers point at
Cloudflare.

## Maintenance

None expected. Static HTML, no dependencies, no build. Edit `dist/index.html`
directly and redeploy.
