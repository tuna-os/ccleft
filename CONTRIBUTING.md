# Contributing to ccleft

Thanks for your interest in contributing to ccleft. This guide walks you through setting up your development environment, running tests, and submitting changes.

## Getting Started

### Prerequisites

ccleft is written in Go. You'll need:

- **Go 1.24 or later**: Install from [go.dev](https://go.dev/dl/). The project uses Go 1.24 in CI and tests against stable.
- **Docker**: Required if you plan to test the Docker image build locally.

### Clone and Build

```bash
git clone https://github.com/tuna-os/ccleft.git
cd ccleft
go build ./cmd/ccleft
```

The binary is written to `./ccleft` and can be invoked directly:

```bash
./ccleft probe --help
./ccleft serve --help
```

## Development Workflow

### Running Tests

Before committing, run the test suite:

```bash
go test -race -count=1 ./...
```

The `-race` flag enables the race detector, and `-count=1` disables result caching. This is the same check run in CI.

### Code Quality

ccleft uses standard Go tooling:

```bash
# Check formatting
gofmt -l .

# Run the linter
go vet ./...
```

Both checks are enforced in CI. If `gofmt` reports changes, run `gofmt -w .` to fix them.

### Testing New Providers

If you add a new provider or modify provider logic:

1. Add httptest fixtures to `fixtures_test.go` or create a new test file
2. Test both success and failure cases (auth errors, throttling, schema drift)
3. Include at least one real API response snapshot from the provider's documentation or a live probe
4. Run `go test -race -count=1 ./...` to verify your tests pass

See `claude_test.go` and `codex_test.go` for examples of comprehensive provider tests.

### Docker Image

To build the Docker image locally:

```bash
docker build -t ccleft:local .
docker run --rm ccleft:local probe --help
```

The image is published to `ghcr.io/tuna-os/ccleft` on every push to main and on version tags.

## Submitting Changes

### Commit Messages

- Keep the first line concise and descriptive
- Reference the issue number if applicable (`fixes #123`)
- Explain *why* the change is needed, not just what changed
- Sign your commits with `git commit -s` to add the Developer Certificate of Origin

### Pull Request Process

1. Push your branch to your fork or to a feature branch on the main repo
2. Open a pull request against `main`
3. Ensure all CI checks pass (tests, formatting, linting, Docker build)
4. Wait for review — maintainers will provide feedback if needed
5. Never merge your own PR; a maintainer will merge once approved

### Before Opening a PR

- Run `go test -race -count=1 ./...` and verify all tests pass
- Run `gofmt -w .` and `go vet ./...` to fix formatting and linting
- Test the CLI locally: `./ccleft probe --help` and `./ccleft serve --help` should work
- If adding a new provider, test its detection and error handling paths

## Library API Changes

ccleft is primarily a library consumed by [Hive](https://github.com/hivecommons/hive). API changes should:

1. Preserve backward compatibility when possible
2. Document any breaking changes clearly in the PR body
3. Include examples in docstrings for new public functions
4. Update `README.md` and this guide if the public interface changes

## Questions?

Open an issue on GitHub or reach out to the maintainers. We're happy to help.
