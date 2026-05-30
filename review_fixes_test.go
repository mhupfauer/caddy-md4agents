package md4agents

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
	"golang.org/x/net/html"
)

// 1+2: cache.put must reclaim bytes on overwrite (golang-lru Add doesn't
// fire evict callback when the key already exists) and concurrent puts
// must not over-admit past maxBytes.
func TestCachePutOverwriteReclaimsBytes(t *testing.T) {
	c, err := newCache(64, 0, 10<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c.put("k", &entry{Markdown: make([]byte, 500)})
	c.put("k", &entry{Markdown: make([]byte, 700)})
	_, bytes := c.stats()
	if bytes != 700 {
		t.Fatalf("expected bytes=700 after overwrite, got %d", bytes)
	}
}

func TestCachePutConcurrentDoesNotOverAdmit(t *testing.T) {
	const maxBytes = 1 << 20
	c, err := newCache(64, 0, maxBytes, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for i := 0; i < 32; i++ {
		i := i
		go func() {
			c.put(fmtKey(i), &entry{Markdown: make([]byte, 200<<10)})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 32; i++ {
		<-done
	}
	_, bytes := c.stats()
	if bytes > maxBytes {
		t.Fatalf("byte budget violated under concurrency: bytes=%d > maxBytes=%d", bytes, maxBytes)
	}
}

func fmtKey(i int) string { return "k-" + strings.Repeat("x", i%5) + string(rune('A'+i)) }

// 3: promoteToRoot must refuse to act when main_selector matches body or
// any ancestor of body (would otherwise create body.Parent == body).
func TestMainSelectorBodyDoesNotCycle(t *testing.T) {
	m := &MarkdownForAgents{MainSelector: "body"}
	m.conv = m.buildConverter()
	out, err := m.convert([]byte(`<html><body><p>hello</p></body></html>`))
	if err != nil {
		t.Fatalf("convert errored on main_selector=body: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected hello in output, got %q", out)
	}
}

func TestPromoteToRootAncestorGuard(t *testing.T) {
	// Construct a tree where keep is an ancestor of body.
	doc := &html.Node{Type: html.DocumentNode}
	htmlEl := &html.Node{Type: html.ElementNode, Data: "html"}
	body := &html.Node{Type: html.ElementNode, Data: "body"}
	doc.AppendChild(htmlEl)
	htmlEl.AppendChild(body)

	// keep == htmlEl is an ancestor of body — promoteToRoot must no-op.
	promoteToRoot(doc, htmlEl)
	if body.Parent != htmlEl {
		t.Fatalf("expected body parent unchanged; got %v", body.Parent)
	}
}

// 4: janitor must NOT delete sidecars on transient stat errors.
func TestJanitorIgnoresNonExistErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root defeats the EACCES test")
	}
	root := t.TempDir()
	cacheDir := t.TempDir()
	// Sidecar with no matching source — but source dir is unreadable, so
	// Stat returns EACCES (not NotExist). Janitor must skip the delete.
	if err := os.MkdirAll(filepath.Join(cacheDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(cacheDir, "sub", "page.html.md")
	if err := os.WriteFile(sidecar, []byte("# stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Restrict source dir so os.Stat returns EACCES.
	srcDir := filepath.Join(root, "sub")
	if err := os.MkdirAll(srcDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(srcDir, 0o755) })

	m := &MarkdownForAgents{
		Root: root, CacheDir: cacheDir,
		rootResolved: root, cacheResolved: cacheDir,
	}
	m.log = zap.NewNop()
	j := newJanitor(m, time.Hour)
	j.sweep(context.Background())

	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar deleted on non-existence error: %v", err)
	}
}

// 5: HEAD on cache-miss must produce the same ETag as an equivalent GET.
func TestHEADOnMissPromotesToGETForETag(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "doc.html"),
		`<html><body><h1>hello</h1></body></html>`)

	m := newTestModule(t, root)

	rec1 := httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "/doc.md", nil)
	if _, err := m.serveStatic(rec1, get, "/doc.html"); err != nil {
		t.Fatal(err)
	}
	getETag := rec1.Header().Get("ETag")
	if getETag == "" {
		t.Fatal("no ETag on GET")
	}

	rec2 := httptest.NewRecorder()
	head := httptest.NewRequest(http.MethodHead, "/doc.md", nil)
	if _, err := m.serveStatic(rec2, head, "/doc.html"); err != nil {
		t.Fatal(err)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("HEAD must have empty body, got %q", rec2.Body.String())
	}
	if rec2.Header().Get("ETag") != getETag {
		t.Fatalf("HEAD ETag %q != GET ETag %q", rec2.Header().Get("ETag"), getETag)
	}
}

// 6: cache_ttl 0 in Caddyfile must map to the never-expire sentinel, not
// be silently overridden by Provision's default.
func TestCaddyfileCacheTTLZeroMeansNever(t *testing.T) {
	d := caddyfile.NewTestDispenser(`markdown_for_agents { cache_ttl 0 }`)
	var m MarkdownForAgents
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if int64(m.CacheTTL) >= 0 {
		t.Fatalf("expected negative sentinel for `cache_ttl 0`, got %v", time.Duration(m.CacheTTL))
	}
	// And it survives the entry.expired() ttl<=0 check:
	e := &entry{Created: time.Now().Add(-24 * time.Hour)}
	if e.expired(time.Duration(m.CacheTTL)) {
		t.Fatal("expected entry not expired with never-sentinel ttl")
	}
}

func TestCaddyfileCacheTTLNever(t *testing.T) {
	d := caddyfile.NewTestDispenser(`markdown_for_agents { cache_ttl never }`)
	var m MarkdownForAgents
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if int64(m.CacheTTL) >= 0 {
		t.Fatalf("expected negative sentinel for `cache_ttl never`, got %v", time.Duration(m.CacheTTL))
	}
}

// 7: byte budget already covered by TestCachePutConcurrentDoesNotOverAdmit.

// 8: errSourceUnavailable must preserve errors.Is(fs.ErrNotExist) chain.
func TestErrSourceUnavailablePreservesIsChain(t *testing.T) {
	wrapped := errSourceUnavailable(fs.ErrNotExist)
	if !errors.Is(wrapped, fs.ErrNotExist) {
		t.Fatal("expected errors.Is to see through the redaction")
	}
	if wrapped.Error() != "source unavailable" {
		t.Fatalf("Error() must still be redacted, got %q", wrapped.Error())
	}

	// Nil cause case (size-cap / source-changed): Is should not match
	// anything specific.
	nilCause := errSourceUnavailable(nil)
	if errors.Is(nilCause, fs.ErrNotExist) {
		t.Fatal("nil-cause must not falsely match fs.ErrNotExist")
	}
}

// 9: capture overflow path must reset the buffer (no pinning) and writes
// of subsequent chunks must pass through.
func TestCaptureOverflowPassesThroughAndDropsBuffer(t *testing.T) {
	rr := httptest.NewRecorder()
	c := newCaptureWriter(rr, 100) // 100-byte cap
	c.Header().Set("Content-Type", "text/html")
	c.WriteHeader(200)

	// First write fits in the buffer.
	if _, err := c.Write(make([]byte, 50)); err != nil {
		t.Fatal(err)
	}
	// Second write overflows; drains buffered 50 bytes + writes new 60.
	if _, err := c.Write(make([]byte, 60)); err != nil {
		t.Fatal(err)
	}
	if !c.overflow {
		t.Fatal("expected overflow flag set")
	}
	if c.body != nil {
		t.Fatal("body buffer must be released after overflow")
	}
	if rr.Body.Len() != 110 {
		t.Fatalf("expected 110 bytes on wire (50 drained + 60 new), got %d", rr.Body.Len())
	}
	// Subsequent writes go straight to the real writer.
	if _, err := c.Write(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if rr.Body.Len() != 120 {
		t.Fatalf("expected 120 bytes total, got %d", rr.Body.Len())
	}
}

// 10: pregenerator must accept the run context so cancellation propagates.
// We verify the signature change indirectly — cancelling ctx before
// process() runs makes it a no-op.
func TestPregeneratorContextCancellation(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "p.html"), `<html><body><p>x</p></body></html>`)
	m := newTestModule(t, root)
	pre := newPregenerator(m)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before processing
	pre.process(ctx, filepath.Join(root, "p.html"))

	// Sidecar must NOT exist because process returned early.
	sidecar := filepath.Join(m.CacheDir, "p.html.md")
	if _, err := os.Stat(sidecar); err == nil {
		t.Fatal("expected no sidecar after cancellation, but it was written")
	}
}

