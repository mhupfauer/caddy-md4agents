package md4agents

import (
	"net/http"
	"strconv"
	"strings"
)

// negotiateMarkdown decides whether the caller wants Markdown for this request.
// It returns (wantMarkdown, rewrittenPath). The rewrittenPath is non-empty when
// the request used a URL-based hint (suffix or query) that should be stripped
// before the inner handler sees it, so the upstream still resolves the HTML
// resource.
func (m *MarkdownForAgents) negotiate(r *http.Request) (bool, string) {
	// 1. URL suffix (e.g. /docs/page.md → /docs/page.html)
	if m.URLSuffix != "" && strings.HasSuffix(r.URL.Path, m.URLSuffix) {
		base := strings.TrimSuffix(r.URL.Path, m.URLSuffix)
		// Prefer .html sibling; if absent the upstream will 404 — caller decides.
		return true, base + ".html"
	}

	// 2. Query parameter (e.g. ?format=md)
	if m.QueryParam != "" {
		if v := r.URL.Query().Get(m.QueryParam); v == "md" || v == "markdown" {
			return true, ""
		}
	}

	// 3. Accept header content negotiation
	if accept := r.Header.Get("Accept"); accept != "" {
		return preferMarkdown(accept), ""
	}
	return false, ""
}

// preferMarkdown returns true if the Accept header expresses a preference
// for text/markdown over text/html. Wildcards (text/*, */*) don't count
// as a preference for markdown — the client must explicitly ask for it.
//
// Per RFC 7231 §5.3.1, q=0 means "not acceptable" — we honor that and
// refuse to serve markdown when the client explicitly disclaimed it,
// even if no other media type is listed.
func preferMarkdown(accept string) bool {
	var (
		mdQ   float64 = -1
		htmlQ float64 = -1
	)
	for _, part := range strings.Split(accept, ",") {
		mime, q := parseAcceptPart(part)
		switch mime {
		case "text/markdown", "text/x-markdown":
			if q > mdQ {
				mdQ = q
			}
		case "text/html", "application/xhtml+xml":
			if q > htmlQ {
				htmlQ = q
			}
		}
	}
	if mdQ <= 0 {
		return false
	}
	if htmlQ < 0 {
		return true
	}
	return mdQ >= htmlQ
}

func parseAcceptPart(part string) (mime string, q float64) {
	q = 1.0
	segs := strings.Split(strings.TrimSpace(part), ";")
	mime = strings.TrimSpace(segs[0])
	for _, p := range segs[1:] {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "q=") {
			continue
		}
		if f, err := strconv.ParseFloat(p[2:], 64); err == nil {
			q = f
		}
	}
	return mime, q
}
