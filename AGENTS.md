# AGENTS.md

## Overview

KindleVibe is a small local Go server that renders a Kindle-friendly usage dashboard for:

- GitHub Copilot
- Codex

The app is intentionally simple:

- one executable
- one main server file: `main.go`
- one sample config: `config.yaml`
- one public-facing doc: `README.MD`
- one convenience entrypoint: `Makefile`

There is no authentication layer. The app is meant for local-network use only.

## Project Structure

- `main.go`
  - config loading
  - Copilot usage fetch
  - Codex usage fetch
  - HTML template
  - HTTP server entrypoint
- `config.yaml`
  - sample local config
- `README.MD`
  - GitHub-facing setup and usage docs
- `Makefile`
  - default local build/run flow

## Provider Notes

### Copilot

- Source: `https://api.github.com/copilot_internal/user`
- Token resolution order:
  1. `COPILOT_API_TOKEN`
  2. `~/.config/github-copilot/apps.json`
- Do not hardcode tokens.
- Do not add any device-flow or browser-based login unless explicitly requested.
- Preserve local-only token resolution behavior.

### Codex

- Source: local session files under:
  - `~/.codex/sessions`
  - `~/.codex/archived_sessions`
- Prefer parsing the latest valid local session data.
- Do not add destructive cleanup of user session files.

## Engineering Guidelines

- Keep the app dependency-light.
- Prefer standard library code unless a new dependency is clearly justified.
- Preserve the current single-binary, single-page architecture.
- Keep the HTML Kindle-friendly:
  - high contrast
  - simple layout
  - no JS dependency unless explicitly required
- Favor explicit parsing over overly clever generic helpers.
- If a provider response is partially missing, fail soft and keep the dashboard rendering.

## Configuration Rules

- `config.yaml` must remain safe to commit.
- Never commit local tokens, cookies, or machine-specific secrets.
- New config fields should have sane local defaults.
- If a feature is local-only and auto-discoverable, prefer auto-discovery over more config.

## README Rules

- Keep `README.MD` accurate for public GitHub use.
- Document only the current supported providers.
- Do not document private credentials or personal file contents.
- If setup changes, update the README in the same change.

## Local Development

- Use `make` for the normal local run loop.
- `make` should keep rebuilding and starting the local server.
- Keep generated binaries out of git.

## Safe Change Defaults

- Prefer additive, minimal changes.
- Do not reintroduce authentication unless explicitly requested.
- Do not add analytics, telemetry, or remote storage by default.
- Do not assume internet access is available for Codex usage.
- Avoid breaking the current `config.yaml` shape unless the change is necessary.

## Before Finishing

- Run `gofmt -w main.go` if Go code changed.
- Run `go build ./...` after code changes.
- If server behavior changed, verify by fetching `http://127.0.0.1:8080/` locally when feasible.
