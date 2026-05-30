package md4agents

import (
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("markdown_for_agents", parseCaddyfile)
	// Must run before file_server so it can wrap the response. Otherwise
	// file_server writes HTML and our middleware never sees it.
	httpcaddyfile.RegisterDirectiveOrder("markdown_for_agents", httpcaddyfile.Before, "file_server")
}

// Caddyfile syntax:
//
//	markdown_for_agents [<root>] {
//	    root            <path>
//	    cache_dir       <path>
//	    url_suffix      <string>   # default .md ("" to disable)
//	    query_param     <string>   # default format ("" to disable)
//	    cache_size      <int>      # default 4096
//	    cache_ttl       <duration> # default 0 (no expiry)
//	    max_body_bytes  <int>      # default 4194304
//	    pregenerate
//	    main_selector   <selector>
//	    strip_tags      <tag> [<tag>...]
//	    strip_selectors <selector> [<selector>...]
//	}
//
// The optional inline argument is a shortcut for `root`.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m MarkdownForAgents
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *MarkdownForAgents) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			m.Root = d.Val()
			if d.NextArg() {
				return d.ArgErr()
			}
		}
		for d.NextBlock(0) {
			switch d.Val() {
			case "root":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Root = d.Val()
			case "cache_dir":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.CacheDir = d.Val()
			case "url_suffix":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.URLSuffix = d.Val()
			case "query_param":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.QueryParam = d.Val()
			case "cache_size":
				if !d.NextArg() {
					return d.ArgErr()
				}
				n, err := strconv.Atoi(d.Val())
				if err != nil {
					return d.Errf("cache_size: %v", err)
				}
				m.CacheSize = n
			case "cache_ttl":
				if !d.NextArg() {
					return d.ArgErr()
				}
				if d.Val() == "never" || d.Val() == "0" {
					// Translate to the negative sentinel; Provision's
					// zero-default would otherwise overwrite an
					// operator's explicit "no expiry" choice.
					m.CacheTTL = caddy.Duration(-1)
					break
				}
				dur, err := time.ParseDuration(d.Val())
				if err != nil {
					return d.Errf("cache_ttl: %v", err)
				}
				m.CacheTTL = caddy.Duration(dur)
			case "max_body_bytes":
				if !d.NextArg() {
					return d.ArgErr()
				}
				n, err := strconv.ParseInt(d.Val(), 10, 64)
				if err != nil {
					return d.Errf("max_body_bytes: %v", err)
				}
				m.MaxBodyBytes = n
			case "convert_timeout":
				if !d.NextArg() {
					return d.ArgErr()
				}
				dur, err := time.ParseDuration(d.Val())
				if err != nil {
					return d.Errf("convert_timeout: %v", err)
				}
				m.ConvertTimeout = caddy.Duration(dur)
			case "max_concurrent":
				if !d.NextArg() {
					return d.ArgErr()
				}
				n, err := strconv.Atoi(d.Val())
				if err != nil {
					return d.Errf("max_concurrent: %v", err)
				}
				m.MaxConcurrent = n
			case "cache_bytes":
				if !d.NextArg() {
					return d.ArgErr()
				}
				n, err := strconv.ParseInt(d.Val(), 10, 64)
				if err != nil {
					return d.Errf("cache_bytes: %v", err)
				}
				m.CacheBytes = n
			case "cache_entry_bytes":
				if !d.NextArg() {
					return d.ArgErr()
				}
				n, err := strconv.ParseInt(d.Val(), 10, 64)
				if err != nil {
					return d.Errf("cache_entry_bytes: %v", err)
				}
				m.CacheEntryBytes = n
			case "janitor_interval":
				if !d.NextArg() {
					return d.ArgErr()
				}
				dur, err := time.ParseDuration(d.Val())
				if err != nil {
					return d.Errf("janitor_interval: %v", err)
				}
				m.JanitorInterval = caddy.Duration(dur)
			case "pregenerate":
				m.Pregenerate = true
			case "allow_authenticated":
				m.AllowAuthenticated = true
			case "main_selector":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.MainSelector = d.Val()
			case "strip_tags":
				m.StripTags = append(m.StripTags, d.RemainingArgs()...)
			case "strip_selectors":
				m.StripSelectors = append(m.StripSelectors, d.RemainingArgs()...)
			default:
				return d.Errf("unknown subdirective: %s", d.Val())
			}
		}
	}
	return nil
}
