package sitebuilder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLLM answers by inspecting the request; it records concurrency so the
// parallel build is actually parallel.
type fakeLLM struct {
	mu       sync.Mutex
	calls    []Request
	inFlight int32
	maxSeen  int32
	plan     string
	section  func(req Request) string
}

func (f *fakeLLM) Complete(_ context.Context, req Request) (string, error) {
	n := atomic.AddInt32(&f.inFlight, 1)
	for {
		m := atomic.LoadInt32(&f.maxSeen)
		if n <= m || atomic.CompareAndSwapInt32(&f.maxSeen, m, n) {
			break
		}
	}
	defer atomic.AddInt32(&f.inFlight, -1)
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if strings.Contains(req.System, "planning") {
		return f.plan, nil
	}
	if strings.Contains(req.System, "route edit requests") {
		return "pricing", nil
	}
	if f.section != nil {
		// give concurrent section calls a chance to overlap so the
		// parallelism assertion in TestBuild is meaningful
		time.Sleep(20 * time.Millisecond)
		return f.section(req), nil
	}
	return `<section><div class="container"><h2>Generated</h2><p>Body copy that is specific.</p></div>`, nil
}

const goodPlan = "Sure! Here is the plan:\n```json\n" + `{
  "title": "Bright Smile Dental",
  "tagline": "Same-day appointments in Johor Bahru",
  "language": "en",
  "font": "sans",
  "palette": {"primary": "#0b5fff", "accent": "#ff8a00", "background": "#ffffff", "text": "#111111"},
  "sections": [
    {"id": "hero", "kind": "hero", "title": "Welcome", "brief": "same-day appointments"},
    {"id": "services", "kind": "services", "title": "Services", "brief": "cleaning, whitening, implants"},
    {"id": "Pricing!", "kind": "pricing", "title": "Pricing", "brief": "three plans"},
    {"id": "hero", "kind": "weird-kind", "title": "", "brief": "duplicate id and unknown kind"},
    {"id": "contact", "kind": "contact", "title": "Contact", "brief": "address and form"}
  ]
}` + "\n```\nLet me know if you want changes."

func TestPlan_ToleratesProseAndFences_NormalizesIDsAndKinds(t *testing.T) {
	llm := &fakeLLM{plan: goodPlan}
	sp, err := Plan(context.Background(), llm, "dental clinic in JB")
	if err != nil {
		t.Fatal(err)
	}
	if sp.Title != "Bright Smile Dental" || len(sp.Sections) != 5 {
		t.Fatalf("spec = %+v", sp)
	}
	ids := []string{}
	for _, s := range sp.Sections {
		ids = append(ids, s.ID)
	}
	want := "hero services pricing hero-2 contact"
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("ids = %q, want %q", got, want)
	}
	if sp.Sections[3].Kind != "custom" || sp.Sections[3].Title != "Custom" {
		t.Fatalf("unknown kind not normalized: %+v", sp.Sections[3])
	}
}

