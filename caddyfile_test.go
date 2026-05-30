package md4agents

import (
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestUnmarshalCaddyfile(t *testing.T) {
	input := `markdown_for_agents /srv/site {
		root            /srv/site
		cache_dir       /var/cache/md
		url_suffix      .md
		query_param     format
		cache_size      8192
		cache_ttl       2h
		max_body_bytes  10485760
		pregenerate
		main_selector   article
		strip_tags      nav footer aside
		strip_selectors .ad "#cookie"
	}`

	d := caddyfile.NewTestDispenser(input)
	var m MarkdownForAgents
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}

	if m.Root != "/srv/site" {
		t.Errorf("Root = %q", m.Root)
	}
	if m.CacheDir != "/var/cache/md" {
		t.Errorf("CacheDir = %q", m.CacheDir)
	}
	if m.URLSuffix != ".md" {
		t.Errorf("URLSuffix = %q", m.URLSuffix)
	}
	if m.QueryParam != "format" {
		t.Errorf("QueryParam = %q", m.QueryParam)
	}
	if m.CacheSize != 8192 {
		t.Errorf("CacheSize = %d", m.CacheSize)
	}
	if time.Duration(m.CacheTTL) != 2*time.Hour {
		t.Errorf("CacheTTL = %v", time.Duration(m.CacheTTL))
	}
	if m.MaxBodyBytes != 10485760 {
		t.Errorf("MaxBodyBytes = %d", m.MaxBodyBytes)
	}
	if !m.Pregenerate {
		t.Error("Pregenerate should be true")
	}
	if m.MainSelector != "article" {
		t.Errorf("MainSelector = %q", m.MainSelector)
	}
	if len(m.StripTags) != 3 || m.StripTags[0] != "nav" {
		t.Errorf("StripTags = %v", m.StripTags)
	}
	if len(m.StripSelectors) != 2 || m.StripSelectors[1] != "#cookie" {
		t.Errorf("StripSelectors = %v", m.StripSelectors)
	}
}

func TestUnmarshalCaddyfileMinimal(t *testing.T) {
	d := caddyfile.NewTestDispenser(`markdown_for_agents`)
	var m MarkdownForAgents
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if m.Root != "" {
		t.Errorf("Root should be empty, got %q", m.Root)
	}
}

func TestUnmarshalCaddyfileUnknownDirective(t *testing.T) {
	d := caddyfile.NewTestDispenser(`markdown_for_agents { nonsense }`)
	var m MarkdownForAgents
	if err := m.UnmarshalCaddyfile(d); err == nil {
		t.Error("expected error for unknown subdirective")
	}
}
