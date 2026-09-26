package sitebuilder

// sanitizer_integrity_test.go — the sanitizer is the only thing between a
// model's (or a router's) output and a page published to the open web, and
// four holes in it were reproduced end-to-end through the real CLI
// (2026-09-26 audit, third round):
//
//   - byte offsets taken from strings.ToLower(frag) were used to slice frag
//     itself. Several runes change length when lowercased (İ U+0130 and Ⱥ
//     U+023A grow; ẞ, Ω U+2126, K U+212A, Å U+212B and ~20 more shrink), which
//     desynchronises the two strings: sanitize.go's stripWrapping then slices
//     out of range (a PANIC on model output), and sitebuilder.go's
//     ensureSectionID — which runs AFTER Sanitize and is never re-sanitized —
//     splices raw attribute-VALUE bytes back into attribute-NAME position,
//     producing a live onmouseover handler in the stored and deployed page;
//   - srcset was allowlisted but never URL-checked;
//   - safeURL called anything starting with "/" relative, so "//evil.example"
//     (and its backslash spellings, which WHATWG resolves the same way) passed
//     as a relative link to an attacker's origin;
//   - Load rendered fragments straight off disk with no sanitizer at all.

import (
	"path/filepath"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
)

// runes whose case-folding changes their UTF-8 byte length.
var lengthChangingRunes = []rune{
	'İ', 'Ⱥ', 'Ⱦ', 'ẞ', 'Ω', 'K', 'Å', 'ſ', 'ﬀ', 'ﬁ', 'ﬅ', 'ᾈ', 'ﬔ',
}

// A1/A2: no fragment can make Sanitize panic, and none of these runes can
// desynchronise an offset.
func TestSanitizeSurvivesLengthChangingRunes(t *testing.T) {
	for _, r := range lengthChangingRunes {
		for _, where := range []string{"head", "attr", "body", "tail"} {
			var in string
			switch where {
			case "head":
				in = "<section>" + strings.Repeat(string(r), 3) + "</section>"
			case "attr":
				in = `<section title="` + strings.Repeat(string(r), 24) + `">x</section>`
			case "body":
				in = "<section><p>" + strings.Repeat(string(r), 24) + "</p></section>"
			case "tail":
				in = "<section>x</section>" + strings.Repeat(string(r), 24)
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						t.Errorf("Sanitize(%q) panicked: %v — an offset from strings.ToLower must never index the original string (model output is remote input)", in, rec)
					}
				}()
				got := Sanitize(in)
				if got == "" {
					return // no <section> found is a legitimate answer
				}
				if n := parsedAttrCount(t, got, "onmouseover"); n != 0 {
					t.Errorf("Sanitize(%q) = %q, whose parse has an onmouseover attribute", in, got)
				}
			}()
		}
	}
}

