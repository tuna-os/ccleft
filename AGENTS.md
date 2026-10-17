# Automation guide

This file applies to the entire repository. Human contributors should also read
[CONTRIBUTING.md](CONTRIBUTING.md).

## Repository map

- Root Go package: public `ccleft` library and provider implementations.
- `cmd/ccleft/`: CLI, configuration, and Prometheus HTTP server.
- `testdata/`: synthetic or redacted provider payloads and their provenance.
- `.github/workflows/`: CI and container publishing.

ccleft is maintained directly in this repository, not as a fork. `NOTICE`
records code origins and references; there is no upstream-sync workflow.

## Working rules

- Keep changes focused on the issue being solved. Reuse existing provider,
  parser, fixture, and CLI patterns.
- Preserve the library-first design. Provider logic belongs in the root package;
  CLI-only concerns belong under `cmd/ccleft/`.
- Keep credential access read-only. Never refresh, rewrite, create, or change
  permissions on credential files. Never invoke `codex` or `claude` for token
  discovery.
- Derive credential paths from `Source.Home` or explicit source environment
  overrides, never the automation process's own home directory.
- Never commit real credentials, account identifiers, e-mail addresses, or
  unredacted captures. Document fixture provenance in `testdata/README.md`.
- Treat unknown payloads as measurement errors, not healthy quota. Preserve
  useful upstream error context after redacting secrets.
- Pin every third-party GitHub Action to a full commit SHA with a release comment.
  Let Renovate update pinned digests.

## Provider changes

A provider implementation separates local `identify` work from bounded `fetch`
work and registers itself with the root package. Update timeouts, detection,
normalized windows, state mapping, fixtures, tests, and README provider tables
as applicable. Tests use `httptest` and local fixtures; they must not contact
live provider APIs.

State meanings are strict:

- `auth_required`: credentials are missing, expired, or rejected.
- `rate_limited`: the quota endpoint throttled the request.
- `unsupported`: no usable quota source exists for that provider or login.
- `error`: network, timeout, schema, or other measurement failure.

## Required checks

Run from repository root:

```
gofmt -l .
go vet ./...
go test -race -count=1 ./...
CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/ccleft
```

`gofmt -l .` must print nothing. A missing C compiler may justify dropping
`-race` locally, but CI runs it. Docker changes also require the image build
shown in `CONTRIBUTING.md`.

## Pull requests

Branch from `main`. Use Conventional Commit subjects and DCO-sign every commit
with `git commit -s`. Update documentation when public behavior or provider
support changes. A TunaOS maintainer merges only after required CI passes and
review feedback is resolved.
