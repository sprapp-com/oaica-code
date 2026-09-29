// Package sitebuilder turns a one-line brief into a static website, section
// by section, using any OpenAI-compatible chat model behind the LLM
// interface. It is an optional add-on to the oaica CLI (`oaica site ...`);
// nothing in the core launch/run path depends on it.
//
// Pipeline: Plan (one JSON call: title, palette, 4-7 sections) -> one
// bounded HTML call per section, in parallel, stopped at </section> ->
// Sanitize -> Assemble into a single index.html with a built-in
// stylesheet. State (spec + per-section fragments) lives in
// <dir>/.oaica-site so `edit` can regenerate one section without touching
// the others.
package sitebuilder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// Request is one chat completion. System/User map to the two messages.
type Request struct {
	System      string
	User        string
	MaxTokens   int
	Temperature float64
	Stop        []string
}

// LLM is the only thing the builder needs from the outside world.
type LLM interface {
	Complete(ctx context.Context, req Request) (string, error)
}

// Section is one block of the page, generated independently.
type Section struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Nav   string `json:"nav,omitempty"` // short menu label; falls back to Title
	Brief string `json:"brief"`
}

// navLabel is what the header menu shows for a section.
func (s Section) navLabel() string {
	if l := strings.TrimSpace(s.Nav); l != "" {
		return l
	}
	return s.Title
}

// Palette holds the four colours the theme is derived from.
type Palette struct {
	Primary    string `json:"primary"`
	Accent     string `json:"accent"`
	Background string `json:"background"`
	Text       string `json:"text"`
}

// Spec is the plan the model produces before any HTML is written.
type Spec struct {
	Title    string    `json:"title"`
	Tagline  string    `json:"tagline"`
	Language string    `json:"language"`
	Font     string    `json:"font"`
	Palette  Palette   `json:"palette"`
	Sections []Section `json:"sections"`
}

// Site is the persisted state of one generated site.
type Site struct {
	Prompt    string            `json:"prompt"`
	Model     string            `json:"model"`
	Created   time.Time         `json:"created"`
	Updated   time.Time         `json:"updated"`
	Spec      Spec              `json:"spec"`
	Fragments map[string]string `json:"-"`
}

const (
	StateDir     = ".oaica-site"
	siteFile     = "site.json"
	sectionsDir  = "sections"
	IndexFile    = "index.html"
	maxSections  = 8
	minSections  = 3
	planTokens   = 1200
	sectionToken = 1500
	parallelism  = 4
)

var sectionKinds = []string{"hero", "features", "services", "about", "how-it-works", "testimonials", "pricing", "gallery", "faq", "team", "cta", "contact", "custom"}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(slugRe.ReplaceAllString(s, "-"), "-")
	if s == "" {
		return "section"
	}
	return s
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// Progress receives human-readable milestones during Build/Edit. Optional.
type Progress func(msg string)

func (p Progress) log(format string, args ...any) {
	if p != nil {
		p(fmt.Sprintf(format, args...))
	}
}

// ---------- planning ----------

const planSystem = `You are a senior web designer planning a single-page website.
Reply with ONE JSON object and nothing else: no prose, no markdown, no code fences.`

func planUser(prompt string) string {
	return fmt.Sprintf(`Brief: %s

Return JSON with exactly this shape:
{
  "title": "site name (2-4 words)",
  "tagline": "one sentence value proposition",
  "language": "BCP-47 code, e.g. en",
  "font": "sans" | "serif",
  "palette": {"primary": "#hex", "accent": "#hex", "background": "#hex", "text": "#hex"},
  "sections": [
    {"id": "hero", "kind": "hero", "title": "section heading", "nav": "1-2 word menu label", "brief": "what this section must say, concretely, 1-2 sentences"},
    …
  ]
}
Rules: %d to %d sections; the first is kind "hero"; the last is "cta" or "contact";
kinds must be one of %s; ids are unique lowercase slugs; briefs must be specific to THIS brief (names, numbers, offers), never generic filler.`,
		strings.TrimSpace(prompt), minSections, maxSections-1, strings.Join(sectionKinds, ", "))
}

