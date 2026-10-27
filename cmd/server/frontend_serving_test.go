package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// The SPA shell answers routes, not files. A tab that still holds an
// older index.html asks for the asset that build named; if the server
// answers with HTML, the browser refuses a script it was told is
// JavaScript and the page comes up blank with nothing in the log.
// And the shell itself is re-read, so a frontend rebuilt under a
// running server stops pointing at assets it has deleted.
func TestServeFrontend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	dist := filepath.Join(dir, "web", "dist")
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dist, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", `<script src="/assets/app-v1.js"></script>`)
	write("assets/app-v1.js", "console.log(1)")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	r := gin.New()
	r.GET("/api/v1/health", func(c *gin.Context) { c.String(http.StatusOK, "api") })
	serveFrontend(r)

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	if w := get("/assets/app-v1.js"); w.Code != http.StatusOK || w.Body.String() != "console.log(1)" {
		t.Fatalf("asset: %d %q", w.Code, w.Body.String())
	} else if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset should be cacheable forever, got %q", cc)
	}
	// The app's own routes get the shell, and it must not be cached.
	w := get("/licenses/abc")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("route: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("the shell must be revalidated, got %q", cc)
	}
	// A route that happens to end in something dot-like is still a
	// route, not a file.
	if w := get("/licenses/1.0"); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("versioned route: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	// The asset a previous build named is gone: say so.
	if w := get("/assets/app-v0.js"); w.Code != http.StatusNotFound {
		t.Fatalf("stale asset: %d, want 404", w.Code)
	}
	if w := get("/favicon.ico"); w.Code != http.StatusNotFound {
		t.Fatalf("missing file: %d, want 404", w.Code)
	}
	// Nothing outside dist is looked up, however the path is spelled. A
	// backslash (%5C) survives path.Clean, and on Windows Join would read
	// it as a separator: the file next to dist would be found, and
	// ServeFile's refusal (400) would tell the caller it exists. Each of
	// these is a route (the shell) or a missing asset (404), never more.
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/../secret.txt", "/..%5Csecret.txt", "/..%5C..%5Csecret.txt",
		"/assets/..%5C..%5C..%5Csecret.txt", "/%2E%2E%5C%2E%2E%5Csecret.txt"} {
		w := get(p)
		shell := w.Code == http.StatusOK && w.Header().Get("Content-Type") == "text/html; charset=utf-8"
		if w.Body.String() == "secret" || (!shell && w.Code != http.StatusNotFound) {
			t.Fatalf("%s: %d %q, want the shell or 404", p, w.Code, w.Header().Get("Content-Type"))
		}
	}
	// API routes are never touched by the fallback.
	if w := get("/api/v1/health"); w.Code != http.StatusOK || w.Body.String() != "api" {
		t.Fatalf("api passthrough: %d %q", w.Code, w.Body.String())
	}
	// A rebuild lands while the server runs: the next page load has
	// to name the new asset, not the deleted one.
	write("index.html", `<script src="/assets/app-v2.js"></script>`)
	if err := os.Remove(filepath.Join(dist, "assets", "app-v1.js")); err != nil {
		t.Fatal(err)
	}
	write("assets/app-v2.js", "console.log(2)")
	if w := get("/"); w.Body.String() != `<script src="/assets/app-v2.js"></script>` {
		t.Fatalf("shell not re-read after a rebuild: %q", w.Body.String())
	}
}
