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
		if strings.Contains(r.URL.Path, "/"+StateDir) {
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

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return base + "/", nil
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
