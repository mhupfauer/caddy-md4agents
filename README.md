# caddy-md4agents

A [Caddy v2](https://caddyserver.com/) HTTP middleware that serves a Markdown
rendition of HTML pages when a client (typically an AI agent) negotiates for
it. It implements Cloudflare's [Markdown for Agents](https://developers.cloudflare.com/fundamentals/reference/markdown-for-agents/)
conventions on top of Caddy's static and dynamic handlers.

## Design

Three things kept it small and fast:

1. **Static-first.** When `root` is set, requests resolve to disk: an
   author-written `*.md` wins, otherwise the matching `*.html` is converted
   on first hit and written to a sidecar cache.
2. **Lazy in-memory + disk write-through.** First request to a page pays the
   conversion cost (~ms); subsequent requests serve from a sized LRU. Disk
   sidecars survive restarts and worker recycling.
3. **Stat-based invalidation.** Every request stats the source HTML and keys
   the cache on `path | mtime | size`. Edits invalidate automatically, no
   watcher required.

A capture-and-convert fallback handles dynamic upstreams (reverse proxy,
templates, anything that doesn't resolve to a file on disk).

## Content negotiation

A request is served Markdown when any of these is true (in order):

| Trigger | Example |
| ------- | ------- |
| URL suffix | `GET /docs/page.md` |
| Query param | `GET /docs/page?format=md` |
| `Accept` header | `Accept: text/markdown` (with q-value handling vs `text/html`) |

The first two are stripped before the inner handler sees the request, so the
upstream still resolves the underlying HTML resource.

## Build

This is a Caddy plugin, so it needs to be compiled into a Caddy binary with
[`xcaddy`](https://github.com/caddyserver/xcaddy):

```sh
xcaddy build --with github.com/mhupfauer/caddy-md4agents
```

## Caddyfile

Minimal — static site, defaults everywhere:

```caddy
example.com {
    root * /var/www/site
    markdown_for_agents /var/www/site
    file_server
}
```

Full options:

```caddy
example.com {
    root * /var/www/site

    markdown_for_agents {
        root            /var/www/site
        cache_dir       /var/cache/md4agents
        url_suffix      .md
        query_param     format
        cache_size      8192
        cache_ttl       24h
        max_body_bytes  8388608
        pregenerate
        main_selector   article
        strip_tags      script style noscript nav footer aside
        strip_selectors .ads "#cookie-banner"   # quote id selectors — # is a Caddyfile comment
    }

    file_server
}
```

Reverse-proxy mode (no `root` → uses capture path with the LRU only):

```caddy
example.com {
    markdown_for_agents {
        strip_selectors nav footer .site-chrome
        main_selector   main
    }
    reverse_proxy backend:8080
}
```

## Configuration reference

| Field | Default | Notes |
| ----- | ------- | ----- |
| `root` | — | Static file root. Enables the static-first path. |
| `cache_dir` | `<root>/.md4agents` | Disk write-through cache for generated MD. |
| `url_suffix` | `.md` | URL suffix that requests Markdown. Empty disables. |
| `query_param` | `format` | Query param checked for `md`/`markdown`. Empty disables. |
| `cache_size` | `4096` | In-memory LRU capacity. |
| `cache_ttl` | `0` (no expiry) | TTL for in-memory entries; disk uses mtime. |
| `max_body_bytes` | `4194304` | Dynamic-path body cap; over-cap streams through unconverted. |
| `pregenerate` | `false` | Walk `root` on startup and warm the cache. Optional. |
| `main_selector` | — | If set, only this element's subtree is converted. |
| `strip_tags` | `script style noscript iframe svg` | Tags removed entirely from output. |
| `strip_selectors` | — | Simple `tag`, `.class`, `#id` selectors removed pre-conversion. |

## Cache layout

```
<root>/                          # original site
  docs/about.html
  docs/about.md                  # OPTIONAL: author-written, served verbatim

<cache_dir>/                     # generated; safe to delete
  docs/about.html.md             # sidecar for docs/about.html
```

The `.html.md` double-extension keeps generated artifacts impossible-by-
convention to confuse with author Markdown.

## Response headers

- `Content-Type: text/markdown; charset=utf-8`
- `ETag: "<sha256[:16]>"` (strong; supports `If-None-Match` → 304)
- `Vary: Accept`

## Performance notes

- The HTML→Markdown converter is built once at provision time and is
  goroutine-safe (per html-to-markdown/v2 contract).
- A single-flight pattern collapses concurrent identical conversions into
  one execution, preventing thundering-herd cost on a cold cache.
- The hot path on a warmed cache is a `stat()` + map lookup + write — no
  parsing, no allocations beyond the response itself.

## License

MIT
