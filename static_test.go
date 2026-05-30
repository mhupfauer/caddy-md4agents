package md4agents

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

func newTestModule(t *testing.T, root string) *MarkdownForAgents {
	t.Helper()
	m := &MarkdownForAgents{
		Root:           root,
		CacheDir:       filepath.Join(t.TempDir(), "cache"),
		ConvertTimeout: caddy.Duration(5 * time.Second),
	}
	cache, err := newCache(64, 0, 64<<20, 8<<20)
	if err != nil {
		t.Fatalf("cache: %v", err)
	}
	m.cache = cache
	m.sem = make(chan struct{}, 4)
	m.MaxBodyBytes = 4 << 20
	if abs, err := filepath.EvalSymlinks(root); err == nil {
		m.rootResolved = abs
	}
	if abs, err := filepath.EvalSymlinks(m.CacheDir); err == nil {
		m.cacheResolved = abs
	} else if abs, err := filepath.Abs(m.CacheDir); err == nil {
		m.cacheResolved = abs
		_ = ensureDir(m.cacheResolved)
		if r, err := filepath.EvalSymlinks(m.cacheResolved); err == nil {
			m.cacheResolved = r
		}
	}
	m.log = zap.NewNop()
	m.URLSuffix = ".md"
	m.QueryParam = "format"
	m.StripTags = []string{"script", "style"}
	m.conv = m.buildConverter()
	return m
}

func TestServeStaticAuthorMarkdown(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "docs", "page.md"), "# Hand-written\n")

	m := newTestModule(t, root)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/docs/page.md", nil)

	served, err := m.serveStatic(rec, req, "/docs/page.html")
	if err != nil || !served {
		t.Fatalf("served=%v err=%v", served, err)
	}
	if !strings.Contains(rec.Body.String(), "Hand-written") {
		t.Fatalf("expected author content, got %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestServeStaticGeneratesAndCaches(t *testing.T) {
	root := t.TempDir()
	html := `<html><body><h1>Hello</h1><p>World</p><script>x()</script></body></html>`
	mustWrite(t, filepath.Join(root, "docs", "page.html"), html)

	m := newTestModule(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/docs/page.md", nil)
	if served, err := m.serveStatic(rec, req, "/docs/page.html"); err != nil || !served {
		t.Fatalf("served=%v err=%v", served, err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Hello") || !strings.Contains(body, "World") {
		t.Fatalf("conversion missing content: %q", body)
	}
	if strings.Contains(body, "x()") {
		t.Fatalf("script tag survived: %q", body)
	}

	sidecar := filepath.Join(m.CacheDir, "docs", "page.html.md")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
}

func TestServeStaticSidecarReusedUntilSourceChanges(t *testing.T) {
	root := t.TempDir()
	srcPath := filepath.Join(root, "page.html")
	mustWrite(t, srcPath, `<html><body><p>v1</p></body></html>`)
	m := newTestModule(t, root)

	// Prime sidecar.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/page.md", nil)
	if _, err := m.serveStatic(rec, req, "/page.html"); err != nil {
		t.Fatal(err)
	}

	// Tamper with sidecar: replace its content. If our resolver reuses the
	// sidecar (mtime >= source) we should see the tampered text — proving
	// no re-conversion happened. Bump sidecar mtime to be safely after source.
	sidecar := filepath.Join(m.CacheDir, "page.html.md")
	mustWrite(t, sidecar, "tampered\n")
	bumpMtime(t, sidecar, time.Now().Add(time.Hour))

	// Drop in-memory cache so the resolver has to hit disk.
	m.cache, _ = newCache(64, 0, 64<<20, 8<<20)

	rec2 := httptest.NewRecorder()
	if _, err := m.serveStatic(rec2, req, "/page.html"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec2.Body.String(), "tampered") {
		t.Fatalf("expected tampered sidecar to be served, got %q", rec2.Body.String())
	}

	// Now bump source mtime past sidecar — resolver should re-convert and
	// overwrite. Use the real "v1" body so we know conversion ran.
	bumpMtime(t, srcPath, time.Now().Add(2*time.Hour))
	m.cache, _ = newCache(64, 0, 64<<20, 8<<20)

	rec3 := httptest.NewRecorder()
	if _, err := m.serveStatic(rec3, req, "/page.html"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec3.Body.String(), "tampered") {
		t.Fatalf("stale sidecar served after source change: %q", rec3.Body.String())
	}
	if !strings.Contains(rec3.Body.String(), "v1") {
		t.Fatalf("expected regenerated content, got %q", rec3.Body.String())
	}
}

func TestServeStatic304(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "p.html"), `<html><body><p>hi</p></body></html>`)
	m := newTestModule(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/p.md", nil)
	if _, err := m.serveStatic(rec, req, "/p.html"); err != nil {
		t.Fatal(err)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/p.md", nil)
	req2.Header.Set("If-None-Match", etag)
	if _, err := m.serveStatic(rec2, req2, "/p.html"); err != nil {
		t.Fatal(err)
	}
	if rec2.Code != 304 {
		t.Fatalf("expected 304, got %d", rec2.Code)
	}
}

func TestServeStaticDirectoryIndex(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "docs", "index.html"),
		`<html><body><h1>Index</h1></body></html>`)

	m := newTestModule(t, root)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/docs/", nil)
	served, err := m.serveStatic(rec, req, "/docs/")
	if err != nil || !served {
		t.Fatalf("served=%v err=%v", served, err)
	}
	if !strings.Contains(rec.Body.String(), "Index") {
		t.Fatalf("expected index conversion, got %q", rec.Body.String())
	}
}

func TestServeStaticSingleflight(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "p.html"),
		`<html><body><p>concurrent</p></body></html>`)
	m := newTestModule(t, root)

	// Drive N concurrent requests for the same page; they must share a
	// single conversion. We don't have a hook to count conversions, but we
	// can at least assert no panics, identical bodies, and a populated
	// sidecar afterward.
	const N = 16
	type res struct {
		body string
		err  error
	}
	out := make(chan res, N)
	for i := 0; i < N; i++ {
		go func() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/p.md", nil)
			_, err := m.serveStatic(rec, req, "/p.html")
			out <- res{rec.Body.String(), err}
		}()
	}
	first := <-out
	if first.err != nil {
		t.Fatal(first.err)
	}
	for i := 1; i < N; i++ {
		r := <-out
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.body != first.body {
			t.Fatalf("divergent body under concurrency: %q vs %q", first.body, r.body)
		}
	}
	sidecar := filepath.Join(m.CacheDir, "p.html.md")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar missing after concurrent run: %v", err)
	}
}

func TestConverterNestedSelectorRemoval(t *testing.T) {
	m := &MarkdownForAgents{StripSelectors: []string{".ad"}}
	m.conv = m.buildConverter()
	// Nested matches: removing the outer should be enough; the inner must
	// not also be processed against a stale Parent pointer.
	html := `<html><body>
	  <p>keep me</p>
	  <div class="ad"><span class="ad">nested</span> outer-ad</div>
	</body></html>`
	md, err := m.convert([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "keep me") {
		t.Errorf("kept content missing: %q", md)
	}
	if strings.Contains(md, "outer-ad") || strings.Contains(md, "nested") {
		t.Errorf("ad subtree not stripped: %q", md)
	}
}

// --- helpers -----------------------------------------------------------------

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bumpMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}
