# Security Alert Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the actionable Go and Alpine security findings while retaining the patched Caddy source pin, then dismiss only the two Caddy scanner false positives with reproducible upstream evidence.

**Architecture:** Treat `go.mod` as the source of truth for the Go toolchain and the existing Alpine `apk upgrade` command as the runtime-package patch point. Verify both configuration changes with failing-before/passing-after shell assertions, then exercise the complete Go, xcaddy, and Docker build paths before using GitHub's alert API.

**Tech Stack:** Go 1.26.5, Caddy/xcaddy, Alpine Linux, Docker, GitHub Actions, CodeQL, Snyk, GitHub CLI.

## Global Constraints

- Keep `github.com/caddyserver/caddy/v2 v2.11.5-0.20260711231708-b2693fb63a30`.
- Do not update `github.com/google/cel-go` to v0.30.0.
- Keep the `go 1.26.0` language minimum and CI `go-version: '1.26'`.
- Change the toolchain directive and documentation from exactly `go1.26.4` to `go1.26.5`.
- Upgrade Alpine `c-ares` through both branches of the existing `apk upgrade --no-cache` command.
- Do not add scanner suppressions or blanket ignores.
- Dismiss only code-scanning alerts `#92` and `#93`, using reason `false positive` and the exact evidence comment specified in Task 2.
- Do not modify Dependabot alerts, secret-scanning alerts, or dismissed CodeQL path-injection findings.

---

### Task 1: Patch the Go toolchain and Alpine runtime package

**Files:**
- Modify: `go.mod`
- Modify: `Dockerfile`
- Modify: `README.md`
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes: The existing `toolchain go1.26.4` directive, the documented toolchain references, and the two-branch Alpine package-upgrade command.
- Produces: A Go 1.26.5 module/build declaration and a runtime image that explicitly upgrades `c-ares`.

- [ ] **Step 1: Run focused assertions and verify the desired configuration is absent**

Run each command independently:

```bash
rg -n '^toolchain go1\.26\.5$' go.mod
rg -n 'currently `go1\.26\.5`' README.md
rg -n 'currently `go1\.26\.5`' CLAUDE.md
rg -n 'apk upgrade --no-cache c-ares curl' Dockerfile
```

Expected: all four commands exit 1 because the security updates are not yet present.

- [ ] **Step 2: Apply the minimal configuration changes**

Change `go.mod`:

```go
toolchain go1.26.5
```

Change the matching prose in `README.md` and `CLAUDE.md` from:

```text
currently `go1.26.4`
```

to:

```text
currently `go1.26.5`
```

Change the Dockerfile runtime upgrade command to:

```dockerfile
RUN apk upgrade --no-cache c-ares curl libcurl openssl libcrypto3 libssl3 2>/dev/null \
 || apk upgrade --no-cache c-ares curl openssl
```

- [ ] **Step 3: Refresh and verify the module graph**

Run:

```bash
GOTOOLCHAIN=go1.26.5 go mod tidy
git diff --check
GOTOOLCHAIN=go1.26.5 go mod verify
```

Expected: `go mod tidy` succeeds without dependency-version drift, `git diff --check` prints nothing, and module verification reports `all modules verified`.

- [ ] **Step 4: Re-run the focused assertions**

Run:

```bash
rg -n '^toolchain go1\.26\.5$' go.mod
rg -n 'currently `go1\.26\.5`' README.md
rg -n 'currently `go1\.26\.5`' CLAUDE.md
test "$(rg -c 'apk upgrade --no-cache c-ares curl' Dockerfile)" -eq 2
```

Expected: all commands exit 0 and the Dockerfile contains the patched package prefix in both fallback branches.

- [ ] **Step 5: Run the Go verification suite**

Run:

```bash
test -z "$(gofmt -l .)"
GOTOOLCHAIN=go1.26.5 go vet ./...
GOTOOLCHAIN=go1.26.5 go test -race -coverprofile=coverage.out ./...
GOTOOLCHAIN=go1.26.5 go tool cover -func=coverage.out | tail -1
GOTOOLCHAIN=go1.26.5 go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
```

Expected: formatting and vet pass, all race tests pass, coverage is reported, and govulncheck reports that the code is affected by zero vulnerabilities.

- [ ] **Step 6: Verify the xcaddy and Docker artifact paths**

Run:

```bash
GOTOOLCHAIN=go1.26.5 go install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.5
CADDY_VERSION="$(GOTOOLCHAIN=go1.26.5 go list -m -f '{{.Version}}' github.com/caddyserver/caddy/v2)"
"$(go env GOPATH)/bin/xcaddy" build "$CADDY_VERSION" \
  --with github.com/mhupfauer/caddy-md4agents=. \
  --output ./caddy
./caddy version
./caddy list-modules | rg '^http\.handlers\.markdown_for_agents$'
go version -m ./caddy | rg 'go1\.26\.5'
docker build -t caddy-md4agents:security-alert-cleanup .
docker run --rm --entrypoint sh caddy-md4agents:security-alert-cleanup \
  -c 'apk info -v c-ares'
```

