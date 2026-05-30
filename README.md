# caddy-md4agents

[![CI](https://github.com/mhupfauer/caddy-md4agents/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/mhupfauer/caddy-md4agents/actions/workflows/ci.yml)
[![Docker](https://github.com/mhupfauer/caddy-md4agents/actions/workflows/docker.yml/badge.svg?branch=main)](https://github.com/mhupfauer/caddy-md4agents/actions/workflows/docker.yml)
[![codecov](https://codecov.io/gh/mhupfauer/caddy-md4agents/branch/main/graph/badge.svg)](https://codecov.io/gh/mhupfauer/caddy-md4agents)
[![CodeQL](https://github.com/mhupfauer/caddy-md4agents/actions/workflows/github-code-scanning/codeql/badge.svg?branch=main)](https://github.com/mhupfauer/caddy-md4agents/security/code-scanning)
[![Snyk](https://github.com/mhupfauer/caddy-md4agents/actions/workflows/snyk.yml/badge.svg?branch=main)](https://github.com/mhupfauer/caddy-md4agents/actions/workflows/snyk.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/mhupfauer/caddy-md4agents)](go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/mhupfauer/caddy-md4agents)](https://goreportcard.com/report/github.com/mhupfauer/caddy-md4agents)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Container](https://img.shields.io/badge/ghcr.io-mhupfauer%2Fcaddy--md4agents-2496ed?logo=docker&logoColor=white)](https://github.com/users/mhupfauer/packages/container/package/caddy-md4agents)

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

## Docker

A pre-built image follows the upstream `caddy` release cadence and is
rebuilt daily so it picks up Caddy base-image updates automatically:

```sh
docker pull ghcr.io/mhupfauer/caddy-md4agents:latest
```

Tags:

| Tag                    | Pointer                                    |
| ---------------------- | ------------------------------------------ |
| `latest`               | Last successful build of `main`            |
| `caddy-<version>`      | Built against that upstream Caddy release  |
| `sha-<short-sha>`      | Built from that exact commit               |

Run with a Caddyfile mounted at the standard path:

```sh
docker run --rm -p 80:80 -p 443:443 -p 443:443/udp \
  -v $PWD/Caddyfile:/etc/caddy/Caddyfile:ro \
  -v $PWD/site:/srv/site:ro \
  ghcr.io/mhupfauer/caddy-md4agents:latest
```

Or build locally against the current checkout:

```sh
docker build -t caddy-md4agents:dev .
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
        root                /var/www/site
        cache_dir           /var/cache/md4agents
        url_suffix          .md
        query_param         format
        cache_size          8192
        cache_ttl           24h
        max_body_bytes      8388608
        convert_timeout     5s
        pregenerate
        allow_authenticated     # opt-in: cache responses for authenticated requests
        main_selector       article
        strip_tags          script style noscript nav footer aside
        strip_selectors     .ads "#cookie-banner"   # quote id selectors — # is a Caddyfile comment
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
| `cache_dir` | `<caddy-data-dir>/md4agents/<hash>` | Disk write-through cache; lives outside `root`. |
| `url_suffix` | `.md` | URL suffix that requests Markdown. Empty disables. |
| `query_param` | `format` | Query param checked for `md`/`markdown`. Empty disables. |
| `cache_size` | `4096` | In-memory LRU entry count. |
| `cache_bytes` | `268435456` (256 MiB) | Total in-memory cache byte budget. |
| `cache_entry_bytes` | `1048576` (1 MiB) | Per-entry cap; oversized entries are rejected. |
| `cache_ttl` | `15m` | TTL for in-memory entries; disk uses mtime. Use `cache_ttl 0` or `cache_ttl never` to disable TTL eviction entirely (operator must purge manually). |
| `max_body_bytes` | `4194304` (4 MiB) | Source HTML size cap (both static disk reads and dynamic captures). |
| `convert_timeout` | `5s` | Per-conversion timeout; on exceed, returns 503. |
| `max_concurrent` | `max(4, NumCPU)` | Conversion semaphore — bounds CPU/goroutine usage. |
| `pregenerate` | `false` | Walk `root` on startup and warm the cache. |
| `janitor_interval` | `0` (off) | Periodic orphan-sidecar cleanup interval. |
| `allow_authenticated` | `false` | If true, cache responses to requests carrying `Authorization`/`Cookie`. |
| `main_selector` | — | If set, only this element's subtree is converted. |
| `strip_tags` | `script style noscript iframe svg` | Tags removed entirely from output. |
| `strip_selectors` | — | Simple `tag`, `.class`, `#id` selectors removed pre-conversion. |

## Cache safety

The shared cache is, well, shared, so a few hard rules apply:

- Only `GET` and `HEAD` are cacheable.
- Requests with `Authorization` or `Cookie` headers bypass the cache by
  default. Set `allow_authenticated` only when upstream content is not
  user-specific.
- Upstream responses carrying `Set-Cookie`, `Cache-Control: private`,
  `Cache-Control: no-store`, or a non-trivial `Vary` (anything beyond
  `Accept-Encoding`) are converted and served once but never cached.
- The dynamic-path cache key is `path + ?query`, so `/api/p?id=1` and
  `/api/p?id=2` do not collide.
- The in-memory cache is bounded by both entry count and total bytes;
  per-entry oversized responses are rejected outright.

## Authorization placement

The static-first path serves matched files directly without calling the
next handler. That means any `basicauth`, `forward_auth`, `jwtauth`, or
similar middleware that comes **after** `markdown_for_agents` in the
Caddyfile chain will not run for markdown responses.

In Caddy, the directive runs immediately before `file_server`, so the
default placement of auth middleware (which is `before file_server`) is
safe. If you place auth in a `handle` block that wraps both this module
and `file_server`, ordering is preserved and auth still runs first.

If your config puts auth after `file_server` (unusual), or applies path
matchers that only target `*.html`, ensure the matcher also covers the
URL suffix you've configured for markdown negotiation — e.g.
`path *.html *.md`.

## Personalization beyond cookies

The `Authorization` and `Cookie` headers bypass the shared cache by
default. If your application personalizes responses on **other**
signals — mTLS, `X-Forwarded-User`, IP-based ACL — those are not
considered cache-safe automatically. Either set `Cache-Control: private`
on the upstream response (this module will honor it) or run separate
module instances per personalization dimension.

## File system semantics

- Paths are canonicalized at provision time and on every request via
  `filepath.EvalSymlinks`. If a request's target resolves outside `root`
  (e.g. via a symlink in the tree), the module returns `404` and refuses
  to fall through, preventing a downstream `file_server` + dynamic-path
  conversion from leaking the file.
- Source HTML is opened once and stat'd from the file descriptor, closing
  the TOCTOU window between `stat` and `read`.
- The default `cache_dir` lives in `caddy.AppDataDir()/md4agents/<hash>`
  outside `root`. The segment name is a SHA-256 prefix only — the
  original path's basename is never written into the data dir.
- HEAD responses include all headers (including `Content-Length`) but no
  body, per RFC 9110 §15.3.
- Upstream headers from the dynamic path are whitelist-forwarded:
  `Cache-Control`, `Expires`, `Last-Modified`, `Content-Language`,
  `Content-Security-Policy`, `Strict-Transport-Security`, `X-Frame-Options`,
  `X-Content-Type-Options`, `Referrer-Policy`, `Permissions-Policy`.
  Everything else (notably `Set-Cookie`, `Server`, `X-Powered-By`) is
  dropped.

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

## Security

Two scanners run on every push and weekly on a cron:

- **CodeQL** (GitHub native) — Go SAST + dependency scanning. Findings
  appear under the repo's Security → Code scanning tab.
- **Snyk** — SAST (Snyk Code → SARIF → GitHub Code Scanning), SCA
  (`snyk monitor --all-projects`), Infrastructure-as-Code, and Container
  scans against the built Docker image.

Snyk needs a `SNYK_TOKEN` repo secret (free Snyk account → API token →
GitHub Settings → Secrets → `SNYK_TOKEN`). When the token is missing
the workflow short-circuits in its preflight job so PRs and the initial
setup window don't fail noisily.

Vulnerabilities can also be reported privately via
[GitHub Security Advisories](https://github.com/mhupfauer/caddy-md4agents/security/advisories/new).

## License

MIT
