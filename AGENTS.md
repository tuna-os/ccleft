# AGENTS.md — agent guide for tuna-os/ccleft

**ccleft** queries AI provider quota endpoints and reports remaining tokens/credits. A library-first design consumed by Hive's rotation scheduler and exposed via CLI and Prometheus metrics.

Human docs: [`README.md`](README.md) (comprehensive overview, provider matrix, usage patterns),
[`CONTRIBUTING.md`](CONTRIBUTING.md) (development setup and PR process).

## Build and Test

```bash
go build ./...                    # build library and CLI
go test ./...                     # run all tests
go run ./cmd/ccleft probe --home $HOME         # run probe command
go run ./cmd/ccleft serve --addr :9464        # run exporter/API
```

## Key Facts

- **Language**: Go 1.21+
- **Design**: Library-first; CLI and exporter are wrappers
- **Provider matrix**: 8 providers (claude, codex, agy, gemini, kiro, copilot, deepseek, muse)
- **Implementation status**: All documented in README provider matrix table
- **Live verification**: Claude, codex, agy, kiro, copilot, deepseek verified 2026-09-24/25
- **Architecture**: Providers → Client (dedup, rate limit, backoff, cache) → CLI/Exporter

## Repository Structure

- **cmd/ccleft/**: CLI (probe, serve) and main entrypoint
- **claude.go, codex.go, agy.go, etc.**: Provider implementations
- **client.go**: Client with deduplication, rate limiting, single-flight, backoff
- **detect.go**: Credential discovery from home and environment
- **client_test.go**: Client and integration tests
- **Dockerfile**: Distroless-static image (no agy CLI)
- **ccleft.example.yaml**: Configuration template

## Provider Implementation

Each provider implements `Prober` interface:
- Credential discovery from `Home` or `Env`
- Account fingerprinting (SHA-256[:16], non-secret, stable)
- Quota probing via provider's official API or CLI
- State machine: ok | limited | exhausted | rate_limited | auth_required | unsupported | error
- Window/binding detection (binding windows decide account state)

Provider matrix in README lists:
- Source (API endpoint or CLI)
- Credentials (paths or env vars)
- Windows (quota buckets, bindings)
- States
- Fixtures and live-verification status

## Client Behavior

- **Per-account deduplication**: Multiple homes sharing one account = one upstream call
- **Single-flight**: Concurrent Gets for same account share one request
- **Per-account rate limit**: Configurable per provider (claude 3m, agy 2m, others 1m); gate opens 5% early
- **Backoff**: 30s→30m doubling on 429/timeout/5xx; `Retry-After` honored
- **Last-good caching**: Stale readings served during failures with `cause` and `retry_at`
- **Definitive answers**: 401, unsupported, schema errors returned as-is (not retried)

## Metrics

Key series (full list in README):
- `ccleft_remaining_ratio{provider,account,window,binding}`: 0..1 gauge
- `ccleft_state{provider,account,state}`: one-hot
- `ccleft_stale`, `ccleft_upstream_requests_total{cause}`: observability
- `ccleft_reset_timestamp_seconds`: next reset time

Example alerts:
```promql
min by (provider, account) (ccleft_remaining_ratio{binding="true"}) < 0.1
ccleft_upstream_requests_total{state="rate_limited"} > 0
```

## Fleet Deployment

Run one `ccleft serve` per node/pod (share one process per account pool to dedupe upstream calls):

```yaml
- name: ccleft
  image: ghcr.io/tuna-os/ccleft:latest
  args: [serve, --config, /etc/ccleft/ccleft.yaml]
  ports: [{containerPort: 9464}]
  volumeMounts:
    - {name: data, mountPath: /data/home, readOnly: true}
  env:
    - name: KIRO_API_KEY
      valueFrom: {secretKeyRef: {name: kiro, key: api-key}}
```

agy note: Default image has no agy CLI. Either:
1. Derive image with agy binary and set `agy_path` in config
2. Run ccleft inside agent container that has agy (read-only HOME)

## Hive Integration

Hive's rotation scheduler queries ccleft for headroom:
- `Headroom.Available` ← `state == ok`
- `Limits[]` ← binding windows (use `scope` for model-scoped quotas)
- `pct_remaining` ← `remaining_pct`
- Stale readings respected; unknown/failed measurements hold

## DCO and Attribution

All commits must be signed with DCO: `git commit -s`. No special PR attribution needed beyond standard Git author.