// parsedAttrCount reports how many attributes of the given (lowercased) name a
// browser parser finds in doc — the auditor's method, since a grep for the
// substring cannot tell an attribute name from text.
func parsedAttrCount(t *testing.T, doc, attr string) int {
	t.Helper()
	nodes, err := xhtml.Parse(strings.NewReader("<body>" + doc))
	if err != nil {
		t.Fatalf("parse %q: %v", doc, err)
	}
	count := 0
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		for _, a := range n.Attr {
			if strings.EqualFold(a.Key, attr) {
				count++
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(nodes)
	return count
}

// A1: the exact shape the auditor reproduced — a title attribute value padded
// so that the desynchronised offset lands inside it, and the value's own tail
// is spliced back in attribute-NAME position (which is how `onmouseover` goes
// live in a fragment that Sanitize had already approved).
//
// The padding length is not a constant to guess at: which runes shift an
// offset, and by how much, depends on the rune and on the tag, so the test
// sweeps the length for both a SHRINKING rune (ẞ U+1E9E, 3 bytes → 2) and a
// GROWING one (İ U+0130, 2 bytes → 3). One of them lands exactly on the value's
// tail for some n, which is the case the audit hit by hand.
func TestEnsureSectionIDCannotSmuggleAnEventHandler(t *testing.T) {
	for _, padRune := range []string{"ẞ", "İ", "Ⱥ"} {
		for n := 1; n <= 64; n++ {
			raw := `<section title="` + strings.Repeat(padRune, n) +
				`onmouseover=alert(document.domain)//"><p>Same-day appointments.</p></section>`
			frag := Sanitize(raw)
			if frag == "" {
				continue // no <section> found is a legitimate answer
			}
			stored := ensureSectionID(frag, Section{ID: "hero", Kind: "hero"})
			if c := parsedAttrCount(t, stored, "onmouseover"); c != 0 {
				t.Fatalf("stored fragment parses with %d live onmouseover attribute(s) (pad %q x%d):\n%s", c, padRune, n, stored)
			}
			if !strings.Contains(stored, `id="hero"`) {
				t.Fatalf("the section id was lost (pad %q x%d):\n%s", padRune, n, stored)
			}
		}
	}
}

// A3: srcset is a URL-bearing attribute and goes through the same check.
func TestSrcsetIsURLChecked(t *testing.T) {
	for _, bad := range []string{
		`<section><img srcset="javascript:alert(1) 1x"></section>`,
		`<section><img srcset="data:text/html,<script>alert(1)</script> 1x"></section>`,
		`<section><img srcset="//evil.example/track.gif 1x"></section>`,
	} {
		got := Sanitize(bad)
		if strings.Contains(strings.ToLower(got), "javascript:") || strings.Contains(got, "data:text/html") || strings.Contains(got, "//evil.example") {
			t.Errorf("Sanitize(%q) = %q — srcset is allowlisted but was never URL-checked", bad, got)
		}
	}
	// And a legitimate srcset still survives.
	good := Sanitize(`<section><img srcset="a.png 1x, b.png 2x" src="a.png" alt="x"></section>`)
	if !strings.Contains(good, "srcset=") {
		t.Errorf("Sanitize dropped a safe srcset: %q", good)
	}
}

// A4: a protocol-relative URL is an absolute URL to another origin.
func TestSafeURLRefusesProtocolRelative(t *testing.T) {
	for _, bad := range []string{"//evil.example/phish", `\\evil.example/phish`, `\/\/evil.example/phish`, `/\evil.example/x`, "  //evil.example/x"} {
		if got, ok := safeURL(bad); ok {
			t.Errorf("safeURL(%q) = %q, ok — a protocol-relative URL resolves to another origin (and the backslash spellings are the same URL to a WHATWG parser)", bad, got)
		}
	}
	for _, good := range []string{"/local/x.png", "images/x.png", "#frag", "https://ok.example/x", "mailto:a@b.c", "tel:+60123456789", "./rel"} {
		if _, ok := safeURL(good); !ok {
			t.Errorf("safeURL(%q) refused a legitimate URL", good)
		}
	}
}

// A5: on-disk fragments are data too. Load is the one place every read path
// funnels through, and it used to store whatever was in the file.
func TestLoadSanitizesStoredFragments(t *testing.T) {
	dir := t.TempDir()
	s := &Site{
		Prompt:    "p",
		Model:     "m",
		Spec:      Spec{Title: "T", Sections: []Section{{ID: "hero", Kind: "hero", Title: "Hero"}}},
		Fragments: map[string]string{"hero": `<section id="hero">clean</section>`},
	}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}

	// A fragment file that did NOT come from this pipeline: an older version's
	// output, a restored backup, a hand edit, a site dir received from someone.
	writeFile(t, filepath.Join(dir, StateDir, sectionsDir, "hero.html"),
		`<section id="hero"><script src="//evil.example/x.js"></script><img src=x onerror=alert(1)></section>`)

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := got.Fragments["hero"]
	if strings.Contains(strings.ToLower(body), "script") || strings.Contains(strings.ToLower(body), "onerror") {
		t.Errorf("Load returned the file verbatim: %q — the sanitizer is a gate at generation, and stored state is data like any other", body)
	}
	if html := Assemble(got); strings.Contains(strings.ToLower(html), "onerror") || strings.Contains(strings.ToLower(html), "<script") {
		t.Errorf("the assembled page still carries the payload:\n%s", html)
	}
}

// Load re-sanitizes every stored fragment and ensureSectionID re-renders what
// it splices, so a fragment can be transformed more than once on the way to a
// page. Sanitize must therefore be a fixed point: running it on a fragment it
// already produced must not change a byte (a second pass that re-escapes
// "&amp;" to "&amp;amp;" or strips an attribute it emitted itself would corrupt
// every stored site on the next edit).
func TestSanitizeIsIdempotentOnStoredFragments(t *testing.T) {
	cases := []string{
		`<section id="hero" class="x"><h1>Tea &amp; Cake</h1><p title="a &quot;b&quot; &amp; c">Same-day <a href="/book?a=1&amp;b=2">booking</a></p><img src="a.png" srcset="a.png 1x, b.png 2x" alt="x"></section>`,
		`<section><p>Ünïcödé İ ẞ Ⱥ &lt;tag&gt; Ω</p></section>`,
		`<section><div class="card" data-x="1" aria-label="y">ok</div></section>`,
	}
	for _, in := range cases {
		once := Sanitize(in)
		if twice := Sanitize(once); twice != once {
			t.Errorf("Sanitize is not a fixed point:\n once  = %q\n twice = %q", once, twice)
		}
		sec := Section{ID: "hero", Kind: "hero"}
		o1 := ensureSectionID(once, sec)
		if o2 := ensureSectionID(o1, sec); o2 != o1 {
			t.Errorf("ensureSectionID is not a fixed point:\n o1 = %q\n o2 = %q", o1, o2)
		}
		if again := Sanitize(o1); again != o1 {
			t.Errorf("Sanitize changes a fragment that was already stored:\n stored = %q\n again  = %q", o1, again)
		}
	}
}
