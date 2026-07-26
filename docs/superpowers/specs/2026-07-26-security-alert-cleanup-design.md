# Security Alert Cleanup Design

## Goal

Clear the repository's actionable high-, medium-, and low-severity code-scanning
findings without weakening the existing Caddy security pin, then dismiss the two
remaining Caddy findings only after documenting why they are scanner false
positives.

## Audit Findings

The latest scan of `main` reports five open alerts:

- Alert `#95` is a high-severity `std/os` vulnerability in Go 1.26.4. Go
  1.26.5 contains the fix.
- Alert `#94` is a medium-severity `std/crypto/tls` vulnerability in Go
  1.26.4. The same Go 1.26.5 update contains the fix.
- Alert `#96` is a low-severity `c-ares` vulnerability in the Alpine runtime
  image. Alpine package `c-ares >= 1.34.8-r0` contains the fix.
- Alerts `#92` and `#93` report CVE-2026-52845 against the Caddy binary.
  They are version-range false positives: the pinned Caddy source commit
  `b2693fb63a30` is 23 commits after the upstream fix
  `fcba554d658b`, with the fix in its ancestry.

The official Go vulnerability scanner reports zero reachable vulnerabilities.
The current Caddy pin contains all published Caddy security fixes and is ten
commits behind upstream `master`. The latest stable Caddy release remains
v2.11.4, so replacing the pseudo-version with a release would be a security
downgrade.

## Considered Approaches

### 1. Patch actionable findings and dismiss proven false positives

Update the Go toolchain and Alpine runtime package, keep the patched Caddy
pseudo-version, and dismiss only alerts `#92` and `#93` with the upstream commit
ancestry as evidence.

This is the selected approach. It fixes real vulnerabilities, retains the
newest Caddy security fixes, and makes the alert dashboard accurately reflect
the repository's risk.

### 2. Move Caddy to current upstream `master`

This would pick up ten non-urgent upstream fixes and broad transitive dependency
updates. It would not reliably clear the Snyk alerts because the resulting
pseudo-version still compares as pre-v2.11.5. The compatibility surface is
larger without improving this maintenance outcome.

### 3. Wait for Caddy v2.11.5

This avoids dismissals but leaves two actionable Go findings and one Alpine
finding open. It also has no known release date. Waiting is not appropriate for
the high- and medium-severity toolchain fixes.

## Repository Changes

### Go toolchain

Change the `toolchain` directive in `go.mod` from `go1.26.4` to `go1.26.5`.
Update the matching version references in `README.md` and `CLAUDE.md` so the
documented security procedure remains accurate.

The `go 1.26.0` language minimum and CI's `go-version: '1.26'` remain unchanged.
The latter already resolves the latest Go 1.26 patch release.

### Alpine runtime

Add `c-ares` to both branches of the existing `apk upgrade --no-cache` command
in `Dockerfile`. This preserves the current fallback for Alpine package-name
differences while ensuring the installed runtime package is upgraded to the
patched repository build.

No base-image tag or digest change is required.

### Caddy

Keep:

`github.com/caddyserver/caddy/v2 v2.11.5-0.20260711231708-b2693fb63a30`

Do not update CEL independently and do not downgrade Caddy to v2.11.4.

## Alert Lifecycle

After the branch passes local verification and the hosted scan confirms the new
artifact:

1. Allow alerts `#94`, `#95`, and `#96` to close automatically when the updated
   SARIF results no longer contain them.
2. Dismiss alerts `#92` and `#93` as `false positive`.
3. Use this dismissal comment on both alerts:

   `Pinned Caddy commit b2693fb63a30 contains the CVE-2026-52845 fix
   fcba554d658b in its ancestry (23 commits ahead, 0 behind). Snyk is
   interpreting the pre-v2.11.5 pseudo-version by range rather than source
   contents.`

No Dependabot or secret-scanning alerts will be modified.

## Verification

Run the following against Go 1.26.5:

- `gofmt -l .`
- `go mod tidy` followed by a clean module diff check
- `go mod verify`
- `go vet ./...`
- `go test -race -coverprofile=coverage.out ./...`
- `govulncheck ./...`
- `xcaddy build` using the Caddy version resolved from `go.mod`
- Caddy module-registration smoke test
- Full `docker build`
- Inspect the built image to confirm Go 1.26.5 and patched `c-ares`

Push a dedicated maintenance PR and wait for CI, CodeQL, Snyk SCA, and Snyk
Container checks. Alert dismissal occurs only after the hosted checks complete
successfully on the new head commit.

## Scope Boundaries

- No application behavior changes.
- No upgrade to CEL 0.30.0.
- No move to Caddy `master`.
- No changes to previously dismissed CodeQL path-injection findings.
- No suppression rules or blanket scanner ignores.
