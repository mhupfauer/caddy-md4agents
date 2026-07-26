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

- **Go 1.26 minimum** (`go 1.26.0` directive). The `toolchain` directive
  (currently `go1.26.5`) is the actual stdlib version Snyk scans via the
  binary's embedded buildinfo — bump it to the latest 1.26.x patch to clear
  stdlib CVEs, never lower it. Keep the build-stage `GO_BUILDER_TAG` in the
  Dockerfile on the matching `1.26-alpine` floating tag so the published
  image's stdlib stays in lockstep. See [Clearing CVEs](#clearing-cves).
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

### Clearing CVEs

Snyk alerts on this repo fall into three buckets, each with a fixed remedy:

- **Go stdlib CVEs (`go.mod`)** — bump the `toolchain` directive to the
  latest 1.26.x patch (`go test ./... && go vet ./...` to confirm), then let
  CI's `xcaddy build` re-embed the patched buildinfo. Verify with
  `go version <binary>` → should report the new toolchain.
- **Base-image package CVEs (`Dockerfile`, e.g. curl/openssl)** — the runtime
  stage runs `apk upgrade --no-cache <pkgs>` to pull Alpine's patched build
  ahead of an upstream `caddy` rebuild. Add the flagged package to that line;
  it no-ops cleanly if Alpine hasn't shipped the fix yet. `base-image-refresh.yml`
  picks up the upstream fix automatically once published.
- **Dependency CVEs (module graph)** — `go get <module>@<fixed>` +
  `go mod tidy`, mirroring the existing Dependabot `go-deps` group. If the
  upstream fix is only on `master` (no tagged release), pin to the fix commit
  (`go get <module>@<commit>` → pseudo-version) and revert to the release once
  published. For `github.com/caddyserver/caddy/v2` specifically, `xcaddy` must
  be given the version — CI and the Dockerfile pass
  `$(go list -m -f '{{.Version}}' github.com/caddyserver/caddy/v2)` — or it
  builds the latest *release* and the container binary ships the vulnerable
  Caddy even though `go.mod` is patched (SCA passes, Container scan still fails).

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
