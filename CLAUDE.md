# caddy-md4agents

Caddy v2 HTTP middleware that serves a Markdown rendition of HTML pages when a
client negotiates for it (URL suffix `.md`, query `?format=md`, or
`Accept: text/markdown`). Implements Cloudflare's "Markdown for Agents"
convention. Live at https://hupfauer.one — see README.md for the design
write-up, configuration reference, and security notes.

## Repo layout

Single Go package, flat layout. No `cmd/` or `internal/` — Caddy plugins
register themselves via `init()`.

| File | Role |
|---|---|
| `md4agents.go` | Module type, `Provision` / `ServeHTTP` / `Cleanup`, headers, ETag, cache key |
| `caddyfile.go` | Caddyfile parser (`UnmarshalCaddyfile`), directive registration |
| `static.go` | Static-first path: resolves to disk, author `*.md` wins, sidecar cache under `cache_dir` |
| `capture.go` | Dynamic path: response capture writer + buffering with size cap |
| `converter.go` | HTML→Markdown via `html-to-markdown/v2` + selector pre-processing (`main_selector`, `strip_selectors`) |
| `cache.go` | LRU + byte-budget + TTL + singleflight (`do()`) |
| `negotiation.go` | Accept-header parsing, q-value preference vs `text/html` |
| `janitor.go` | Periodic orphan-sidecar sweep when `janitor_interval > 0` |
| `pathsafe.go` | Symlink resolution + `root` containment check (TOCTOU-safe) |
| `pregenerator.go` | Optional startup walk that warms the cache |
| `*_test.go` | Unit + hardening tests; covers cache safety, headers, negotiation, symlink escape, oversized bodies |

## Development

```sh
go test ./...                                    # full suite (~0.5s)
go test -race -coverprofile=coverage.out ./...   # what CI runs
go vet ./...
gofmt -l .                                       # CI fails on diff
```

End-to-end (matches CI `build` job):

```sh
go install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.5
xcaddy build --with github.com/mhupfauer/caddy-md4agents=. --output ./caddy
./caddy list-modules | grep '^http.handlers.markdown_for_agents$'
```

Docker (matches published image):

```sh
docker build -t caddy-md4agents:dev .
```

## Conventions

- **Go 1.26 minimum** (`go.mod` directive). Bumped to clear stdlib CVEs
  flagged by Snyk; don't lower without replacement.
- **gofmt strict** — CI fails on any `gofmt -l` output.
- **No new top-level packages.** Plugin is intentionally one flat package;
  if a concept needs separation, use a new `*.go` file.
- **No `panic` in handlers.** Errors propagate via `caddyhttp.Error` or
  inline 5xx writes. See `convertWithTimeout` for the timeout-→503 pattern.
- **Sanitize before returning errors to clients** — `TestErrorMessagesDontLeakPaths`
  guards against leaking filesystem paths in error responses.
- **Cache invariants are tested, not asserted.** Any change to `cache.go`
  must keep `TestCacheByteBudgetEvicts`,
  `TestCachePutConcurrentDoesNotOverAdmit`, and
  `TestCachePutOverwriteReclaimsBytes` green.

## Caddyfile gotcha (also in README)

```caddy
markdown_for_agents /var/www/site     # WRONG — `/...` is parsed as a path matcher
markdown_for_agents { root /var/www/site }  # right
```

## Security / scanning

- **CodeQL** on push + weekly cron.
- **Snyk** SAST/SCA/IaC/Container — gated on `SNYK_TOKEN` repo secret;
  preflight short-circuits cleanly when the secret is absent.
- Vuln reports: GitHub Security Advisories (private). Don't open public
  issues for security findings.

## CI tag → image flow

- `ci.yml` runs tests + `xcaddy build` on every push/PR.
- `docker.yml` builds & publishes `ghcr.io/mhupfauer/caddy-md4agents` on
  tag pushes and manual dispatch — **not** on every push to main.
- `base-image-refresh.yml` rebuilds daily, but only if the upstream
  `caddy` base image SHA changed.

## When making changes

- Touching the cache or response path? Run `go test -race ./...` and eyeball
  `cachesafety_test.go` + `hardening_test.go` — both are scenario tests, not
  unit tests, so failures usually indicate real regressions.
- Touching `Provision` or Caddyfile parsing? Update both `caddyfile_test.go`
  and the README configuration reference table.
- Adding a config field? Mirror it in: the struct in `md4agents.go`, the
  parser in `caddyfile.go`, the README table, and the godoc on the struct.
