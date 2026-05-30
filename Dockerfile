# syntax=docker/dockerfile:1.7

# Pin Caddy version at build time. Defaults to `latest` (i.e. whatever
# the upstream Caddy team is shipping); CI rebuilds on the daily cron
# so this image follows upstream automatically.
ARG CADDY_VERSION=latest

# ----- build stage -----------------------------------------------------------
FROM caddy:${CADDY_VERSION}-builder AS builder

# Bring our plugin source into the build context so xcaddy can resolve
# the local module instead of fetching from a public git ref. This makes
# CI builds verify the *current* commit, not whatever is at HEAD of main.
WORKDIR /src
COPY . .

# xcaddy: compose Caddy with our handler. The =/src tells xcaddy to use
# the local module path (a go.mod replace under the hood) so we link
# against this checkout instead of going through `go get`.
RUN xcaddy build \
    --with github.com/mhupfauer/caddy-md4agents=/src \
    --output /out/caddy

# Smoke test inside the builder so a regression aborts the build before
# we publish the runtime image.
RUN /out/caddy version \
 && /out/caddy list-modules | grep -q '^http.handlers.markdown_for_agents$'

# ----- runtime stage ---------------------------------------------------------
FROM caddy:${CADDY_VERSION}

COPY --from=builder /out/caddy /usr/bin/caddy

# Default Caddyfile lives at /etc/caddy/Caddyfile (Caddy convention).
# Override at runtime with `-v ./Caddyfile:/etc/caddy/Caddyfile:ro`.
EXPOSE 80 443 443/udp

LABEL org.opencontainers.image.title="caddy-md4agents" \
      org.opencontainers.image.description="Caddy v2 + the markdown_for_agents handler" \
      org.opencontainers.image.source="https://github.com/mhupfauer/caddy-md4agents" \
      org.opencontainers.image.licenses="MIT"
