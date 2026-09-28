# Contributing to ccleft

ccleft is a Go library and CLI tool for querying AI coding-agent quota remaining across providers (Claude, OpenAI, Google, etc.). It powers Hive's fleet scheduling: before starting work, the scheduler checks headroom and decides whether to wait, scale down, or proceed.

The library reads from each provider's credential store and quota API; the CLI and Prometheus exporter expose that data for fleet dashboards and automation.

See [tuna-os/.github/CODE_OF_CONDUCT.md](https://github.com/tuna-os/.github/blob/main/CODE_OF_CONDUCT.md) for community guidelines.

## Set Up

You need Go 1.22 or later:

```bash
git clone https://github.com/tuna-os/ccleft
cd ccleft
go mod download
```

## Build and Test

```bash
# Build CLI
go build -o bin/ccleft ./cmd/ccleft

# Build Prometheus exporter
go build -o bin/ccleft-prom ./cmd/ccleft-prom

# Run test suite (uses httptest fixtures)
go test ./...

# Test with verbose output
go test -v ./...

# Format and vet
go fmt ./...
go vet ./...
```

Tests use httptest fixtures for each provider (e.g., payload shapes, 429 responses, token expiry). Live verification against production APIs is documented in the README per provider.

## Architecture

ccleft is layered:

1. **Providers** — each provider (claude, codex, kiro, etc.) implements quota reading from its API
2. **Credentials** — credential paths are standardized per provider (e.g., `~/.claude/.credentials.json` for Claude)
3. **Quota windows** — time-windowed or balance-based quotas (e.g., `five_hour`, `seven_day` for Claude; `plan`, `bonus` for Copilot)
4. **States** — normalized state across providers: `ok`, `limited`, `rate_limited`, `auth_required`, `error`, `unsupported`
5. **CLI** — `ccleft probe` queries all homes and outputs a table
6. **Exporter** — Prometheus metrics for each window/state

## Adding a Provider

To add a new provider:

1. **Define credentials path** — where ccleft will read the token/key (e.g., `~/.newprov/api.json`)
2. **Implement the API call** — fetch quota from the provider's endpoint
3. **Map windows** — define time windows or balance fields (e.g., `monthly`, `daily`)
4. **Map states** — return `ok`, `limited`, `rate_limited`, `auth_required`, or `error` based on the API response
5. **Add httptest fixtures** — mock API responses (success, rate limit, expired token, etc.)
6. **Write tests** — verify credential lookup, API call, state transitions, error cases
7. **Live verify** — document testing against real production quota

See the claude or codex provider implementations for detailed examples.

## Code Conventions

- Follow Go style via `go fmt` and `go vet`
- Error handling is critical: distinguish between auth failures, rate limits, and transient errors
- All state transitions must be documented: when does a provider report `limited`? What makes it `rate_limited`?
- Credential paths should never execute untrusted code (e.g., never run a shell command from a credential store)
- Homebase discovery via `--home` flags or `CCLEFT_HOMES` env var — support multiple agent homes

## Pull Requests

1. Open or find an issue for your work
2. Branch from `main`: `git checkout -b feature/your-feature`
3. For new providers, add the issue first so API shape can be discussed
4. Write commit messages: `feat: …`, `fix: …`, `refactor: …`, `docs: …`
5. Before pushing:
   ```bash
   go fmt ./...
   go vet ./...
   go test ./...
   ```
6. Push and open a PR against `main`

For complex provider integrations, include a summary of the API shape and quota windows.

## Live Verification

Providers in the README table are marked as `live-verified` with dates. When adding a provider, note:
- Which API endpoints were tested
- Which credential paths work
- Which windows were verified
- Any provider-specific quirks (e.g., rate limits without `Retry-After`, API shutdown)

---

By contributing, you agree your contributions are licensed under the project license.
