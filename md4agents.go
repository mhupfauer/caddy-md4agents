// Package md4agents implements a Caddy v2 HTTP middleware that serves a
// Markdown rendition of HTML pages when an agent (or any client) negotiates
// for it. See the README for design notes and the Cloudflare "Markdown for
// Agents" RFC this is modelled after.
package md4agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(MarkdownForAgents{})
}

// MarkdownForAgents is the Caddy module. All durations and sizes are zero-value
// safe: an unset field falls back to a documented default in Provision.
type MarkdownForAgents struct {
	// Root is the static file root to resolve requests against. When set,
	// the static-first path is enabled: author `.md` files are served
	// verbatim, generated artifacts are written to CacheDir and reused.
	// When unset, the module only acts as a streaming converter in front
	// of dynamic handlers (reverse_proxy, templates, etc.).
	Root string `json:"root,omitempty"`

	// CacheDir holds the on-disk write-through cache for generated
	// markdown. Defaults to "<Root>/.md4agents". Use a path outside Root
	// if you don't want generated files visible to file_server.
	CacheDir string `json:"cache_dir,omitempty"`

	// URLSuffix appended to a path to explicitly request markdown
	// (e.g. ".md"). Empty disables URL-suffix negotiation.
	URLSuffix string `json:"url_suffix,omitempty"`

	// QueryParam to check for markdown opt-in (e.g. "format"). The value
	// must be "md" or "markdown". Empty disables query-param negotiation.
	QueryParam string `json:"query_param,omitempty"`

	// StripTags is a list of HTML tag names to remove entirely from the
	// conversion (e.g. "nav", "footer", "script", "style"). These are
	// applied at converter build time and are essentially free.
	StripTags []string `json:"strip_tags,omitempty"`

	// StripSelectors is a list of simple selectors (#id, .class, tag) to
	// remove from the DOM before conversion. Heavier than StripTags because
	// it walks the parsed tree.
	StripSelectors []string `json:"strip_selectors,omitempty"`

	// MainSelector, if set, restricts conversion to the first matching
	// element. Useful for stripping site chrome around an <article>.
	MainSelector string `json:"main_selector,omitempty"`

	// CacheSize bounds the number of cached markdown responses held in
	// memory. 0 → 4096.
	CacheSize int `json:"cache_size,omitempty"`

	// CacheTTL bounds how long an in-memory cache entry is reused. The
	// on-disk cache uses mtime-based invalidation and ignores TTL. 0 → no
	// expiry.
	CacheTTL caddy.Duration `json:"cache_ttl,omitempty"`

	// MaxBodyBytes limits the size of an upstream HTML response that we'll
	// attempt to convert in the dynamic (non-Root) path. 0 → 4 MiB.
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`

	// ConvertTimeout bounds an individual HTML→MD conversion. 0 → 5s.
	// Exceeding it returns 503; the conversion continues in the
	// background and populates the cache for the next request.
	ConvertTimeout caddy.Duration `json:"convert_timeout,omitempty"`

	// Pregenerate, when true and Root is set, walks the root at startup
	// and converts every .html file ahead of the first request. Off by
	// default — the lazy path is fast enough for most sites.
	Pregenerate bool `json:"pregenerate,omitempty"`

	// AllowAuthenticated, when true, allows caching responses for requests
	// that carry Authorization or Cookie headers. Default false: any such
	// request bypasses the shared cache to avoid serving one user's
	// markdown to another. Only enable if you know upstream content is
	// not user-specific.
	AllowAuthenticated bool `json:"allow_authenticated,omitempty"`

	// internal state
	conv  *converter.Converter
	cache *markdownCache
	pre   *pregenerator
	log   *zap.Logger
}

func (MarkdownForAgents) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.markdown_for_agents",
		New: func() caddy.Module { return new(MarkdownForAgents) },
	}
}

func (m *MarkdownForAgents) Provision(ctx caddy.Context) error {
	m.log = ctx.Logger()

	if m.URLSuffix == "" {
		m.URLSuffix = ".md"
	}
	if m.QueryParam == "" {
		m.QueryParam = "format"
	}
	if m.MaxBodyBytes == 0 {
		m.MaxBodyBytes = 4 << 20
	}
	if m.ConvertTimeout == 0 {
		m.ConvertTimeout = caddy.Duration(5 * time.Second)
	}
	if len(m.StripTags) == 0 {
		m.StripTags = []string{"script", "style", "noscript", "iframe", "svg"}
	}
	if m.Root != "" && m.CacheDir == "" {
		// Default to Caddy's per-plugin data dir, *outside* Root, so the
		// sidecar files are never served accidentally by file_server.
		// Multiple Root configurations on the same Caddy instance get
		// separate subdirs derived from the absolute root path.
		m.CacheDir = filepath.Join(caddy.AppDataDir(), "md4agents", safeDirSegment(m.Root))
	}

	m.conv = m.buildConverter()

	cache, err := newCache(m.CacheSize, time.Duration(m.CacheTTL))
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	m.cache = cache

	if m.Pregenerate && m.Root != "" {
		m.pre = newPregenerator(m)
		if err := m.pre.start(ctx); err != nil {
			return fmt.Errorf("pregenerator: %w", err)
		}
	}
	return nil
}

func (m *MarkdownForAgents) Cleanup() error {
	if m.pre != nil {
		return m.pre.stop()
	}
	return nil
}

func (m *MarkdownForAgents) Validate() error { return nil }

var (
	_ caddy.Provisioner           = (*MarkdownForAgents)(nil)
	_ caddy.CleanerUpper          = (*MarkdownForAgents)(nil)
	_ caddy.Validator             = (*MarkdownForAgents)(nil)
	_ caddyhttp.MiddlewareHandler = (*MarkdownForAgents)(nil)
	_ caddyfile.Unmarshaler       = (*MarkdownForAgents)(nil)
)

// ServeHTTP dispatches to the static-first path when Root is configured and
// the request resolves to an HTML file on disk, falling back to the dynamic
// capture path otherwise. The static path is hot for typical sites; the
// dynamic path covers reverse_proxy and template handlers.
func (m *MarkdownForAgents) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	wantMD, rewritePath := m.negotiate(r)
	if !wantMD {
		return next.ServeHTTP(w, r)
	}

	logicalPath := r.URL.Path
	if rewritePath != "" {
		logicalPath = rewritePath
	}

	if m.Root != "" {
		served, err := m.serveStatic(w, r, logicalPath)
		if err != nil {
			return err
		}
		if served {
			return nil
		}
	}

	return m.serveDynamic(w, r, next, logicalPath, rewritePath)
}

// serveDynamic is the response-capture path: it lets `next` produce HTML,
// converts the buffered body, caches under the request path+query, and
// writes markdown back. Used for reverse_proxy, templates, or any case
// without a resolvable file root.
//
// Cache-safety rules:
//   - Only GET/HEAD are cacheable; everything else converts but never reads
//     or writes the shared cache.
//   - Requests with Authorization or Cookie headers bypass the shared cache
//     by default (override with allow_authenticated). Without this, one
//     user's authenticated content could be served to another user issuing
//     a public request for the same URL.
//   - Upstream responses with Set-Cookie or Cache-Control: private|no-store
//     are converted and served once but never cached.
func (m *MarkdownForAgents) serveDynamic(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler, logicalPath, rewritePath string) error {
	cacheable := m.requestIsCacheable(r)
	cacheKey := dynamicCacheKey(logicalPath, r.URL.RawQuery)

	if cacheable {
		if e, ok := m.cache.get(cacheKey); ok {
			return m.writeMarkdown(w, r, e)
		}
	}

	if rewritePath != "" {
		r2 := r.Clone(r.Context())
		r2.URL.Path = rewritePath
		r2.RequestURI = ""
		r = r2
	}

	rec := newCaptureWriter(w, m.MaxBodyBytes)
	if err := next.ServeHTTP(rec, r); err != nil {
		return err
	}
	if !rec.isConvertibleHTML() {
		return rec.flush()
	}

	convert := func() (*entry, error) {
		md, cerr := m.convertWithTimeout(r.Context(), rec.body.Bytes())
		if cerr != nil {
			return nil, cerr
		}
		return newEntry([]byte(md)), nil
	}

	var (
		e   *entry
		err error
	)
	if cacheable && responseIsCacheable(rec) {
		e, err = m.cache.do(cacheKey, convert)
	} else {
		e, err = convert()
	}
	if err != nil {
		m.log.Warn("markdown conversion failed; serving original",
			zap.String("path", cacheKey), zap.Error(err))
		return rec.flush()
	}
	return m.writeMarkdown(w, r, e)
}

// requestIsCacheable decides whether the in-memory cache may be read from
// or written to for this request. Conservative by default — see the
// AllowAuthenticated option.
func (m *MarkdownForAgents) requestIsCacheable(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !m.AllowAuthenticated {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			return false
		}
	}
	return true
}

// responseIsCacheable inspects the captured upstream response. We never
// cache anything carrying user-specific cookies or an explicit private/
// no-store directive.
func responseIsCacheable(rec *captureWriter) bool {
	h := rec.Header()
	if h.Get("Set-Cookie") != "" {
		return false
	}
	cc := strings.ToLower(h.Get("Cache-Control"))
	if strings.Contains(cc, "private") || strings.Contains(cc, "no-store") {
		return false
	}
	return true
}

// dynamicCacheKey includes the raw query so /api?id=1 and /api?id=2 don't
// collide. Path comes first so logs are still scannable.
func dynamicCacheKey(path, query string) string {
	if query == "" {
		return path
	}
	return path + "?" + query
}

// convertWithTimeout runs conversion in a goroutine so we can surface a 503
// to the client if the converter wedges. The goroutine is not killed (Go
// has no safe way to cancel CPU-bound work) — it keeps running and its
// result populates the cache for the next request.
func (m *MarkdownForAgents) convertWithTimeout(ctx context.Context, htmlBytes []byte) (string, error) {
	timeout := time.Duration(m.ConvertTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		md  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		md, err := m.convert(htmlBytes)
		done <- result{md, err}
	}()

	select {
	case r := <-done:
		return r.md, r.err
	case <-ctx.Done():
		return "", fmt.Errorf("convert: %w", ctx.Err())
	}
}

func (m *MarkdownForAgents) writeMarkdown(w http.ResponseWriter, r *http.Request, e *entry) error {
	if ifNoneMatchHits(r.Header.Get("If-None-Match"), e.ETag) {
		w.Header().Set("ETag", e.ETag)
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	h := w.Header()
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("ETag", e.ETag)
	h.Set("Vary", "Accept")
	h.Set("Content-Length", strconv.Itoa(len(e.Markdown)))
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(e.Markdown)
	return err
}

// ifNoneMatchHits parses an If-None-Match value (RFC 7232 §3.2) and returns
// true if any tag matches our strong ETag. A `W/` prefix is accepted for
// weak comparison (which is fine for revalidation). `*` matches any.
func ifNoneMatchHits(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, raw := range strings.Split(header, ",") {
		raw = strings.TrimSpace(raw)
		raw = strings.TrimPrefix(raw, "W/")
		if raw == etag {
			return true
		}
	}
	return false
}

func newEntry(md []byte) *entry {
	sum := sha256.Sum256(md)
	return &entry{
		Markdown: md,
		ETag:     `"` + hex.EncodeToString(sum[:16]) + `"`,
		Created:  time.Now(),
	}
}

// safeDirSegment turns an absolute filesystem path into a single directory
// segment safe to use as a sub-path (no slashes, no nul). It uses a short
// hex prefix of the SHA-256 plus a sanitized tail for human-friendly logs.
func safeDirSegment(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	sum := sha256.Sum256([]byte(abs))
	hash := hex.EncodeToString(sum[:6])
	tail := filepath.Base(abs)
	clean := make([]rune, 0, len(tail))
	for _, r := range tail {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			clean = append(clean, r)
		default:
			clean = append(clean, '_')
		}
	}
	if len(clean) == 0 {
		return hash
	}
	return string(clean) + "-" + hash
}
