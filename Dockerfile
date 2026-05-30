# syntax=docker/dockerfile:1.7

# We build with an explicit modern Go toolchain (not caddy:builder) so the
# resulting binary's embedded buildinfo points at a Go stdlib without the
# CVEs Snyk flags for older toolchains. Runtime stays on the official
# caddy image; override either tag to pin a release.
ARG GO_BUILDER_TAG=1.26-alpine
ARG CADDY_RUNTIME_TAG=latest
# Pinned to a tagged xcaddy release so `go install` resolves the version
# through Go's checksum database (sumdb) and the build is reproducible.
ARG XCADDY_VERSION=v0.4.5

# ----- build stage -----------------------------------------------------------
FROM golang:${GO_BUILDER_TAG} AS builder

# git is required by `go install` / xcaddy to fetch module sources.
RUN apk add --no-cache git ca-certificates

# Install xcaddy into $GOPATH/bin (on PATH in the golang image).
RUN go install github.com/caddyserver/xcaddy/cmd/xcaddy@${XCADDY_VERSION}

WORKDIR /src
COPY . .

# xcaddy: compose Caddy with our handler against the local module path,
# so CI builds verify the *current* commit instead of fetching from a
# public git ref that might be racing this PR.
RUN xcaddy build \
    --with github.com/mhupfauer/caddy-md4agents=/src \
    --output /out/caddy

# Smoke test inside the builder so a regression aborts the build before
# we ever publish the runtime image.
RUN /out/caddy version \
 && /out/caddy list-modules | grep -q '^http.handlers.markdown_for_agents$'

# ----- runtime stage ---------------------------------------------------------
FROM caddy:${CADDY_RUNTIME_TAG}

# Pull patched curl (and libcurl, when present) from the Alpine repo to
# clear the curl CVEs Snyk flags against the base image. libcurl is not
# a separate package on all Alpine versions, so fall back to curl-only.
RUN apk upgrade --no-cache curl libcurl 2>/dev/null \
 || apk upgrade --no-cache curl

COPY --from=builder /out/caddy /usr/bin/caddy

EXPOSE 80 443 443/udp

LABEL org.opencontainers.image.title="caddy-md4agents" \
      org.opencontainers.image.description="Caddy v2 + the markdown_for_agents handler" \
      org.opencontainers.image.source="https://github.com/mhupfauer/caddy-md4agents" \
      org.opencontainers.image.licenses="MIT"
