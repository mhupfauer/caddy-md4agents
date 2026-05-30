// Package md4agents implements a Caddy v2 HTTP middleware that serves a
// Markdown rendition of HTML pages when an agent (or any client) negotiates
// for it. See the README for design notes and the Cloudflare "Markdown for
// Agents" RFC this is modelled after.
package md4agents

import (
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

	// Pregenerate, when true and Root is set, walks the root at startup
	// and converts every .html file ahead of the first request. Off by
	// default — the lazy path is fast enough for most sites.
	Pregenerate bool `json:"pregenerate,omitempty"`

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
	if len(m.StripTags) == 0 {
		m.StripTags = []string{"script", "style", "noscript", "iframe", "svg"}
	}
	if m.Root != "" && m.CacheDir == "" {
		m.CacheDir = filepath.Join(m.Root, ".md4agents")
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
		// Static path didn't resolve — fall through to the dynamic capture
		// path. Useful when Root exists but the request is for a dynamic
		// handler (e.g. an SPA route handled by a downstream rewrite).
	}

	return m.serveDynamic(w, r, next, logicalPath, rewritePath)
}

// serveDynamic is the response-capture path: it lets `next` produce HTML,
// converts the buffered body, caches under the request path, and writes
// markdown back. Used for reverse_proxy, templates, or any case without a
// resolvable file root.
func (m *MarkdownForAgents) serveDynamic(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler, cacheKey, rewritePath string) error {
	if e, ok := m.cache.get(cacheKey); ok {
		return m.writeMarkdown(w, r, e)
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

	e, err := m.cache.do(cacheKey, func() (*entry, error) {
		md, cerr := m.convert(rec.body.Bytes())
		if cerr != nil {
			return nil, cerr
		}
		return newEntry([]byte(md)), nil
	})
	if err != nil {
		m.log.Warn("markdown conversion failed; serving original",
			zap.String("path", cacheKey), zap.Error(err))
		return rec.flush()
	}
	return m.writeMarkdown(w, r, e)
}

func (m *MarkdownForAgents) writeMarkdown(w http.ResponseWriter, r *http.Request, e *entry) error {
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, e.ETag) {
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

func newEntry(md []byte) *entry {
	sum := sha256.Sum256(md)
	return &entry{
		Markdown: md,
		ETag:     `"` + hex.EncodeToString(sum[:16]) + `"`,
		Created:  time.Now(),
	}
}