func TestPlan_RetriesOnceWithFeedback(t *testing.T) {
	n := 0
	// first attempt is prose, second returns a valid plan
	wrapped := llmFunc(func(ctx context.Context, req Request) (string, error) {
		n++
		if n == 1 {
			return "I cannot produce that.", nil
		}
		if !strings.Contains(req.User, "rejected") {
			t.Fatalf("retry must feed the rejection back, got user prompt without it")
		}
		return goodPlan, nil
	})
	if _, err := Plan(context.Background(), wrapped, "x"); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

type llmFunc func(ctx context.Context, req Request) (string, error)

func (f llmFunc) Complete(ctx context.Context, req Request) (string, error) { return f(ctx, req) }

func TestSanitize_StripsFencesScriptsStylesSVGHandlersAndRepeats(t *testing.T) {
	raw := "<think>planning…</think>\n```html\n" +
		`<section id="x" class="whatever" onclick="evil()" style="color:red">
<style>body{display:none}</style>
<script>alert(1)</script>
<svg width="9999"><path d="M0 0"/><path d="M0 0"/></svg>
<div class="container">
<h2>Title</h2>
<a href="javascript:alert(1)" onmouseover="x()">bad link</a>
<a href="https://example.com" target="_blank">good link</a>
<img src="data:image/png;base64,AAAA" alt="no">
<p>Repeated line that goes on and on.</p>
<p>Repeated line that goes on and on.</p>
<p>Repeated line that goes on and on.</p>
<p>Repeated line that goes on and on.</p>
<custom-el>kept text</custom-el>
</div>
</section>` + "\n```\nHope this helps!"
	got := Sanitize(raw)
	for _, bad := range []string{"<script", "<style", "<svg", "onclick", "onmouseover", "javascript:", "style=", "data:image", "```", "<think>", "Hope this helps", "<custom-el"} {
		if strings.Contains(got, bad) {
			t.Fatalf("sanitized output still contains %q:\n%s", bad, got)
		}
	}
	for _, good := range []string{`<section`, `<h2>Title</h2>`, `href="https://example.com"`, `rel="noopener noreferrer"`, "kept text", "</section>"} {
		if !strings.Contains(got, good) {
			t.Fatalf("sanitized output lost %q:\n%s", good, got)
		}
	}
	if n := strings.Count(got, "Repeated line"); n != maxRepeatedLine {
		t.Fatalf("repeated line kept %d times, want %d:\n%s", n, maxRepeatedLine, got)
	}
	if !strings.HasSuffix(got, "</section>") {
		t.Fatalf("must end with </section>: %q", got[len(got)-40:])
	}
}

// Regression: the repeat collapser must key on visible text, not markup.
// Four cards means four identical `<div class="card">` lines; dropping the
// third and fourth openers (while their closers survive) broke the grid on
// the first live run.
func TestSanitize_RepeatedMarkupLinesAreKept(t *testing.T) {
	raw := `<section><div class="container"><div class="grid grid-3">
<div class="card">
<h3>Cleaning</h3>
<p><span class="price">RM 120</span></p>
</div>
<div class="card">
<h3>Whitening</h3>
<p><span class="price">RM 450</span></p>
</div>
<div class="card">
<h3>Braces</h3>
<p><span class="price">RM 3,500</span></p>
</div>
<div class="card">
<h3>Implants</h3>
<p><span class="price">RM 2,800</span></p>
</div>
</div></div></section>`
	got := Sanitize(raw)
	if n := strings.Count(got, `<div class="card">`); n != 4 {
		t.Fatalf("kept %d cards, want 4:\n%s", n, got)
	}
	if !strings.Contains(got, `<div class="card">
<h3>Implants</h3>`) {
		t.Fatalf("fourth card lost its wrapper:\n%s", got)
	}
}

func TestSanitize_NoSectionMeansEmpty(t *testing.T) {
	if got := Sanitize("<div>just a div</div>"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestSanitize_StopAteCloseTag_AppendsIt(t *testing.T) {
	got := Sanitize(`<section class="sec"><div class="container"><p>hi</p></div>`)
	if !strings.HasSuffix(got, "</div></section>") {
		t.Fatalf("got %q", got)
	}
}

func TestSanitize_CapsRunawayAndClosesTags(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<section><div class="container"><ul>`)
	for i := 0; i < 5000; i++ {
		b.WriteString("<li>item number ")
		b.WriteString(strings.Repeat("x", i%40))
		b.WriteString("</li>\n")
	}
	// no closing tags at all — the model ran out of tokens mid-list
	got := Sanitize(b.String())
	if len(got) > maxFragmentBytes+256 {
		t.Fatalf("fragment not capped: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "</ul></div></section>") {
		t.Fatalf("open tags not closed after truncation: …%s", got[len(got)-60:])
	}
}

func TestGenerateSection_ForcesPlannedIDAndKindClass(t *testing.T) {
	llm := &fakeLLM{section: func(Request) string {
		return `<section id="wrong" class="nope"><div class="container"><h1>Hi</h1></div></section>`
	}}
	sp := Spec{Title: "T", Sections: []Section{{ID: "hero", Kind: "hero", Title: "Hero"}}}
	got, err := GenerateSection(context.Background(), llm, sp, sp.Sections[0], "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, `<section id="hero" class="sec sec-hero">`) {
		t.Fatalf("got %q", got)
	}
}

func TestGenerateSection_RetriesWhenNoSection(t *testing.T) {
	n := 0
	llm := llmFunc(func(_ context.Context, req Request) (string, error) {
		n++
		if n == 1 {
			return "Here is the section: nothing", nil
		}
		return `<section><p>ok</p></section>`, nil
	})
	sp := Spec{Sections: []Section{{ID: "a", Kind: "custom"}}}
	if _, err := GenerateSection(context.Background(), llm, sp, sp.Sections[0], "", ""); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("calls = %d", n)
	}
}

func TestBuild_ParallelAndAssembled(t *testing.T) {
	llm := &fakeLLM{plan: goodPlan, section: func(req Request) string {
		// echo the id we were asked for so assembly order is checkable
		for _, id := range []string{"hero-2", "hero", "services", "pricing", "contact"} {
			if strings.Contains(req.User, `id="`+id+`"`) {
				return `<section><div class="container"><h2>` + id + `</h2><p>copy for ` + id + `</p></div></section>`
			}
		}
		return `<section><p>?</p></section>`
	}}
	site, err := Build(context.Background(), llm, "dental", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&llm.maxSeen) < 2 {
		t.Fatalf("sections were generated serially (max in flight %d)", llm.maxSeen)
	}
	html := Assemble(site)
	for _, want := range []string{"<!doctype html>", `<html lang="en">`, "<title>Bright Smile Dental</title>", `--primary:#0b5fff`, `<a href="#services">Services</a>`, `id="hero" class="sec sec-hero"`, "</html>"} {
		if !strings.Contains(html, want) {
			t.Fatalf("assembled page missing %q", want)
		}
	}
	if strings.Contains(html, `<a href="#hero">`) {
		t.Fatal("hero must not appear in the nav")
	}
	if strings.Index(html, `id="hero"`) > strings.Index(html, `id="services"`) {
		t.Fatal("sections out of plan order")
	}
	// exactly one <section> per planned section
	if n := strings.Count(html, "<section "); n != len(site.Spec.Sections) {
		t.Fatalf("%d sections in page, want %d", n, len(site.Spec.Sections))
	}
}

func TestSaveLoadEditExport(t *testing.T) {
	dir := t.TempDir()
	llm := &fakeLLM{plan: goodPlan}
	site, err := Build(context.Background(), llm, "dental", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := site.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, IndexFile)); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model != "m" || len(loaded.Fragments) != 5 || loaded.Spec.Title != site.Spec.Title {
		t.Fatalf("roundtrip lost data: %+v", loaded)
	}

	// edit without --section: the router picks "pricing"
	id, err := ChooseSection(context.Background(), llm, loaded.Spec, "make the plans cheaper")
	if err != nil || id != "pricing" {
		t.Fatalf("ChooseSection = %q, %v", id, err)
	}
	edit := &fakeLLM{section: func(req Request) string {
		if !strings.Contains(req.User, "Current HTML of this section") || !strings.Contains(req.User, "cheaper") {
			t.Fatalf("edit prompt must carry the existing HTML and the instruction")
		}
		return `<section><div class="container"><h2>Cheaper</h2></div></section>`
	}}
	if err := loaded.Edit(context.Background(), edit, "pricing", "make the plans cheaper", nil); err != nil {
		t.Fatal(err)
	}
	if err := loaded.Save(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, IndexFile))
	if !strings.Contains(string(b), "Cheaper") {
		t.Fatal("edit not reflected in index.html")
	}
	if err := loaded.Edit(context.Background(), edit, "nope", "x", nil); err == nil {
		t.Fatal("editing an unknown section must fail")
	}

	// export: publishable files only
	out := t.TempDir()
	if err := Export(dir, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, IndexFile)); err != nil {
		t.Fatal("index.html not exported")
	}
	if _, err := os.Stat(filepath.Join(out, StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("state dir must not be exported")
	}
}

