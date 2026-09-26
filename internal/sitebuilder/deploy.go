package sitebuilder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// checkUploadable refuses a tree that must not be published as a site.
//
// A Pages deploy of a directory with no index.html is a "successful" deploy of
// an empty site: wrangler uploads what it is given and prints a URL. Export
// can legitimately produce such a tree (a site whose index was never written,
// an upload filtered down to nothing), so the deploy checks it rather than
// trusting the caller (2026-09-26 audit, fourth round).
func checkUploadable(dst string) error {
	fi, err := os.Stat(filepath.Join(dst, IndexFile))
	if err != nil {
		return fmt.Errorf("refusing to deploy: nothing to publish — the exported site has no %s", IndexFile)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("refusing to deploy: %s is not a regular file", IndexFile)
	}
	return nil
}

// Deploy publishes dir to Cloudflare Pages via the wrangler CLI, creating
// the project on first use. Only the exported files go up (Export skips
// the state dir, so the brief and plan stay local).
//
// wrangler must be installed and authenticated (CLOUDFLARE_API_TOKEN or
// `wrangler login`); this deliberately does not reimplement the Pages API.

var pagesURLRe = regexp.MustCompile(`https://[a-z0-9.-]+\.pages\.dev\S*`)

// runWrangler is a package var so tests can stub the CLI.
var runWrangler = func(ctx context.Context, stdout io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "wrangler", args...)
	cmd.Stdin = nil
	var buf strings.Builder
	w := io.MultiWriter(&buf, stdout)
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	return buf.String(), err
}

var lookPath = exec.LookPath

// Deploy returns the *.pages.dev URL wrangler reports.
func Deploy(ctx context.Context, dir, project string, stdout io.Writer) (string, error) {
	if strings.TrimSpace(project) == "" {
		return "", errors.New("project name is required")
	}
	if _, err := lookPath("wrangler"); err != nil {
		return "", errors.New("wrangler not found in PATH — install with `npm i -g wrangler` and run `wrangler login` (or set CLOUDFLARE_API_TOKEN)")
	}
	if _, err := os.Stat(dir); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "oaica-site-export-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := Export(dir, tmp); err != nil {
		return "", fmt.Errorf("export: %w", err)
	}
	// Refuse to publish a tree with no index.html. Export can legitimately
	// produce one (a symlinked root before that was fixed, a site whose index
	// was never written, an upload filtered down to nothing), and wrangler
	// will happily deploy it and print a URL — a "successful" deploy of an
	// empty site is worse than a failed one (2026-09-26 audit, fourth round).
	if err := checkUploadable(tmp); err != nil {
		return "", err
	}

	deploy := []string{"pages", "deploy", tmp, "--project-name", project, "--commit-dirty=true"}
	out, err := runWrangler(ctx, stdout, deploy...)
	if err != nil && strings.Contains(out, "Project not found") {
		fmt.Fprintf(stdout, "creating Pages project %q…\n", project)
		if _, cerr := runWrangler(ctx, stdout, "pages", "project", "create", project, "--production-branch", "main"); cerr != nil {
			return "", fmt.Errorf("create project: %w", cerr)
		}
		out, err = runWrangler(ctx, stdout, deploy...)
	}
	if err != nil {
		return "", fmt.Errorf("wrangler pages deploy: %w", err)
	}
	if u := pagesURLRe.FindString(out); u != "" {
		return u, nil
	}
	return "", errors.New("deploy finished but wrangler printed no *.pages.dev URL")
}
