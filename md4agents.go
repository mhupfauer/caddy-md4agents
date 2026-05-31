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
	"os"
	"path/filepath"
	"runtime"
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

// MarkdownForAgents serves a Markdown rendition of HTML pages when a
// client — typically an AI agent — negotiates for it. It implements
// Cloudflare's "Markdown for Agents" convention on top of Caddy's
// static and dynamic handlers, with caching, content negotiation,
// and HTML sanitization built in.
//
// ## Why this matters
//
// Modern AI agents (Claude, ChatGPT, Perplexity, crawler bots) waste
// tokens parsing HTML chrome — navigation, scripts, cookie banners,
// analytics — before reaching the content. Serving the same URL as
// Markdown gives them roughly 5–10× more useful content per token and
// measurably improves answer quality on long documents. Same URL,
// same auth, just `Accept: text/markdown` (or a `.md` suffix).
//
// ## Content negotiation
//
// A request is served Markdown when any of these is true:
//
// Trigger      | Example
// -------------|--------
// URL suffix   | `GET /docs/page.md`
// Query param  | `GET /docs/page?format=md`
// Accept hdr   | `Accept: text/markdown` (q-value aware vs `text/html`)
//
// The first two are stripped before the inner handler sees the
// request, so the upstream still resolves the underlying HTML.
//
// ## Quick start (static site)
//
// ```caddy
//
//	example.com {
//	    root * /var/www/site
//	    markdown_for_agents {
//	        root /var/www/site
//	    }
//	    file_server
//	}
//
// ```
//
// Caddyfile note: always use the block form to set `root`. A
// bare `markdown_for_agents /var/www/site` would be parsed by
// Caddy as a path matcher (`/var/www/site`), not as a positional
// argument to the directive.
//
// Author-written `*.md` files win over generated ones; generated
// artifacts are written to a sidecar cache (`/var/cache/md4agents`
// by default) and reused on every subsequent request. Edits to
// source HTML invalidate cache entries automatically (mtime + size
// stat) — no watcher required.
//
// ## Reverse-proxy mode
//
// Omit `root` and the module becomes a streaming converter in front
// of any dynamic upstream:
//
// ```caddy
//
//	example.com {
//	    markdown_for_agents {
//	        main_selector   article
//	        strip_selectors nav footer .ads
//	    }
//	    reverse_proxy backend:8080
//	}
//
// ```
//
// ## Cache safety
//
// Only `GET` and `HEAD` are cacheable. Requests carrying
// `Authorization` or `Cookie` headers bypass the shared cache by
// default; upstream responses with `Set-Cookie`,
// `Cache-Control: private/no-store`, or a non-trivial `Vary` are
// converted and served once but never cached.
//
// ## More
//
// Full documentation, performance notes, and security guidance live
// at https://github.com/mhupfauer/caddy-md4agents.
//
// All durations and sizes are zero-value safe: any unset field
// falls back to a documented default during provisioning.
type MarkdownForAgents struct {
	// Root is the static file root to resolve requests against. When set,
	// the static-first path is enabled: author `.md` files are served
	// verbatim, generated artifacts are written to CacheDir and reused.
	// When unset, the module only acts as a streaming converter in front
	// of dynamic handlers (reverse_proxy, templates, etc.).
	Root string `json:"root,omitempty"`

	// CacheDir holds the on-disk write-through cache for generated
	// markdown. Defaults to caddy.AppDataDir()/md4agents/<hash> so it
	// can never be served by file_server.
	CacheDir string `json:"cache_dir,omitempty"`

	// URLSuffix appended to a path to explicitly request markdown
	// (e.g. ".md"). Empty disables URL-suffix negotiation.
	URLSuffix string `json:"url_suffix,omitempty"`

	// QueryParam to check for markdown opt-in (e.g. "format"). The value
	// must be "md" or "markdown". Empty disables query-param negotiation.
	QueryParam string `json:"query_param,omitempty"`

	// StripTags lists HTML tag names removed entirely (with their
	// subtree) before conversion. Default:
	// `script style noscript iframe svg`. Useful for stripping
	// inline analytics, embedded videos, decorative SVGs, etc.
	StripTags []string `json:"strip_tags,omitempty"`

	// StripSelectors lists simple selectors removed before
	// conversion. Supported forms: bare tag (`nav`), class
	// (`.ads`), or id (`#cookie-banner`). Quote id selectors in
	// the Caddyfile — `#` is a comment marker there.
	StripSelectors []string `json:"strip_selectors,omitempty"`

	// MainSelector, if set, restricts conversion to the subtree
	// rooted at the first element matching this simple selector
	// (e.g. `article`, `main`, `.post-body`). Everything outside
	// is discarded, which is the cleanest way to strip site
	// chrome on theme-heavy pages.
	MainSelector string `json:"main_selector,omitempty"`

	// CacheSize bounds the number of cached markdown responses held in
	// memory. 0 → 4096.
	CacheSize int `json:"cache_size,omitempty"`

	// CacheBytes is the total in-memory cache byte budget. 0 → 256 MiB.
	CacheBytes int64 `json:"cache_bytes,omitempty"`

	// CacheEntryBytes caps the size of a single cached entry, both to
	// reject pathological pages and to make the byte budget meaningful.
	// 0 → 1 MiB.
	CacheEntryBytes int64 `json:"cache_entry_bytes,omitempty"`

	// CacheTTL bounds how long an in-memory cache entry is reused.
	//   0  → use default (15m)
	//   <0 → never expire (use with care; the on-disk cache already
	//         mtime-invalidates, so this only matters for the dynamic
	//         path)
	CacheTTL caddy.Duration `json:"cache_ttl,omitempty"`

	// MaxBodyBytes limits the size of an HTML body we'll attempt to
	// convert (both static disk reads and dynamic captures). 0 → 4 MiB.
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`

	// ConvertTimeout bounds an individual HTML→MD conversion. 0 → 5s.
	ConvertTimeout caddy.Duration `json:"convert_timeout,omitempty"`

	// MaxConcurrent caps the number of conversions that can run at once.
	// 0 → max(4, NumCPU). New requests wait on a buffered semaphore;
	// hitting the ConvertTimeout while waiting returns 503.
	MaxConcurrent int `json:"max_concurrent,omitempty"`

	// Pregenerate, when true and Root is set, walks the root at startup
	// and converts every .html file ahead of the first request.
	Pregenerate bool `json:"pregenerate,omitempty"`

	// AllowAuthenticated, when true, allows caching responses for
	// requests carrying Authorization or Cookie headers. Default false —
	// any such request bypasses the shared cache to avoid serving one
	// user's markdown to another.
	AllowAuthenticated bool `json:"allow_authenticated,omitempty"`

	// JanitorInterval, when >0 and Root is set, runs a periodic cleanup
	// of orphaned sidecar files whose source HTML no longer exists.
	// 0 → off (the lazy mtime check is enough for correctness).
	JanitorInterval caddy.Duration `json:"janitor_interval,omitempty"`

	// internal state
	conv          *converter.Converter
	cache         *markdownCache
	pre           *pregenerator
	janitor       *janitor
	log           *zap.Logger
	sem           chan struct{}
	rootResolved  string
	cacheResolved string
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
	if m.MaxConcurrent <= 0 {
		m.MaxConcurrent = max(4, runtime.NumCPU())
	}
	if m.CacheTTL == 0 {
		// Bound how long stale dynamic content can live in memory. The
		// static path remains exact (mtime invalidates) so this only
		// affects reverse_proxy / template handlers — without it, a
		// page that got removed or restricted upstream could keep
		// serving from cache indefinitely.
		m.CacheTTL = caddy.Duration(15 * time.Minute)
	}
	if len(m.StripTags) == 0 {
		m.StripTags = []string{"script", "style", "noscript", "iframe", "svg"}
	}
	if m.Root != "" && m.CacheDir == "" {
		m.CacheDir = filepath.Join(caddy.AppDataDir(), "md4agents", safeDirSegment(m.Root))
	}

	m.conv = m.buildConverter()
	m.sem = make(chan struct{}, m.MaxConcurrent)

	cache, err := newCache(m.CacheSize, time.Duration(m.CacheTTL), m.CacheBytes, m.CacheEntryBytes)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	m.cache = cache

	if m.Root != "" {
		// Resolve symlinks on Root *once* and use the canonicalized path
		// for every subsequent under-root check. If Root itself doesn't
		// exist yet (deploy-time race), proceed with the textual path —
		// the per-request resolver will fail safely.
		if resolved, err := filepath.EvalSymlinks(m.Root); err == nil {
			m.rootResolved = resolved
		} else {
			abs, _ := filepath.Abs(m.Root)
			m.rootResolved = abs
		}
	}
	if m.CacheDir != "" {
		if err := ensureDir(m.CacheDir); err != nil {
			return fmt.Errorf("cache_dir: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(m.CacheDir); err == nil {
			m.cacheResolved = resolved
		} else {
			abs, _ := filepath.Abs(m.CacheDir)
			m.cacheResolved = abs
		}
		// Loudly warn if the operator has pointed cache_dir at a
		// subdirectory of Root — file_server would happily serve the
		// generated sidecars as if they were site assets.
		if m.rootResolved != "" && pathInside(m.rootResolved, m.cacheResolved) {
			m.log.Warn("cache_dir is inside root; file_server may expose generated sidecars",
				zap.String("cache_dir", m.cacheResolved),
				zap.String("root", m.rootResolved))
		}
	}

	if m.Pregenerate && m.Root != "" {
		m.pre = newPregenerator(m)
		if err := m.pre.start(ctx); err != nil {
			return fmt.Errorf("pregenerator: %w", err)
		}
	}
	if time.Duration(m.JanitorInterval) > 0 && m.Root != "" && m.CacheDir != "" {
		m.janitor = newJanitor(m, time.Duration(m.JanitorInterval))
		m.janitor.start(ctx)
	}
	return nil
}

func (m *MarkdownForAgents) Cleanup() error {
	if m.pre != nil {
		_ = m.pre.stop()
	}
	if m.janitor != nil {
		_ = m.janitor.stop()
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
// capture path otherwise.
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

// serveDynamic is the response-capture path. Cache-safety rules are
// documented on the helper functions below.
func (m *MarkdownForAgents) serveDynamic(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler, logicalPath, rewritePath string) error {
	canRead, canWrite := m.cacheCapability(r)
	cacheKey := dynamicCacheKey(r.Host, logicalPath, r.URL.RawQuery)

	if canRead {
		if e, ok := m.cache.get(cacheKey); ok {
			return m.writeMarkdown(w, r, e, nil)
		}
	}

	// HEAD-on-miss: promote to GET against upstream so we receive a real
	// HTML body to convert. Without this, RFC 9110 §15.4 lets upstream
	// return an empty body for HEAD; we'd compute an ETag from empty
	// markdown that the subsequent GET would not match — silently
	// breaking client-driven revalidation. The wire response is still
	// HEAD-correct because writeMarkdown suppresses the body.
	upstreamReq := r
	if rewritePath != "" || r.Method == http.MethodHead {
		r2 := r.Clone(r.Context())
		if rewritePath != "" {
			r2.URL.Path = rewritePath
		}
		if r.Method == http.MethodHead {
			r2.Method = http.MethodGet
		}
		r2.RequestURI = ""
		upstreamReq = r2
	}

	rec := newCaptureWriter(w, m.MaxBodyBytes)
	if err := next.ServeHTTP(rec, upstreamReq); err != nil {
		return err
	}
	if !rec.isConvertibleHTML() {
		return rec.flush()
	}

	// Snapshot the upstream headers (allowlist) and the FULL Vary header
	// set before we throw the captured response away. Using Values
	// (not Get) is critical: upstream may have called Header().Add
	// twice for separate Vary tokens and Get returns only the first.
	passthrough := snapshotSafeHeaders(rec.Header())
	for _, v := range rec.Header().Values("Vary") {
		passthrough.Add("Vary", v)
	}

	convert := func() (*entry, error) {
		md, cerr := m.convertWithTimeout(r.Context(), rec.body.Bytes())
		if cerr != nil {
			return nil, cerr
		}
		e := newEntry([]byte(md))
		e.Headers = passthrough
		return e, nil
	}

	var (
		e   *entry
		err error
	)
	if canWrite && responseIsCacheable(rec) {
		e, err = m.cache.do(cacheKey, convert)
	} else {
		e, err = convert()
	}
	if err != nil {
		m.log.Warn("markdown conversion failed; serving original",
			zap.String("path", cacheKey), zap.Error(err))
		return rec.flush()
	}
	return m.writeMarkdown(w, r, e, nil)
}

// cacheCapability returns (canRead, canWrite). HEAD may *read* a cache
// entry primed by an earlier GET (the headers are correct, the body is
// suppressed in writeMarkdown), but HEAD must NOT *write* — upstreams
// often return an empty body for HEAD per RFC 9110 §15.4 and we'd cache
// empty markdown under the GET key.
func (m *MarkdownForAgents) cacheCapability(r *http.Request) (canRead, canWrite bool) {
	switch r.Method {
	case http.MethodGet:
		canRead, canWrite = true, true
	case http.MethodHead:
		canRead, canWrite = true, false
	default:
		return false, false
	}
	if !m.AllowAuthenticated {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			return false, false
		}
	}
	return
}

// requestIsCacheable is retained for tests; returns canRead.
func (m *MarkdownForAgents) requestIsCacheable(r *http.Request) bool {
	cr, _ := m.cacheCapability(r)
	return cr
}

// responseIsCacheable inspects the captured upstream response. We never
// cache anything carrying Set-Cookie, an explicit private/no-store
// directive, or a Vary other than the trivial "Accept-Encoding". The
// last guard prevents serving a French page to an English-asking client
// when upstream varied by Accept-Language without us knowing.
func responseIsCacheable(rec *captureWriter) bool {
	h := rec.Header()
	if h.Get("Set-Cookie") != "" {
		return false
	}
	cc := strings.ToLower(h.Get("Cache-Control"))
	if hasCacheDirective(cc, "private") || hasCacheDirective(cc, "no-store") ||
		hasCacheDirective(cc, "no-cache") || hasCacheDirective(cc, "must-revalidate") {
		return false
	}
	// Iterate all Vary values, not just the first — multiple Add() calls
	// produce multiple entries and Get would silently drop them.
	for _, v := range h.Values("Vary") {
		for _, part := range strings.Split(v, ",") {
			k := strings.TrimSpace(strings.ToLower(part))
			if k == "" || k == "accept-encoding" {
				continue
			}
			return false
		}
	}
	return true
}

// dynamicCacheKey includes Host to avoid cross-tenant leakage when the
// same module instance handles multiple vhosts.
func dynamicCacheKey(host, path, query string) string {
	if query == "" {
		return host + "|" + path
	}
	return host + "|" + path + "?" + query
}

// hasCacheDirective does a token-aware lookup in a comma-separated
// Cache-Control value. Avoids the substring trap where "no-cache-mode"
// would match "no-cache".
func hasCacheDirective(cc, want string) bool {
	for _, tok := range strings.Split(cc, ",") {
		tok = strings.TrimSpace(tok)
		// strip "=value" if present (e.g. max-age=60).
		if i := strings.IndexByte(tok, '='); i >= 0 {
			tok = tok[:i]
		}
		if tok == want {
			return true
		}
	}
	return false
}

// convertWithTimeout caps both the conversion runtime and the number of
// concurrent conversions. The semaphore prevents a flood of distinct URLs
// from spawning unlimited goroutines; the timeout prevents one wedged
// conversion from blocking everyone else (its goroutine continues so the
// next request gets a cache hit).
func (m *MarkdownForAgents) convertWithTimeout(ctx context.Context, htmlBytes []byte) (string, error) {
	timeout := time.Duration(m.ConvertTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return "", fmt.Errorf("convert: wait for slot: %w", ctx.Err())
	}

	type result struct {
		md  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-m.sem }()
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

// safeHeaderAllowlist lists upstream response headers we'll re-emit when
// we replace an HTML body with markdown. Everything else is dropped so
// we don't accidentally smuggle Set-Cookie, X-Powered-By, or similar
// upstream-specific noise into the rewritten response.
var safeHeaderAllowlist = []string{
	"Cache-Control",
	"Expires",
	"Last-Modified",
	"Content-Language",
	"Content-Security-Policy",
	"Strict-Transport-Security",
	"X-Frame-Options",
	"X-Content-Type-Options",
	"Referrer-Policy",
	"Permissions-Policy",
}

func snapshotSafeHeaders(h http.Header) http.Header {
	out := make(http.Header, len(safeHeaderAllowlist))
	for _, k := range safeHeaderAllowlist {
		if v := h.Values(k); len(v) > 0 {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}

// writeMarkdown emits the markdown body (or a 304) and the cache-relevant
// headers stored on the entry. Pre-stored headers always win over the
// `extra` param (which dynamic-path may pass for the initial request),
// but `extra` lets the static path pass nil since it has no upstream.
func (m *MarkdownForAgents) writeMarkdown(w http.ResponseWriter, r *http.Request, e *entry, extra http.Header) error {
	stored := e.Headers
	if stored == nil {
		stored = extra
	}

	if ifNoneMatchHits(r.Header.Get("If-None-Match"), e.ETag) {
		h := w.Header()
		h.Set("ETag", e.ETag)
		setMergedVary(h, stored)
		for _, k := range []string{"Cache-Control", "Expires", "Content-Language"} {
			if v := stored.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	h := w.Header()
	for k, vs := range stored {
		if strings.EqualFold(k, "Vary") {
			continue // handled below
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("ETag", e.ETag)
	setMergedVary(h, stored)
	h.Set("Content-Length", strconv.Itoa(len(e.Markdown)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	_, err := w.Write(e.Markdown)
	return err
}

// setMergedVary writes a Vary header that always includes "Accept" plus
// any tokens upstream contributed. Without this, a downstream cache
// might store a French markdown response and replay it to a client
// asking for English, because upstream's Accept-Language vary signal
// got dropped on the rewritten response.
func setMergedVary(h http.Header, stored http.Header) {
	seen := map[string]struct{}{"accept": {}}
	out := []string{"Accept"}
	if stored != nil {
		for _, v := range stored.Values("Vary") {
			for _, tok := range strings.Split(v, ",") {
				tok = strings.TrimSpace(tok)
				if tok == "" {
					continue
				}
				key := strings.ToLower(tok)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, tok)
			}
		}
	}
	h.Set("Vary", strings.Join(out, ", "))
}

// ifNoneMatchHits parses an If-None-Match value (RFC 7232 §3.2).
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

// safeDirSegment turns an absolute path into a deterministic, opaque
// directory segment. Hash-only — the previous basename suffix could leak
// portions of admin-controlled paths into shared data dirs.
func safeDirSegment(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:16])
}

func ensureDir(p string) error {
	return os.MkdirAll(p, 0o755)
}