// extractJSON tolerates prose or fences around the object.
func extractJSON(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

func normalizeSpec(sp *Spec) error {
	sp.Title = strings.TrimSpace(sp.Title)
	if sp.Title == "" {
		return errors.New("plan has no title")
	}
	if strings.TrimSpace(sp.Language) == "" {
		sp.Language = "en"
	}
	if len(sp.Sections) == 0 {
		return errors.New("plan has no sections")
	}
	if len(sp.Sections) > maxSections {
		sp.Sections = sp.Sections[:maxSections]
	}
	seen := map[string]bool{}
	for i := range sp.Sections {
		s := &sp.Sections[i]
		s.Kind = strings.ToLower(strings.TrimSpace(s.Kind))
		known := false
		for _, k := range sectionKinds {
			if k == s.Kind {
				known = true
				break
			}
		}
		if !known {
			s.Kind = "custom"
		}
		id := slugify(s.ID)
		if id == "section" {
			id = slugify(s.Kind)
		}
		base := id
		for n := 2; seen[id]; n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		seen[id] = true
		s.ID = id
		if strings.TrimSpace(s.Title) == "" {
			s.Title = titleCase(strings.ReplaceAll(s.Kind, "-", " "))
		}
	}
	return nil
}

// Plan asks the model for the site spec. One retry with the parse error fed
// back, since small models occasionally wrap JSON in commentary.
func Plan(ctx context.Context, llm LLM, prompt string) (Spec, error) {
	user := planUser(prompt)
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := llm.Complete(ctx, Request{System: planSystem, User: user, MaxTokens: planTokens, Temperature: 0.3})
		if err != nil {
			return Spec{}, err
		}
		js := extractJSON(raw)
		var sp Spec
		if js == "" {
			lastErr = errors.New("no JSON object in reply")
		} else if err := json.Unmarshal([]byte(js), &sp); err != nil {
			lastErr = err
		} else if err := normalizeSpec(&sp); err != nil {
			lastErr = err
		} else {
			return sp, nil
		}
		user = planUser(prompt) + fmt.Sprintf("\n\nYour previous reply was rejected (%v). Reply with only the JSON object.", lastErr)
	}
	return Spec{}, fmt.Errorf("plan: %w", lastErr)
}

// ---------- sections ----------

const sectionSystem = `You write one section of a static HTML page.
Output ONLY the HTML for that section: a single <section> element, nothing before or after it. No markdown, no code fences, no commentary.
Hard rules:
- No <style>, <script>, <svg>, <iframe>, inline styles or event handlers. They will be removed.
- Use only these CSS classes: %s. The page already has a stylesheet.
- Images: use <div class="ph" aria-hidden="true"></div> as a placeholder block, never an <img> with an invented URL.
- Internal links are "#<section-id>". Buttons are <a class="btn btn-primary" href="#…">.
- Write real, specific copy for this business: 40-160 words. No lorem ipsum, no placeholder brackets.
- Never repeat a line or list item.`