Expected: xcaddy builds the Caddy pseudo-version pinned by `go.mod`, the plugin is registered, build metadata reports Go 1.26.5, the Docker build completes, and the runtime reports `c-ares` version `1.34.8-r0` or later.

- [ ] **Step 7: Confirm scope and commit**

Run:

```bash
git diff -- go.mod Dockerfile README.md CLAUDE.md
git status --short
git add go.mod Dockerfile README.md CLAUDE.md
git commit -m "fix(security): patch Go toolchain and Alpine c-ares"
```

Expected: only the four specified files are staged for the security patch, with no Caddy or CEL version change.

### Task 2: Validate hosted scans and clean up false-positive alerts

**Files:**
- No repository files are modified.

**Interfaces:**
- Consumes: Task 1's verified commit and GitHub code-scanning alerts `#92`, `#93`, `#94`, `#95`, and `#96`.
- Produces: A maintenance PR with green hosted checks and dismissed false-positive alerts `#92` and `#93`.

- [ ] **Step 1: Push the branch and create the maintenance PR**

Run:

```bash
git push -u origin maintenance/security-alert-cleanup
gh pr create \
  --base main \
  --head maintenance/security-alert-cleanup \
  --title "fix(security): patch Go toolchain and Alpine c-ares" \
  --body-file docs/superpowers/specs/2026-07-26-security-alert-cleanup-design.md
```

Expected: GitHub returns the URL of a new PR targeting `main`.

- [ ] **Step 2: Wait for every hosted check**

Run:

```bash
gh pr checks --watch --interval 10
```

Expected: CI vet/race tests, xcaddy build, CodeQL, Snyk Open Source, and Snyk Container all complete successfully.

- [ ] **Step 3: Verify the PR's scanner analyses**

Use the PR merge ref, because Snyk uploads SARIF against GitHub's synthetic
pull-request merge commit rather than the branch head:

```bash
PR_NUMBER="$(gh pr view --json number --jq .number)"
gh api 'repos/mhupfauer/caddy-md4agents/code-scanning/analyses?per_page=100' \
  --jq "map(select(.ref == \"refs/pull/${PR_NUMBER}/merge\"))
        | sort_by(.created_at)
        | group_by(.category)
        | map(last | {tool: .tool.name, category, results_count, error})"
```

Expected: the Snyk Open Source analysis no longer contains the Go 1.26.4 findings and the Snyk Docker-image analysis no longer contains the vulnerable `c-ares` package. Any remaining Caddy results correspond only to alerts `#92` and `#93`.

- [ ] **Step 4: Re-confirm Caddy fix ancestry immediately before dismissal**

Run:

```bash
gh api 'repos/caddyserver/caddy/compare/fcba554d658b0c5fa36f715b77094bb9f02ce799...b2693fb63a30e6d7be0972c3645e9a2c0a500e93' \
  --jq '{status, ahead_by, behind_by, merge_base_commit: .merge_base_commit.sha}'
```

Expected:

```json
{
  "status": "ahead",
  "ahead_by": 23,
  "behind_by": 0,
  "merge_base_commit": "fcba554d658b0c5fa36f715b77094bb9f02ce799"
}
```

- [ ] **Step 5: Dismiss alerts #92 and #93 with the approved evidence**

Use this exact comment for both requests:

```text
Pinned Caddy commit b2693fb63a30 contains the CVE-2026-52845 fix fcba554d658b in its ancestry (23 commits ahead, 0 behind). Snyk is interpreting the pre-v2.11.5 pseudo-version by range rather than source contents.
```

Run:

```bash
gh api --method PATCH repos/mhupfauer/caddy-md4agents/code-scanning/alerts/92 \
  -f state=dismissed \
  -f dismissed_reason='false positive' \
  -f dismissed_comment='Pinned Caddy commit b2693fb63a30 contains the CVE-2026-52845 fix fcba554d658b in its ancestry (23 commits ahead, 0 behind). Snyk is interpreting the pre-v2.11.5 pseudo-version by range rather than source contents.'
gh api --method PATCH repos/mhupfauer/caddy-md4agents/code-scanning/alerts/93 \
  -f state=dismissed \
  -f dismissed_reason='false positive' \
  -f dismissed_comment='Pinned Caddy commit b2693fb63a30 contains the CVE-2026-52845 fix fcba554d658b in its ancestry (23 commits ahead, 0 behind). Snyk is interpreting the pre-v2.11.5 pseudo-version by range rather than source contents.'
```

Expected: both responses report `state: dismissed`, `dismissed_reason: false positive`, and the exact evidence comment.

- [ ] **Step 6: Verify final PR and alert state**

Run:

```bash
gh pr checks
gh api repos/mhupfauer/caddy-md4agents/code-scanning/alerts/92 \
  --jq '{number,state,dismissed_reason,dismissed_comment}'
gh api repos/mhupfauer/caddy-md4agents/code-scanning/alerts/93 \
  --jq '{number,state,dismissed_reason,dismissed_comment}'
git status --short --branch
```

Expected: PR checks remain green, alerts `#92` and `#93` are dismissed as false positives with the approved comment, and the local branch is clean and synchronized with origin.
