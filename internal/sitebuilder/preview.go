package sitebuilder

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"path/filepath"
	"strings"
)

// Preview serves dir on 127.0.0.1 and returns the URL of a wrapper page that
// shows the site inside a fully sandboxed iframe (sandbox="" — no scripts,
// no forms, no same-origin access). Generated sites contain no scripts by
// construction (see Sanitize), so the sandbox costs nothing and guarantees
// that even a fragment the sanitizer missed cannot run in the previewer's
// browser context. The state directory is never served.
//
// The server stops when ctx is cancelled.
func Preview(ctx context.Context, dir string, port int) (string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	base := "http://" + ln.Addr().String()

	fs := http.FileServer(http.Dir(dir))
	mux := http.NewServeMux()
	mux.HandleFunc("/site/", func(w http.ResponseWriter, r *http.Request) {
		// Resolved, not pattern-matched. The old check was
		// strings.Contains(r.URL.Path, "/"+StateDir), which a SYMLINK walks
		// straight past: `ln -s .oaica-site alias` then GET
		// /site/alias/site.json served the brief and the whole plan, because
		// that path contains no "/.oaica-site" (2026-09-26 audit, third
		// round). The state directory must stay private however it is
		// addressed.
		if !servableUnder(dir, r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.StripPrefix("/site/", fs).ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, previewPage(filepath.Base(dir)))
	})

	srv := &http.Server{Handler: previewHostGuard(mux)}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return base + "/", nil
}

// servableUnder reports whether the request path may be served out of root.
// It answers by RESOLVING the path — filepath.Join cleans "..", EvalSymlinks
// resolves every symlink in the deepest existing ancestor of the target — and
// then requiring the resolved location to be inside root and outside root's
// state directory.
//
// The rule is about PATHS, and it says so: a HARDLINK to a state file sits at
// an ordinary path and resolves there, so it is served like any other file
// name. That is not a hole in this function but a limit of path-based
// filtering — the link is created by the same user, on the same filesystem,
// with the same file permissions as the original, so it confers no access they
// did not already have (2026-09-26 audit, fourth round: documented rather than
// changed, because the alternative — inode comparison against every state
// file, per request — buys nothing an ordinary copy would not also defeat).
func servableUnder(root, urlPath string) bool {
	rel := strings.TrimPrefix(urlPath, "/site/")
	// Export refuses to publish dotfiles (.env, .git, .dev.vars) and secret-shaped names; the preview must not
	// hand them to a browser either (2026-09-29 audit, round 130, F130-L2-3).
	for _, part := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if isPrivateName(part) || part == "node_modules" {
			return false
		}
	}
	full := filepath.Join(root, filepath.FromSlash(rel))

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	// Walk up to the deepest ancestor that exists (the target itself may not,
	// which is a 404 for the file server to answer, not a path to guess at).
	probe := full
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			tail := strings.TrimPrefix(strings.TrimPrefix(full, probe), string(filepath.Separator))
			target := filepath.Join(resolved, tail)
			r, rerr := filepath.Rel(realRoot, target)
			if rerr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return false
			}
			return r != StateDir && !strings.HasPrefix(r, StateDir+string(filepath.Separator))
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return false
		}
		probe = parent
	}
}

func previewPage(name string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>oaica site preview — %[1]s</title>
<style>
html,body{margin:0;height:100%%;font-family:system-ui,sans-serif;background:#111;color:#ddd}
.bar{display:flex;gap:1rem;align-items:center;padding:.5rem .9rem;background:#1c1c1c;border-bottom:1px solid #333;font-size:.9rem}
.bar b{color:#fff}.bar button{font:inherit;padding:.3rem .7rem;border-radius:6px;border:1px solid #555;background:#222;color:#eee;cursor:pointer}
.bar .w{margin-left:auto;display:flex;gap:.4rem}
iframe{border:0;width:100%%;height:calc(100%% - 42px);background:#fff;display:block;margin:0 auto}
</style></head><body>
<div class="bar"><b>oaica site</b> <span>%[1]s</span> <button onclick="f.contentWindow.location.reload()">reload</button>
<span class="w"><button onclick="f.style.width='100%%'">desktop</button><button onclick="f.style.width='820px'">tablet</button><button onclick="f.style.width='390px'">phone</button></span></div>
<iframe id="f" sandbox="" src="/site/" title="site preview"></iframe>
</body></html>`, html.EscapeString(name))
}

// previewHostGuard answers only requests addressed to this machine: a page on another origin whose DNS name was
// rebound to 127.0.0.1 sends its own name as Host, and the preview would otherwise serve the site to it
// (2026-09-29 audit, round 130, F130-L2-3).
func previewHostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		if ip := net.ParseIP(host); host != "localhost" && !strings.HasSuffix(host, ".localhost") && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
