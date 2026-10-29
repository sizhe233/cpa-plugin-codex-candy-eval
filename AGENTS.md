# Repository Instructions

## Scope and Layout

Inspect relevant source before changing integration contracts. Keep changes focused and preserve unrelated work.

- `../CLIProxyAPI/`: CPA host; `../Cli-Proxy-API-Management-Center/`: management console (CPAMC).
- `cmd/cpa-codex-candy-eval/`: shared-library entry point and C ABI bridge; `internal/plugin/`: handlers, runners, persistence, and tests.
- `internal/plugin/web/`: embedded UI and vendored renderers; `internal/plugin/data/`: embedded fingerprint detection data.
- `install.sh`, `install.ps1`: public installers; `.github/workflows/release.yml`: release checks, builds, and packaging.

## Build and Checks

For code changes, run `gofmt` on changed Go files, `go vet ./...`, and `go test -race ./...`. Update tests when behavior changes. For UI changes, also run `node scripts/test-ui.cjs`.

`scripts/deploy.sh` builds the Linux/amd64 CGO shared library with Zig (`brew install zig` on macOS), targeting glibc 2.17. Keep build artifacts in ignored `dist/` and cross-compilation variables scoped to each command. Verify the output is ELF x86-64 and loads in the CPA container.

## Private Configuration

Read deployment values from ignored `.env`; `.env.example` is a placeholder template. Keep `.env` at mode `600`, untracked, and out of uploads. Never expose credentials or private deployment values in logs, screenshots, commits, or reports.

`CPA_SSH_HOST` selects the server; `CPA_REMOTE_PLUGIN_DIR` and `CPA_REMOTE_COMPOSE_FILE` are relative to the remote user's home. `CPA_BASE_URL` and `CPA_MANAGEMENT_PASSWORD` provide the verification URL and login.

## Deployment and Verification

This fork is deployed to production only by the operator, following the
deployment workspace's runbook (CI release artifacts with verified hashes,
isolated preflight, explicit confirmation). Agents must not deploy, restart
CPA, log in to a production panel, or run `scripts/deploy.sh` on their own
initiative; every production write needs explicit operator confirmation in
the current conversation. `scripts/deploy.sh` is kept for upstream parity
only and is not the production path.

Verify changes locally: `go vet ./...`, `go test -race ./...`, and
`node scripts/test-ui.cjs`. Report checks and any incomplete verification.

## Commits and Releases

Use an imperative commit subject; add a blank line and a body wrapped at 72 characters for nontrivial changes. Keep `pluginVersion` aligned with the `v<version>` release tag and follow `.github/workflows/release.yml`.
