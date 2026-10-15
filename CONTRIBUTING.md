# Contributing to ccleft

Thanks for looking at ccleft. This document covers the local dev loop; see
[README.md](README.md) for what the project does and how it's used.

## Prerequisites

- Go 1.24 or newer (see `go.mod`)
- Docker (only needed if you're changing the `Dockerfile` or want to run the
  `docker` CI job locally)
- A C compiler (e.g. `gcc`) if you want to run the test suite with `-race`;
  plain `go test` does not need one

## Building

```
go build ./...
```

## Before opening a PR

Run the same checks CI runs, in this order:

```
gofmt -l .                        # must print nothing
go vet ./...
go test -race -count=1 ./...      # drop -race if you don't have a C compiler available
CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/ccleft
```

If `gofmt -l .` prints any file paths, run `gofmt -w .` to fix formatting
before committing.

## Tests and fixtures

Provider behavior is tested against recorded HTTP fixtures under
[`testdata/`](testdata/README.md) rather than live upstream calls — see that
file for what each fixture captures and how it was produced. When adding or
changing a provider, prefer adding a fixture over hitting the real API in a
test.

`live_test.go` contains tests pinned to real payloads captured live from
provider endpoints (see `testdata/README.md` for provenance); they run
against those recorded fixtures like every other test, not against the live
network, so no credentials are needed to run them.

## Security-sensitive changes

ccleft is read-only by design (see the README's "Security" section): it must
never write, refresh, or chmod a credential file, and it must never shell out
to a CLI that could mutate one (`codex`, `claude`). If your change touches
credential discovery or the `agy` exec path, call this out explicitly in the
PR description.

## Docker

To build the image locally the way CI's `docker` job does:

```
docker build -t ccleft:local .
```

## License

By contributing, you agree your contribution is licensed under Apache-2.0
(see [LICENSE](LICENSE)). See [NOTICE](NOTICE) for origin and reference
attributions this project already carries.