func sectionUser(sp Spec, sec Section, existing, instruction string) string {
	var ids []string
	for _, s := range sp.Sections {
		ids = append(ids, s.ID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Site: %s — %s\nLanguage: %s\nAll section ids on the page: %s\n\n", sp.Title, sp.Tagline, sp.Language, strings.Join(ids, ", "))
	fmt.Fprintf(&b, "Write section id=%q kind=%q titled %q.\nBrief: %s\n", sec.ID, sec.Kind, sec.Title, sec.Brief)
	switch sec.Kind {
	case "hero":
		b.WriteString("Layout: <section id=\"…\" class=\"sec sec-hero\"><div class=\"container grid grid-2\"> text column (eyebrow, h1, lead paragraph, two buttons) + <div class=\"ph\"></div> </div></section>. Use the ONLY <h1> on the page here.\n")
	case "cta":
		b.WriteString("Layout: <section class=\"sec sec-cta\"><div class=\"container center stack\"> h2, one paragraph, one button </div></section>.\n")
	case "faq":
		b.WriteString("Layout: 4-6 <details class=\"faq\"><summary>question</summary><p>answer</p></details> inside the container.\n")
	case "pricing":
		b.WriteString("Layout: grid-3 of cards; each card: h3 plan name, <p class=\"price\">, a <ul> of 3-4 features, a button.\n")
	case "testimonials":
		b.WriteString("Layout: grid-3 of cards; each card: <blockquote> then <p class=\"muted\">name, role</p>. Invent plausible but clearly local names.\n")
	case "contact":
		b.WriteString("Layout: grid-2: left column address/phone/hours in <address>; right column a <form> with name, email, message and a submit button.\n")
	default:
		b.WriteString("Layout: <section class=\"sec\"><div class=\"container\"> eyebrow, h2, lead, then a grid-3 of cards (h3 + p) or a stack of paragraphs. </div></section>\n")
	}
	if existing != "" {
		fmt.Fprintf(&b, "\nCurrent HTML of this section:\n%s\n\nRewrite the whole section applying this change: %s\n", existing, strings.TrimSpace(instruction))
	}
	b.WriteString("\nStart your reply with <section")
	return b.String()
}

// GenerateSection produces one sanitized fragment. existing+instruction turn
// it into an edit.
func GenerateSection(ctx context.Context, llm LLM, sp Spec, sec Section, existing, instruction string) (string, error) {
	req := Request{
		System:      fmt.Sprintf(sectionSystem, classVocabulary),
		User:        sectionUser(sp, sec, existing, instruction),
		MaxTokens:   sectionToken,
		Temperature: 0.5,
		Stop:        []string{"</section>"},
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := llm.Complete(ctx, req)
		if err != nil {
			return "", err
		}
		frag := Sanitize(raw)
		if frag == "" {
			lastErr = fmt.Errorf("section %q: model returned no <section> element", sec.ID)
			req.User += "\n\nYour previous reply contained no <section> element. Reply with the <section> HTML only."
			continue
		}
		return ensureSectionID(frag, sec), nil
	}
	return "", lastErr
}

// sectionTagEnd returns the index of the '>' closing a LEADING <section ...>
// opening tag, or -1 when frag does not begin with one.
//
// Every offset here comes from frag itself. strings.ToLower can change a
// rune's UTF-8 byte length, so an offset taken from a folded copy indexes the
// wrong byte of the original — and a fragment that came out of Sanitize and
// is spliced with that offset puts attribute-VALUE bytes into attribute-NAME
// position, producing a live event handler in the stored page (2026-09-26
// audit).
func sectionTagEnd(frag string) int {
	const open = "<section"
	if len(frag) < len(open) || !strings.EqualFold(frag[:len(open)], open) {
		return -1
	}
	if len(frag) > len(open) {
		switch frag[len(open)] {
		case ' ', '\t', '\n', '\r', '>', '/':
		default:
			return -1
		}
	}
	return strings.IndexByte(frag, '>')
}

// ensureSectionID forces the id the plan assigned so nav anchors always
// resolve, and the kind class so the theme applies.
func ensureSectionID(frag string, sec Section) string {
	end := sectionTagEnd(frag)
	if end < 0 {
		return frag
	}
	classes := "sec sec-" + sec.Kind
	open := frag[:end]
	// drop any existing id/class from the opening tag, then re-add ours
	open = regexp.MustCompile(`(?i)\s+(id|class)="[^"]*"`).ReplaceAllString(open, "")
	rebuilt := fmt.Sprintf(`%s id="%s" class="%s"%s`, open, html.EscapeString(sec.ID), classes, frag[end:])
	// The splice writes into attribute position, so re-assert the allowlist on
	// the result: nothing reaches the store that a browser parses as a handler
	// or as a URL the sanitizer would have refused.
	if rendered := renderAllowlisted(rebuilt); rendered != "" {
		return rendered
	}
	return rebuilt
}

// ---------- build / edit ----------

// Build plans and generates every section. Sections run in parallel
// (bounded) because they are independent by construction.
func Build(ctx context.Context, llm LLM, prompt, model string, progress Progress) (*Site, error) {
	progress.log("planning site structure…")
	sp, err := Plan(ctx, llm, prompt)
	if err != nil {
		return nil, err
	}
	progress.log("plan: %q — %d sections (%s)", sp.Title, len(sp.Sections), sectionIDs(sp))

	site := &Site{Prompt: prompt, Model: model, Created: time.Now(), Updated: time.Now(), Spec: sp, Fragments: map[string]string{}}
	results := make([]string, len(sp.Sections))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallelism)
	for i, sec := range sp.Sections {
		g.Go(func() error {
			t0 := time.Now()
			frag, err := GenerateSection(gctx, llm, sp, sec, "", "")
			if err != nil {
				return err
			}
			results[i] = frag
			progress.log("  section %-14s %5d bytes  %.1fs", sec.ID, len(frag), time.Since(t0).Seconds())
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for i, sec := range sp.Sections {
		site.Fragments[sec.ID] = results[i]
	}
	return site, nil
}

func sectionIDs(sp Spec) string {
	ids := make([]string, 0, len(sp.Sections))
	for _, s := range sp.Sections {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ", ")
}

// ChooseSection asks the model which existing section an edit instruction
// targets. Used when the user did not say.
func ChooseSection(ctx context.Context, llm LLM, sp Spec, instruction string) (string, error) {
	var lines []string
	for _, s := range sp.Sections {
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", s.ID, s.Kind, s.Title))
	}
	raw, err := llm.Complete(ctx, Request{
		System:      "You route edit requests to the right section of a web page. Reply with the section id only.",
		User:        fmt.Sprintf("Sections:\n%s\n\nEdit request: %s\n\nWhich single section id does this change? Reply with the id only.", strings.Join(lines, "\n"), instruction),
		MaxTokens:   30,
		Temperature: 0,
	})
	if err != nil {
		return "", err
	}
	got := slugify(strings.Trim(strings.TrimSpace(thinkRe.ReplaceAllString(raw, "")), "`\"' ."))
	for _, s := range sp.Sections {
		if s.ID == got {
			return s.ID, nil
		}
	}
	// tolerate "the hero section" style replies
	for _, s := range sp.Sections {
		if strings.Contains(got, s.ID) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("could not map %q to a section (have: %s)", got, sectionIDs(sp))
}

// Edit regenerates one section with an instruction.
func (s *Site) Edit(ctx context.Context, llm LLM, sectionID, instruction string, progress Progress) error {
	var sec *Section
	for i := range s.Spec.Sections {
		if s.Spec.Sections[i].ID == sectionID {
			sec = &s.Spec.Sections[i]
		}
	}
	if sec == nil {
		return fmt.Errorf("no section %q (have: %s)", sectionID, sectionIDs(s.Spec))
	}
	progress.log("regenerating section %s…", sec.ID)
	frag, err := GenerateSection(ctx, llm, s.Spec, *sec, s.Fragments[sec.ID], instruction)
	if err != nil {
		return err
	}
	s.Fragments[sec.ID] = frag
	s.Updated = time.Now()
	return nil
}

// ---------- assembly ----------

// Assemble renders the full index.html. Header, nav and footer are
// deterministic; only section bodies come from the model.
func Assemble(s *Site) string {
	sp := s.Spec
	var nav strings.Builder
	n := 0
	for _, sec := range sp.Sections {
		if sec.Kind == "hero" || n >= 6 {
			continue
		}
		// Only link to a section that is actually on the page. A missing (not
		// yet generated, or unreadable) fragment otherwise left the header
		// advertising an anchor with no target — a dead link shipped to the
		// public site (2026-09-26 audit, fourth round).
		if frag, ok := s.Fragments[sec.ID]; !ok || strings.TrimSpace(frag) == "" {
			continue
		}
		fmt.Fprintf(&nav, `<a href="#%s">%s</a>`, html.EscapeString(sec.ID), html.EscapeString(sec.navLabel()))
		n++
	}
	var body strings.Builder
	for _, sec := range sp.Sections {
		if frag, ok := s.Fragments[sec.ID]; ok && frag != "" {
			body.WriteString(frag)
			body.WriteString("\n")
		}
	}
	desc := sp.Tagline
	if desc == "" {
		desc = sp.Title
	}
	return fmt.Sprintf(`<!doctype html>
<html lang="%s">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<meta name="description" content="%s">
<meta name="generator" content="oaica site v1">
<style>
%s</style>
</head>
<body>
<header class="site-header"><div class="container"><a class="brand" href="#top">%s</a><nav class="site-nav">%s</nav></div></header>
<main id="top">
%s</main>
<footer class="site-footer"><div class="container">© %d %s</div></footer>
</body>
</html>
`, html.EscapeString(sp.Language), html.EscapeString(sp.Title), html.EscapeString(desc), baseCSS(sp),
		html.EscapeString(sp.Title), nav.String(), body.String(), time.Now().Year(), html.EscapeString(sp.Title))
}

// ---------- persistence ----------

// fragmentFileName is the file a section fragment is stored under, refusing
// any id that is not a bare slug.
//
// Section ids reach this package from two places it does not control: the
// planner model's JSON, and site.json as it exists on disk (hand-edited,
// restored from a backup, or written by an older version). Both were fed
// straight into filepath.Join(st, sectionsDir, id+".html"), so an id of
// "../../../.config/…" wrote — and Load read — outside the site directory
// (2026-09-26 audit, third round). slugify is the normalizer the PLAN path
// already ran ids through; this applies the same rule where the id is about to
// become a path component.
func fragmentFileName(id string) (string, error) {
	if id != slugify(id) {
		return "", fmt.Errorf("section id %q is not a plain slug (expected e.g. \"hero\" or \"how-it-works\")", id)
	}
	return id + ".html", nil
}

// writeFileAtomic writes data to path through a temp file in the same
// directory followed by a rename, so a reader ever sees either the old file or
// the new one and never a half-written one. Save used plain os.WriteFile, and
// a torn fragment read back as an empty section that the next Save then
// rewrote as empty — content lost with no error (2026-09-26 audit, fourth
// round).
//
// The rename REPLACES a symlinked destination rather than writing through it,
// which is deliberate and is the opposite of the write-through policy the
// store writers in cmd/internal/fileutil follow — those write files the user
// owns and may have linked into a git-managed directory. A site directory is
// not that: it can arrive from a backup, a clone or a download, and every
// other path here treats a symlink inside it as hostile (checkStateDir refuses
// a symlinked state dir, Export skips symlinks). Writing through one would let
// a site directory name any path on the machine and have `oaica site edit`
// update it. TestSaveWritesThroughARename pins this.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename succeeded
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// checkStateDir refuses a state directory that is a symlink, or that is not a
// directory at all.
//
// The state path is a fixed, well-known name, and os.MkdirAll/ReadFile follow
// it. A site directory that arrived from a backup, a clone or a download can
// therefore carry ".oaica-site -> /home/you" (or -> ".") and steer `oaica site
// edit` / `new` into writing site.json and section files outside the site tree
// — and, when the link points back inside it, into a layout Export no longer
// recognises as state, so the brief and the whole plan were uploaded to the
// public host (2026-09-26 audit, fourth round).
func checkStateDir(st string) error {
	fi, err := os.Lstat(st)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the %s directory is a symlink (%s) — refusing to read or write state through it; a site's state must live inside the site directory", StateDir, st)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", st)
	}
	// The sections directory inside it is followed by MkdirAll, ReadDir, Remove and every fragment write
	// just the same: a clone with `sections -> ~` had Save delete every file of that directory that was
	// not a section id and write hero.html into it (2026-09-29 audit, round 127, F127-L1-7).
	sec := filepath.Join(st, sectionsDir)
	if sfi, err := os.Lstat(sec); err == nil {
		if !sfi.IsDir() { // Lstat: a symlink is not a directory here
			return fmt.Errorf("%s is not a plain directory (a symlink?) — refusing to read or write state through it", sec)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Save writes the state and the assembled index.html into dir.
//
// Every file goes through writeFileAtomic, and the write order is deliberate:
// the fragments, then index.html, then site.json as the last thing written.
// index.html is a DERIVATION of the state, so the state is the commit point —
// a run that dies partway leaves a page that Load re-derives on the next read
// (see Load's reconciliation step) rather than a state that describes a page
// nobody has.
func (s *Site) Save(dir string) error {
	st := filepath.Join(dir, StateDir)
	if err := checkStateDir(st); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(st, sectionsDir), 0o755); err != nil {
		return err
	}
	// clear stale fragments from a previous plan
	entries, _ := os.ReadDir(filepath.Join(st, sectionsDir))
	for _, e := range entries {
		if _, keep := s.Fragments[strings.TrimSuffix(e.Name(), ".html")]; !keep {
			_ = os.Remove(filepath.Join(st, sectionsDir, e.Name()))
		}
	}
	// A fragment whose id cannot become a file name is refused BEFORE anything
	// is written, so a bad plan cannot leave a half-updated sections/ behind.
	names := make(map[string]string, len(s.Fragments))
	for id := range s.Fragments {
		name, err := fragmentFileName(id)
		if err != nil {
			return err
		}
		names[id] = name
	}
	for id, frag := range s.Fragments {
		if err := writeFileAtomic(filepath.Join(st, sectionsDir, names[id]), []byte(frag), 0o644); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(filepath.Join(dir, IndexFile), []byte(Assemble(s)), 0o644); err != nil {
		return err
	}
	meta, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(st, siteFile), meta, 0o644)
}

// Load reads a site previously written by Save.
func Load(dir string) (*Site, error) {
	st := filepath.Join(dir, StateDir)
	if err := checkStateDir(st); err != nil {
		return nil, err
	}
	meta, err := os.ReadFile(filepath.Join(st, siteFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s is not an oaica site (no %s/%s); run `oaica site new` first", dir, StateDir, siteFile)
		}
		return nil, err
	}
	var s Site
	if err := json.Unmarshal(meta, &s); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", siteFile, err)
	}
	// site.json is on-disk state, not model output: the ids below are about to
	// become path components, and a hand-edited or restored file is exactly
	// where an id of "../.." would come from. normalizeSpec is the same rule
	// the plan path applies (and is idempotent for ids it already wrote).
	if err := normalizeSpec(&s.Spec); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", siteFile, err)
	}
	s.Fragments = map[string]string{}
	for _, sec := range s.Spec.Sections {
		name, err := fragmentFileName(sec.ID)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", siteFile, err)
		}
		b, err := os.ReadFile(filepath.Join(st, sectionsDir, name))
		switch {
		case err == nil:
			s.Fragments[sec.ID] = sanitizeStoredFragment(string(b))
		case errors.Is(err, os.ErrNotExist):
			// No fragment yet (a plan that has not been generated, or a
			// section added since). Assemble skips it and the caller
			// regenerates it; this is not an error.
		default:
			// Any OTHER read failure used to be treated as "no fragment",
			// which is how one unreadable file turned into a silent deletion:
			// Load returned a Site missing that section, and Save's
			// stale-fragment cleanup then removed the file it could not read.
			// The published page lost a section, site.json still listed it,
			// and the nav kept an anchor with no target (2026-09-26 audit,
			// fourth round).
			return nil, fmt.Errorf("cannot read %s/%s: %w", StateDir+"/"+sectionsDir, name, err)
		}
	}
	if err := reconcileIndex(dir, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// reconcileIndex makes index.html match the state it is derived from.
//
// index.html is what `preview` serves and `deploy` uploads, but it is a pure
// function of site.json plus the fragments — and it used to be the LAST thing
// Save wrote, so a run that failed at that write (a read-only file, a full
// disk) left the state ahead of the page: the user saw "updated" and a deploy
// published the previous site. Re-deriving it here means every path that reads
// a site also repairs that divergence.
func reconcileIndex(dir string, s *Site) error {
	want := Assemble(s)
	path := filepath.Join(dir, IndexFile)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == want {
		return nil
	}
	return writeFileAtomic(path, []byte(want), 0o644)
}

// sanitizeStoredFragment re-applies the allowlist to a fragment read off
// disk. Stored state is data like any other: a section file may predate the
// current pipeline (an older version's output, a restored backup, a hand edit,
// a site directory received from someone), and Load is the single place every
// read path funnels through. Sanitize is preferred because it also isolates
// the section element; a fragment with no <section> is still re-rendered
// through the allowlist rather than passed through verbatim.
func sanitizeStoredFragment(b string) string {
	if strings.TrimSpace(b) == "" {
		return ""
	}
	if frag := Sanitize(b); frag != "" {
		return frag
	}
	return strings.TrimSpace(renderAllowlisted(b))
}

// Export copies the publishable files (everything except StateDir) into dst.
// Deploy uses this so the prompt/spec never leave the machine.
//
// What counts as publishable is a whitelist-shaped decision, not "everything
// else": dst is uploaded to a public host. Three classes of file are skipped
// (2026-09-26 audit, third round):
//
//   - the state directory itself (the brief, the plan, the model's raw output);
//   - dotfiles and dot-directories, which is where credentials live in every
//     convention that matters here (.env, .dev.vars, .wrangler/, .npmrc,
//     .netrc, .git/). `.well-known` is the one exception — it is published
//     content by definition;
//   - anything whose name marks it as a key or secret, regardless of
//     directory (a pem, an id_rsa, a secrets.json);
//   - symlinks, because following one can copy a file from outside the site
//     (a link to ~/.ssh/id_rsa was uploaded under a harmless-looking name).
func Export(dir, dst string) error {
	// WalkDir lstats the ROOT it is given, so a symlinked root (a "live" link
	// to the real site dir, which is what shell completion and a symlinked
	// deploy path produce) is walked as a FILE: no child is ever visited, the
	// upload is empty, and Export still returned nil — Deploy then printed a
	// *.pages.dev URL for a site that was never uploaded (2026-09-26 audit,
	// fourth round).
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if d.IsDir() {
			if d.Name() == StateDir || d.Name() == ".git" || d.Name() == "node_modules" || isPrivateName(d.Name()) {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 || isPrivateName(d.Name()) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
}

// privateFileRe matches names that are a credential or a private key whatever
// directory they sit in.
var privateFileRe = regexp.MustCompile(`(?i)^(` +
	// dotfile-shaped secrets, in the spelling that has no leading dot too
	`\.env.*|env\.local|\.dev\.vars.*|\.?npmrc|\.?netrc|\.git-credentials|` +
	// names that say what they are
	`secrets?(\.(json|txt|yaml|yml|toml|ini))?|credentials?(\.(json|txt|yaml|yml|toml|ini))?|` +
	`tokens?(\.(json|txt))?|api[-_.]?keys?(\.(json|txt))?|auth\.json|` +
	`.*keystore.*|\.?truststore|` +
	// key material and certificates
	`.*\.(pem|key|p12|pfx|jks|crt|cer|der|asc|gpg)|id_(rsa|dsa|ecdsa|ed25519)|` +
	// tooling that carries an account id or a deploy credential
	`deploy\.sh|wrangler\.(toml|jsonc?)|cloudflare\.(toml|json)|` +
	// the site's own state file, whatever directory it turned up in
	`site\.json` +
	`)$`)

// isPrivateName reports whether a file or directory name must never be
// uploaded to a public host.
func isPrivateName(name string) bool {
	if name == ".well-known" {
		return false // published content by definition, despite the leading dot
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	return privateFileRe.MatchString(name)
}

// SortedIDs is a stable listing helper for the CLI.
func (s *Site) SortedIDs() []string {
	ids := make([]string, 0, len(s.Spec.Sections))
	for _, sec := range s.Spec.Sections {
		ids = append(ids, sec.ID)
	}
	sort.Strings(ids)
	return ids
}
