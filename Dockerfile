# syntax=docker/dockerfile:1.7

# The Caddy team publishes `caddy:builder` (latest builder) and
# `caddy:<v>-builder` (versioned). There is no `caddy:latest-builder`
# tag, so we keep two separate args. Override both to pin a release.
ARG CADDY_BUILDER_TAG=builder
ARG CADDY_RUNTIME_TAG=latest

# ----- build stage -----------------------------------------------------------
FROM caddy:${CADDY_BUILDER_TAG} AS builder

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

COPY --from=builder /out/caddy /usr/bin/caddy

EXPOSE 80 443 443/udp

LABEL org.opencontainers.image.title="caddy-md4agents" \
      org.opencontainers.image.description="Caddy v2 + the markdown_for_agents handler" \
      org.opencontainers.image.source="https://github.com/mhupfauer/caddy-md4agents" \
      org.opencontainers.image.licenses="MIT"
