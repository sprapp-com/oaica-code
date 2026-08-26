package sitebuilder

import (
	"fmt"
	"regexp"
	"strings"
)

// The model never writes CSS. Every generated fragment uses the small class
// vocabulary below and the page gets one built-in stylesheet whose tokens
// come from the plan's palette. This is what keeps a site consistent across
// independently generated sections, and it removes the single biggest
// source of runaway output (free-form CSS/SVG).

// classVocabulary is what the section prompt advertises. Keep in sync with
// baseCSS.
const classVocabulary = `container, grid, grid-2, grid-3, grid-4, card, btn, btn-primary, btn-outline, eyebrow, lead, muted, center, stack, ph (image placeholder block), price, badge, faq (on a <details>)`

var hexColorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

func colorOr(v, def string) string {
	v = strings.TrimSpace(v)
	if hexColorRe.MatchString(v) {
		return v
	}
	return def
}

func fontStack(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "serif":
		return `Georgia, "Times New Roman", serif`
	case "mono", "monospace":
		return `ui-monospace, "SF Mono", Menlo, Consolas, monospace`
	default:
		return `Inter, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif`
	}
}

// baseCSS renders the stylesheet for a spec's palette.
func baseCSS(sp Spec) string {
	primary := colorOr(sp.Palette.Primary, "#1f4e79")
	accent := colorOr(sp.Palette.Accent, "#d98c2b")
	bg := colorOr(sp.Palette.Background, "#ffffff")
	text := colorOr(sp.Palette.Text, "#1b1b1b")
	return fmt.Sprintf(`:root{--primary:%s;--accent:%s;--bg:%s;--text:%s;--muted:color-mix(in srgb,var(--text) 60%%,var(--bg));--line:color-mix(in srgb,var(--text) 12%%,var(--bg));--card:color-mix(in srgb,var(--bg) 96%%,var(--text));--radius:12px;--font:%s}
*,*::before,*::after{box-sizing:border-box}
html{scroll-behavior:smooth}
body{margin:0;font-family:var(--font);font-size:17px;line-height:1.6;color:var(--text);background:var(--bg)}
img{max-width:100%%;height:auto;display:block}
a{color:var(--primary)}
h1,h2,h3,h4{line-height:1.2;margin:0 0 .5em;font-weight:700}
h1{font-size:clamp(2rem,5vw,3.25rem)}
h2{font-size:clamp(1.5rem,3.2vw,2.25rem)}
h3{font-size:1.2rem}
p{margin:0 0 1em}
.container{width:min(1100px,92vw);margin-inline:auto}
.site-header{position:sticky;top:0;z-index:10;background:color-mix(in srgb,var(--bg) 92%%,transparent);backdrop-filter:blur(8px);border-bottom:1px solid var(--line)}
.site-header .container{display:flex;align-items:center;justify-content:space-between;gap:1rem;padding:.8rem 0}
.brand{font-weight:800;text-decoration:none;color:var(--text);font-size:1.15rem}
.site-nav{display:flex;gap:1.2rem;flex-wrap:wrap}
.site-nav a{text-decoration:none;color:var(--muted);font-size:.95rem}
.site-nav a:hover{color:var(--primary)}
.sec{padding:clamp(3rem,8vw,6rem) 0;border-bottom:1px solid var(--line)}
.sec-hero{padding:clamp(4rem,12vw,8rem) 0;background:linear-gradient(135deg,color-mix(in srgb,var(--primary) 10%%,var(--bg)),var(--bg))}
.sec-cta{background:var(--primary);color:#fff;text-align:center}
.sec-cta a{color:#fff}
.sec-cta .btn-primary{background:#fff;color:var(--primary)}
.grid,.grid-2,.grid-3,.grid-4{display:grid;gap:1.5rem}
.grid-2{grid-template-columns:repeat(auto-fit,minmax(280px,1fr))}
.grid-3{grid-template-columns:repeat(auto-fit,minmax(240px,1fr))}
.grid-4{grid-template-columns:repeat(auto-fit,minmax(200px,1fr))}
/* Model wrote cards straight into the container with no grid wrapper: lay
   them out as a grid anyway and let headings span the full row. */
.container:not(.grid):not(.grid-2):not(.grid-3):not(.grid-4):has(> .card + .card){display:grid;gap:1.5rem;grid-template-columns:repeat(auto-fit,minmax(240px,1fr))}
.container:has(> .card + .card) > :not(.card){grid-column:1/-1}
.card{background:var(--card);border:1px solid var(--line);border-radius:var(--radius);padding:1.5rem}
.card .ph{aspect-ratio:16/9;margin-bottom:1rem}
.stack>*+*{margin-top:1rem}
.btn{display:inline-block;padding:.75rem 1.4rem;border-radius:999px;font-weight:600;text-decoration:none;border:2px solid var(--primary);color:var(--primary);background:transparent;cursor:pointer;font:inherit}
.btn-primary{background:var(--primary);color:#fff}
.btn-primary:hover{filter:brightness(1.1)}
.btn-outline{background:transparent}
.eyebrow{display:inline-block;text-transform:uppercase;letter-spacing:.12em;font-size:.78rem;font-weight:700;color:var(--accent);margin-bottom:.75rem}
.lead{font-size:1.2rem;color:var(--muted);max-width:60ch}
.muted{color:var(--muted)}
.center{text-align:center}
.center .lead{margin-inline:auto}
.ph{aspect-ratio:16/10;max-height:420px;border-radius:var(--radius);background:linear-gradient(135deg,color-mix(in srgb,var(--primary) 35%%,var(--bg)),color-mix(in srgb,var(--accent) 45%%,var(--bg)))}
.price{font-size:clamp(1.4rem,2.4vw,2rem);font-weight:800;color:var(--primary);white-space:nowrap}
.badge{display:inline-block;padding:.2rem .6rem;border-radius:999px;background:color-mix(in srgb,var(--accent) 18%%,var(--bg));color:var(--accent);font-size:.8rem;font-weight:700}
details.faq{border:1px solid var(--line);border-radius:var(--radius);padding:.9rem 1.2rem;margin-bottom:.75rem;background:var(--card)}
details.faq summary{cursor:pointer;font-weight:600}
blockquote{margin:0;padding-left:1rem;border-left:4px solid var(--accent);font-style:italic}
form{display:grid;gap:.75rem;max-width:480px}
input,textarea,select{font:inherit;padding:.7rem .9rem;border:1px solid var(--line);border-radius:8px;background:var(--bg);color:var(--text);width:100%%}
table{width:100%%;border-collapse:collapse}
th,td{text-align:left;padding:.6rem;border-bottom:1px solid var(--line)}
.site-footer{padding:2rem 0;color:var(--muted);font-size:.9rem}
@media (max-width:640px){.site-header .container{flex-direction:column;align-items:flex-start}}
`, primary, accent, bg, text, fontStack(sp.Font))
}