func TestAssemble_NavUsesShortLabelAndGridCSSIsSelfSufficient(t *testing.T) {
	s := &Site{Spec: Spec{Title: "T", Language: "en", Sections: []Section{
		{ID: "hero", Kind: "hero", Title: "Big Hero Heading"},
		{ID: "services", Kind: "services", Title: "Comprehensive Dental Treatments", Nav: "Services"},
		{ID: "faq", Kind: "faq", Title: "Questions People Ask Us"},
	}}, Fragments: map[string]string{}}
	page := Assemble(s)
	if !strings.Contains(page, `<a href="#services">Services</a>`) || !strings.Contains(page, `<a href="#faq">Questions People Ask Us</a>`) {
		t.Fatalf("nav labels wrong:\n%s", page)
	}
	// the model often writes class="grid-3" without "grid"; the theme must
	// still lay that out as a grid
	if !strings.Contains(page, ".grid,.grid-2,.grid-3,.grid-4{display:grid") {
		t.Fatal("grid-N classes must set display:grid on their own")
	}
}

func TestLoad_NotASite(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "oaica site new") {
		t.Fatalf("err = %v", err)
	}
}

func TestPreview_ServesSandboxedWrapperAndHidesState(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, StateDir), 0o755)
	os.WriteFile(filepath.Join(dir, IndexFile), []byte("<h1>site</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, StateDir, "site.json"), []byte("{}"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	url, err := Preview(ctx, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	get := func(p string) (int, string) {
		resp, err := http.Get(url + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(""); code != 200 || !strings.Contains(body, `sandbox=""`) || !strings.Contains(body, `src="/site/"`) {
		t.Fatalf("wrapper: %d %q", code, body)
	}
	if code, body := get("site/"); code != 200 || body != "<h1>site</h1>" {
		t.Fatalf("site: %d %q", code, body)
	}
	if code, _ := get("site/" + StateDir + "/site.json"); code != 404 {
		t.Fatalf("state dir served: %d", code)
	}
}

func TestDeploy_CreatesProjectWhenMissing_ExportsWithoutState(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, StateDir), 0o755)
	os.WriteFile(filepath.Join(dir, IndexFile), []byte("<h1>x</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, StateDir, "site.json"), []byte("{}"), 0o644)

	oldRun, oldLook := runWrangler, lookPath
	t.Cleanup(func() { runWrangler, lookPath = oldRun, oldLook })
	lookPath = func(string) (string, error) { return "/usr/bin/wrangler", nil }
	var calls [][]string
	runWrangler = func(_ context.Context, _ io.Writer, args ...string) (string, error) {
		calls = append(calls, args)
		switch {
		case args[0] == "pages" && args[1] == "deploy":
			if len(calls) == 1 {
				return "✘ [ERROR] Project not found. [code: 8000007]", errors.New("exit 1")
			}
			// the uploaded dir must not contain the state dir
			if _, err := os.Stat(filepath.Join(args[2], StateDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("state dir was exported for upload")
			}
			if _, err := os.Stat(filepath.Join(args[2], IndexFile)); err != nil {
				t.Fatal("index.html missing from upload")
			}
			return "✨ Deployment complete! Take a peek over at https://abc123.demo-site.pages.dev", nil
		case args[0] == "pages" && args[1] == "project":
			return "✨ Successfully created", nil
		}
		return "", errors.New("unexpected: " + strings.Join(args, " "))
	}
	url, err := Deploy(context.Background(), dir, "demo-site", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://abc123.demo-site.pages.dev" {
		t.Fatalf("url = %q", url)
	}
	if len(calls) != 3 || calls[1][1] != "project" || calls[1][2] != "create" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDeploy_NoWrangler(t *testing.T) {
	old := lookPath
	t.Cleanup(func() { lookPath = old })
	lookPath = func(string) (string, error) { return "", errors.New("nope") }
	_, err := Deploy(context.Background(), t.TempDir(), "p", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "wrangler not found") {
		t.Fatalf("err = %v", err)
	}
}
