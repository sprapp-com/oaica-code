package sitebuilder

import (
	"html"
	"regexp"
	"strings"

	xhtml "golang.org/x/net/html"
)

// Why a sanitizer at all: the first probe of "generate a landing page" on
// kat-awq never emitted </html> within 8k tokens, wrapped everything in a
// ```html fence, repeated 161 identical lines and produced a runaway inline
// SVG. Generation is therefore split per section (see Build) and every
// fragment goes through this pipeline before it is stored or rendered:
//
//  1. strip <think> blocks and code fences;
//  2. keep only the first <section ...> ... </section> (append the close
//     tag if the stop sequence ate it);
//  3. re-render through an element/attribute allowlist so no script, style,
//     svg, iframe, event handler or javascript: URL survives — the preview
//     iframe is fully sandboxed and the deployed site is plain static HTML;
//  4. collapse repeated lines and cap total size, closing any open tags.

const (
	maxFragmentBytes = 12 * 1024
	maxRepeatedLine  = 2 // identical non-trivial lines allowed per fragment
)

var (
	thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)
	fenceRe = regexp.MustCompile("(?m)^\\s*```[a-zA-Z0-9_-]*\\s*$")
)

var allowedTags = map[string]bool{
	"section": true, "div": true, "header": true, "footer": true, "nav": true, "main": true,
	"article": true, "aside": true, "figure": true, "figcaption": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"p": true, "a": true, "span": true, "strong": true, "em": true, "b": true, "i": true,
	"small": true, "br": true, "hr": true, "blockquote": true, "q": true, "cite": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true,
	"img": true, "picture": true, "source": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true, "th": true, "td": true,
	"button": true, "form": true, "label": true, "input": true, "textarea": true, "select": true, "option": true,
	"details": true, "summary": true, "time": true, "address": true, "sup": true, "sub": true,
	"code": true, "pre": true, "kbd": true, "abbr": true, "mark": true,
}

var voidTags = map[string]bool{
	"br": true, "hr": true, "img": true, "input": true, "source": true,
}

var allowedAttrs = map[string]bool{
	"id": true, "class": true, "href": true, "src": true, "alt": true, "title": true,
	"target": true, "rel": true, "role": true, "loading": true, "width": true, "height": true,
	"type": true, "placeholder": true, "name": true, "for": true, "required": true,
	"colspan": true, "rowspan": true, "datetime": true, "lang": true, "dir": true,
	"value": true, "rows": true, "cols": true, "open": true, "srcset": true, "sizes": true,
}

// safeURL accepts relative, fragment, http(s), mailto and tel URLs only.
// data: and javascript: (in any casing or with leading whitespace) are out.
func safeURL(raw string) (string, bool) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", false
	}
	lower := strings.ToLower(u)
	switch {
	case strings.HasPrefix(lower, "#"), strings.HasPrefix(lower, "/"), strings.HasPrefix(lower, "./"), strings.HasPrefix(lower, "../"):
		return u, true
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"),
		strings.HasPrefix(lower, "mailto:"), strings.HasPrefix(lower, "tel:"):
		return u, true
	}
	// bare relative path like "about.html" or "images/x.png": no scheme.
	if !strings.Contains(lower, ":") {
		return u, true
	}
	return "", false
}

// stripWrapping removes reasoning blocks and markdown fences, then isolates
// the first <section> element. Returns "" if no section tag is present.
func stripWrapping(raw string) string {
	s := thinkRe.ReplaceAllString(raw, "")
	s = fenceRe.ReplaceAllString(s, "")
	lower := strings.ToLower(s)
	start := strings.Index(lower, "<section")
	if start < 0 {
		return ""
	}
	s = s[start:]
	lower = lower[start:]
	if end := strings.LastIndex(lower, "</section>"); end >= 0 {
		s = s[:end+len("</section>")]
	} else {
		s += "</section>"
	}
	return s
}

