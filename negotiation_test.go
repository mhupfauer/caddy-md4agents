package md4agents

import (
	"net/http/httptest"
	"testing"
)

func TestPreferMarkdown(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"text/markdown", true},
		{"text/html", false},
		{"text/html, text/markdown;q=0.9", false},
		{"text/markdown, text/html;q=0.9", true},
		{"text/markdown;q=0.8, text/html;q=0.8", true},
		{"*/*", false},
		{"", false},
		{"text/x-markdown", true},
		{"application/json", false},
	}
	for _, c := range cases {
		if got := preferMarkdown(c.accept); got != c.want {
			t.Errorf("preferMarkdown(%q) = %v, want %v", c.accept, got, c.want)
		}
	}
}

func TestNegotiate(t *testing.T) {
	m := &MarkdownForAgents{URLSuffix: ".md", QueryParam: "format"}

	t.Run("url suffix", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/docs/page.md", nil)
		want, rewrite := m.negotiate(r)
		if !want || rewrite != "/docs/page.html" {
			t.Fatalf("got (%v, %q)", want, rewrite)
		}
	})

	t.Run("query param", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/docs/page?format=md", nil)
		want, rewrite := m.negotiate(r)
		if !want || rewrite != "" {
			t.Fatalf("got (%v, %q)", want, rewrite)
		}
	})

	t.Run("accept header", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/docs/page", nil)
		r.Header.Set("Accept", "text/markdown")
		want, rewrite := m.negotiate(r)
		if !want || rewrite != "" {
			t.Fatalf("got (%v, %q)", want, rewrite)
		}
	})

	t.Run("no signal", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/docs/page", nil)
		r.Header.Set("Accept", "text/html,*/*")
		want, _ := m.negotiate(r)
		if want {
			t.Fatal("should not negotiate without explicit markdown")
		}
	})
}
