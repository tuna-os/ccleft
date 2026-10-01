# Contributing to ccleft

Thanks for your interest in contributing to ccleft! This guide covers setting up your development environment, building and testing, and submitting changes.

## Getting Started

### Prerequisites

ccleft is a Go library and CLI with a Prometheus exporter. You'll need:

- **Go**: Version 1.21 or later. Install from [golang.org](https://golang.org/doc/install).
- **System dependencies**: Depending on your platform:
  - **Linux (Debian/Ubuntu)**: `sudo apt install build-essential pkg-config`
  - **macOS**: Xcode Command Line Tools (`xcode-select --install`)
  - **Windows**: MinGW or equivalent for CGO (optional; ccleft can build without CGO)
- **Docker**: Optional, for building the container image.
- **agy CLI**: Optional, for testing the agy provider locally.

### Clone and Build

```bash
git clone https://github.com/tuna-os/ccleft.git
cd ccleft
go build ./...
go test ./...
```

## Development Workflow

### Building

Build the library and all CLI commands:

```bash
go build ./...
```

Build just the CLI binary:

```bash
go build -o ./bin/ccleft ./cmd/ccleft
```

### Running Locally

Run the probe command to test provider detection and quota fetching:

```bash
./bin/ccleft probe --home $HOME
./bin/ccleft probe --json --home $HOME --provider claude
```

Run the serve command (Prometheus exporter and API):

```bash
./bin/ccleft serve --addr :9464 --home $HOME
```

Then access metrics at `http://localhost:9464/metrics` and readings at `http://localhost:9464/readings`.

### Testing

Run all tests:

```bash
go test ./...
```

Run tests for a specific provider:

```bash
go test ./... -run TestClaude
```

Run with verbose output:

```bash
go test -v ./...
```

### Testing Providers

Each provider has httptest fixtures (mocked responses) and can be live-verified:

- **claude**: Requires `~/.claude/.credentials.json` with OAuth token
- **codex**: Requires `~/.codex/auth.json` with ChatGPT tokens
- **agy**: Requires `agy` CLI installed and `~/.gemini/antigravity-cli/antigravity-oauth-token`
- **gemini**: Reports static (unsupported — Google shut down the API)
- **kiro**: Requires `KIRO_API_KEY` environment variable
- **copilot**: Requires GitHub token via gh CLI or `COPILOT_GITHUB_TOKEN`
- **deepseek**: Requires `DEEPSEEK_API_KEY` environment variable

Fixtures are snapshot-tested against real provider responses. Before modifying a provider, update fixtures by running tests with actual credentials and committing the new snapshots.

### Linting and Formatting

Format code:

```bash
go fmt ./...
```

Run the linter:

```bash
go vet ./...
go run github.com/golangci/golangci-lint/cmd/golangci-lint@latest run
```

All code must pass `vet` and `fmt` before submitting a PR.

### Docker Image

Build the container image:

```bash
docker build -t ghcr.io/tuna-os/ccleft:local .
```

Run it locally:

```bash
docker run --rm -v $HOME:/home/agent -p 9464:9464 \
  ghcr.io/tuna-os/ccleft:local \
  serve --config /etc/ccleft/ccleft.yaml
```

The image is distroless-static and has no agy CLI. To test agy provider, either:
1. Build a derived image that adds agy
2. Run ccleft inside your agent's container, which already has agy

## Code Standards

- **Go version**: Maintain compatibility with Go 1.21+.
- **Formatting**: All code must pass `go fmt`.
- **Linting**: All code must pass `go vet`.
- **Testing**: Add tests for new providers and features. Fixtures must be updated when provider APIs change.
- **Comments**: Document public functions and types. Use clear, concise language.

## Architecture

ccleft has three main layers:

1. **Providers** (claude.go, codex.go, agy.go, etc.): Implement `Prober` interface
2. **Client** (client.go): Deduplication, rate limiting, caching, backoff, single-flight
3. **CLI and Exporter** (cmd/ccleft): Probe and serve commands, Prometheus metrics

When adding a new provider:
1. Implement the `Prober` interface (Probe method)
2. Handle credential discovery and detection
3. Add httptest fixtures for responses
4. Add tests (at least fixture-based; live verification is optional)
5. Update the provider matrix in README
6. Document the windows and states for that provider

## Submitting Changes

### Before You Push

1. Run `go fmt ./...` to format your code.
2. Run `go vet ./...` to check for errors.
3. Run `go test ./...` to verify tests pass.
4. If you modified a provider, update or add fixtures.
5. Include a clear commit message explaining the "why" behind your change.
6. Sign your commits with DCO: `git commit -s`.

### Creating a Pull Request

1. Push your branch: `git push -u origin guide/your-branch-name`
2. Open a PR on GitHub. Link any related issues.
3. The CI suite will run automatically. If any check fails, review the details and fix the issue.

### PR Guidelines

- **Scope**: Keep PRs focused. One provider or feature per PR when possible.
- **Commits**: Use clear commit messages. If your PR fixes an issue, mention it: `Fixes #123`.
- **Tests**: Add tests for new code. Fixture updates should be in a separate commit.
- **Docs**: Update the provider matrix in README if adding or modifying a provider.
- **Backwards compatibility**: The library API is consumed by Hive; breaking changes need coordination.

## Provider Development Checklist

When adding or modifying a provider:

- [ ] Implement `Prober` interface (Probe method, error handling)
- [ ] Add credential discovery/detection functions
- [ ] Create httptest fixtures (at least success, error, and rate-limit scenarios)
- [ ] Add unit tests using fixtures
- [ ] Live-verify against real provider (if possible)
- [ ] Document windows, states, and account fingerprinting
- [ ] Update provider matrix in README (implementation, fixtures, live status)
- [ ] Add example usage to CLI or library docs
- [ ] Handle provider-specific errors (429, token expiry, schema changes)

## Getting Help

- **Issues**: Use GitHub issues to report bugs or suggest improvements.
- **PR comments**: Ask questions about changes directly on PRs.
- **README**: Review the extensive README for provider details, metrics, and fleet patterns.
- **Hive integration**: See how Hive uses ccleft in `github.com/hivecommons/hive/pkg/rotation`.

## Recognition

All contributors are credited in the commit history. Commits must be signed with DCO (`git commit -s`) per project policy.
