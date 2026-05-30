package md4agents

import (
	"net/http/httptest"
	"testing"
)

func TestRequestIsCacheable(t *testing.T) {
	m := &MarkdownForAgents{}

	cases := []struct {
		name     string
		method   string
		headers  map[string]string
		allowAuth bool
		want     bool
	}{
		{"GET no auth", "GET", nil, false, true},
		{"HEAD no auth", "HEAD", nil, false, true},
		{"POST", "POST", nil, false, false},
		{"PUT", "PUT", nil, false, false},
		{"GET with cookie", "GET", map[string]string{"Cookie": "session=abc"}, false, false},
		{"GET with authz", "GET", map[string]string{"Authorization": "Bearer x"}, false, false},
		{"GET with cookie, allowAuth", "GET", map[string]string{"Cookie": "session=abc"}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m.AllowAuthenticated = c.allowAuth
			r := httptest.NewRequest(c.method, "/page", nil)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if got := m.requestIsCacheable(r); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestResponseIsCacheable(t *testing.T) {
	mk := func(h map[string]string) *captureWriter {
		rec := newCaptureWriter(httptest.NewRecorder(), 1<<20)
		for k, v := range h {
			rec.Header().Set(k, v)
		}
		return rec
	}
	cases := []struct {
		name string
		hdrs map[string]string
		want bool
	}{
		{"plain", nil, true},
		{"set-cookie", map[string]string{"Set-Cookie": "a=1"}, false},
		{"cache-control private", map[string]string{"Cache-Control": "private, max-age=0"}, false},
		{"cache-control no-store", map[string]string{"Cache-Control": "no-store"}, false},
		{"cache-control public", map[string]string{"Cache-Control": "public, max-age=600"}, true},
		{"cache-control no-cache", map[string]string{"Cache-Control": "no-cache"}, false},
		{"cache-control must-revalidate", map[string]string{"Cache-Control": "max-age=0, must-revalidate"}, false},
		{"cache-control substring trap", map[string]string{"Cache-Control": "no-cache-mode"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := responseIsCacheable(mk(c.hdrs)); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDynamicCacheKeyIncludesQueryAndHost(t *testing.T) {
	a := dynamicCacheKey("alpha.example", "/api/p", "id=1")
	b := dynamicCacheKey("alpha.example", "/api/p", "id=2")
	c := dynamicCacheKey("beta.example", "/api/p", "id=1")
	if a == b {
		t.Fatalf("keys must differ on query: %q == %q", a, b)
	}
	if a == c {
		t.Fatalf("keys must differ on host: %q == %q", a, c)
	}
	if got := dynamicCacheKey("alpha.example", "/api/p", ""); got != "alpha.example|/api/p" {
		t.Fatalf("no query: got %q", got)
	}
}

func TestIfNoneMatchHits(t *testing.T) {
	const tag = `"abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{tag, true},
		{`"other"`, false},
		{`"other", "abc123"`, true},
		{`W/"abc123"`, true},
		{`*`, true},
		{`"prefix-abc123-suffix"`, false}, // substring-match regression
	}
	for _, c := range cases {
		t.Run(c.header, func(t *testing.T) {
			if got := ifNoneMatchHits(c.header, tag); got != c.want {
				t.Errorf("hits(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

func TestSafeDirSegment(t *testing.T) {
	a := safeDirSegment("/var/www/site")
	b := safeDirSegment("/var/www/other")
	if a == b {
		t.Fatal("different roots must hash to different segments")
	}
	for _, r := range a {
		if r == '/' || r == 0 {
			t.Fatalf("segment contains path-unsafe rune: %q", a)
		}
	}
}