// renderAllowlisted re-emits the fragment keeping only allowlisted elements
// and attributes. Disallowed containers (script, style, svg, iframe, ...)
// are dropped WITH their contents; other unknown elements are unwrapped
// (contents kept). Output is truncated at maxFragmentBytes with all open
// tags closed so the result is always well-formed.
func renderAllowlisted(fragment string) string {
	z := xhtml.NewTokenizer(strings.NewReader(fragment))
	var out strings.Builder
	var stack []string
	skipDepth := 0 // >0 while inside a dropped container
	truncated := false

	writeOpen := func(t xhtml.Token) {
		out.WriteByte('<')
		out.WriteString(t.Data)
		for _, a := range t.Attr {
			key := strings.ToLower(a.Key)
			if strings.HasPrefix(key, "on") {
				continue
			}
			if !allowedAttrs[key] && !strings.HasPrefix(key, "aria-") && !strings.HasPrefix(key, "data-") {
				continue
			}
			val := a.Val
			if key == "href" || key == "src" {
				v, ok := safeURL(val)
				if !ok {
					continue
				}
				val = v
			}
			if key == "target" && val == "_blank" {
				// never let a generated link hand its opener to another origin
				out.WriteString(` rel="noopener noreferrer"`)
			}
			if key == "rel" {
				continue // emitted alongside target above; other rels are dropped
			}
			out.WriteByte(' ')
			out.WriteString(key)
			out.WriteString(`="`)
			out.WriteString(html.EscapeString(val))
			out.WriteByte('"')
		}
		out.WriteByte('>')
	}

	for !truncated {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			break
		}
		t := z.Token()
		name := strings.ToLower(t.Data)
		switch tt {
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			if skipDepth > 0 {
				if !voidTags[name] && tt == xhtml.StartTagToken {
					skipDepth++
				}
				continue
			}
			if !allowedTags[name] {
				// dangerous or unknown: drop with contents for the
				// known-bad set, otherwise unwrap.
				switch name {
				case "script", "style", "svg", "iframe", "object", "embed", "template", "noscript", "canvas", "video", "audio", "link", "meta", "head", "title", "base":
					if tt == xhtml.StartTagToken && !voidTags[name] {
						skipDepth = 1
					}
				}
				continue
			}
			t.Data = name
			writeOpen(t)
			if !voidTags[name] && tt == xhtml.StartTagToken {
				stack = append(stack, name)
			}
		case xhtml.EndTagToken:
			if skipDepth > 0 {
				skipDepth--
				continue
			}
			if !allowedTags[name] {
				continue
			}
			// close up to the matching open tag (tolerates mis-nesting)
			idx := -1
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i] == name {
					idx = i
					break
				}
			}
			if idx < 0 {
				continue
			}
			for i := len(stack) - 1; i >= idx; i-- {
				out.WriteString("</" + stack[i] + ">")
			}
			stack = stack[:idx]
		case xhtml.TextToken:
			if skipDepth > 0 {
				continue
			}
			out.WriteString(html.EscapeString(t.Data))
		case xhtml.CommentToken, xhtml.DoctypeToken:
			// dropped
		}
		if out.Len() > maxFragmentBytes {
			truncated = true
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		out.WriteString("</" + stack[i] + ">")
	}
	return out.String()
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// collapseRepeats removes runaway repetition: any line whose VISIBLE TEXT
// has already appeared maxRepeatedLine times is dropped. Keying on text
// rather than the raw line matters: markup lines such as
// `<div class="card">` legitimately repeat once per card, and dropping an
// opener while its `</div>` survives collapses the grid (seen on the first
// live run — cards 3 and 4 of a services grid fell out of their wrappers).
func collapseRepeats(s string) string {
	seen := map[string]int{}
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		key := strings.TrimSpace(tagRe.ReplaceAllString(line, ""))
		if len(key) > 12 {
			seen[key]++
			if seen[key] > maxRepeatedLine {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// Sanitize runs the whole pipeline. It returns "" when the model produced no
// <section> at all, which callers treat as a failed generation.
func Sanitize(raw string) string {
	frag := stripWrapping(raw)
	if frag == "" {
		return ""
	}
	frag = collapseRepeats(frag)
	frag = renderAllowlisted(frag)
	return strings.TrimSpace(frag)
}
