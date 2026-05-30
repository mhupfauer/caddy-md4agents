package md4agents

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSymlinkEscapeBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	// secret outside the root that file_server-style semantics would have
	// happily followed via symlink.
	if err := os.WriteFile(filepath.Join(outside, "secret.html"),
		[]byte("<html><body>SECRET</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "esc")); err != nil {
		t.Fatal(err)
	}

	m := newTestModule(t, root)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/esc/secret.md", nil)
	served, err := m.serveStatic(rec, req, "/esc/secret.html")
	if err != nil {
		t.Fatal(err)
	}
	// Refuse-escape returns served=true with a 404 so the dynamic capture
	// path can't fall through and serve the converted secret.
	if !served {
		t.Fatal("escape must be refused at the static layer, not fall through")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("secret leaked: %q", rec.Body.String())
	}
}

func TestHEADSuppressesBody(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "p.html"), `<html><body>hi</body></html>`)
	m := newTestModule(t, root)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/p.md", nil)
	if _, err := m.serveStatic(rec, req, "/p.html"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD must not include body, got %q", rec.Body.String())
	}
	if cl := rec.Header().Get("Content-Length"); cl == "" || cl == "0" {
		t.Fatalf("HEAD should still advertise Content-Length, got %q", cl)
	}
}

func TestCacheByteBudgetEvicts(t *testing.T) {
	c, err := newCache(100, 0, 1000, 600)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, n int) {
		c.put(name, &entry{Markdown: make([]byte, n)})
	}
	mk("a", 500) // bytes=500
	mk("b", 500) // bytes=1000
	mk("c", 500) // would exceed; should evict 'a'
	_, hasA := c.get("a")
	_, hasC := c.get("c")
	if hasA {
		t.Errorf("oldest entry 'a' should have been evicted")
	}
	if !hasC {
		t.Errorf("newest entry 'c' should be present")
	}
	_, bytes := c.stats()
	if bytes > 1000 {
		t.Errorf("byte budget violated: %d", bytes)
	}
}

func TestCacheRejectsOversizedEntry(t *testing.T) {
	c, err := newCache(100, 0, 10_000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ok := c.put("big", &entry{Markdown: make([]byte, 2000)})
	if ok {
		t.Fatal("entry over MaxEntryBytes should be rejected")
	}
	if _, hit := c.get("big"); hit {
		t.Fatal("rejected entry should not be retrievable")
	}
}

func TestStaticFileSizeCapped(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "big.html")
	// 16 KiB file, cap at 8 KiB → should refuse.
	mustWrite(t, src, "<html><body>"+strings.Repeat("x", 16<<10)+"</body></html>")
	m := newTestModule(t, root)
	m.MaxBodyBytes = 8 << 10

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/big.md", nil)
	_, err := m.serveStatic(rec, req, "/big.html")
	if err == nil {
		t.Fatal("expected size-cap error")
	}
	if err != errSourceUnavailable {
		t.Fatalf("expected redacted errSourceUnavailable, got: %v", err)
	}
}

func TestUpstreamVaryBypassesCache(t *testing.T) {
	rec := newCaptureWriter(httptest.NewRecorder(), 1<<20)
	rec.Header().Set("Vary", "Accept-Language")
	if responseIsCacheable(rec) {
		t.Fatal("response with non-trivial Vary should not be cacheable")
	}

	rec2 := newCaptureWriter(httptest.NewRecorder(), 1<<20)
	rec2.Header().Set("Vary", "Accept-Encoding")
	if !responseIsCacheable(rec2) {
		t.Fatal("Vary: Accept-Encoding alone should still be cacheable")
	}
}

func TestSnapshotSafeHeaders(t *testing.T) {
	in := http.Header{}
	in.Set("Cache-Control", "public, max-age=60")
	in.Set("Content-Security-Policy", "default-src 'self'")
	in.Set("Set-Cookie", "session=abc")          // must not pass through
	in.Set("X-Powered-By", "secret-framework")   // must not pass through
	in.Set("Server", "internal/1.0")             // must not pass through

	out := snapshotSafeHeaders(in)
	if out.Get("Cache-Control") != "public, max-age=60" {
		t.Errorf("CC missing")
	}
	if out.Get("Content-Security-Policy") == "" {
		t.Errorf("CSP missing")
	}
	if out.Get("Set-Cookie") != "" {
		t.Errorf("Set-Cookie leaked")
	}
	if out.Get("X-Powered-By") != "" {
		t.Errorf("X-Powered-By leaked")
	}
	if out.Get("Server") != "" {
		t.Errorf("Server leaked")
	}
}

func TestVaryMergedFromUpstream(t *testing.T) {
	h := http.Header{}
	stored := http.Header{}
	stored.Set("Vary", "Accept-Language, User-Agent")
	setMergedVary(h, stored)
	got := h.Get("Vary")
	if !strings.Contains(got, "Accept") {
		t.Errorf("Vary missing Accept: %q", got)
	}
	if !strings.Contains(got, "Accept-Language") {
		t.Errorf("Vary did not merge upstream Accept-Language: %q", got)
	}
	if !strings.Contains(got, "User-Agent") {
		t.Errorf("Vary did not merge upstream User-Agent: %q", got)
	}
	// No duplicate Accept even when upstream also varies on Accept.
	stored2 := http.Header{}
	stored2.Set("Vary", "Accept, Accept-Language")
	h2 := http.Header{}
	setMergedVary(h2, stored2)
	if strings.Count(h2.Get("Vary"), "Accept") < 2 {
		t.Errorf("expected Accept + Accept-Language in: %q", h2.Get("Vary"))
	}
	if strings.Count(strings.ToLower(h2.Get("Vary")), "accept,") > 1 &&
		!strings.Contains(strings.ToLower(h2.Get("Vary")), "accept-") {
		t.Errorf("duplicate Accept in Vary: %q", h2.Get("Vary"))
	}
}

func TestCacheEntryRetainsHeaders(t *testing.T) {
	c, err := newCache(64, 0, 64<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set("Cache-Control", "public, max-age=300")
	hdr.Set("Content-Language", "fr")
	e := newEntry([]byte("# hi"))
	e.Headers = hdr
	c.put("k", e)

	got, ok := c.get("k")
	if !ok {
		t.Fatal("cache miss")
	}
	if got.Headers.Get("Cache-Control") != "public, max-age=300" {
		t.Errorf("CC lost: %v", got.Headers)
	}
	if got.Headers.Get("Content-Language") != "fr" {
		t.Errorf("Lang lost: %v", got.Headers)
	}
}

func TestErrorMessagesDontLeakPaths(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "big.html"),
		"<html><body>"+strings.Repeat("x", 16<<10)+"</body></html>")
	m := newTestModule(t, root)
	m.MaxBodyBytes = 8 << 10

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/big.md", nil)
	_, err := m.serveStatic(rec, req, "/big.html")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "big.html") {
		t.Fatalf("error leaks server path: %v", err)
	}
}

func TestSafeDirSegmentNoLeak(t *testing.T) {
	a := safeDirSegment("/var/www/customer-acme-secret-project")
	if strings.Contains(a, "customer") || strings.Contains(a, "secret") || strings.Contains(a, "acme") {
		t.Fatalf("segment leaks input substring: %q", a)
	}
	if len(a) != 32 {
		t.Fatalf("expected 16-byte hex (32 chars), got %q", a)
	}
}
