# Livepeer standalone extraction

This repository is the **in-progress** standalone Live Runner extraction from
`go-livepeer`. The source baseline and scope are recorded in
[`docs/source-revision.md`](docs/source-revision.md) and
[`docs/scope-matrix.md`](docs/scope-matrix.md).

## Current implementation

- `livepeer` dispatches only to bundled sibling or `libexec` executables and
  forwards their process status, streams and signals.
- `livepeer-orchestrator` provides an off-chain Live Runner HTTP slice:
  dynamic registration and authenticated heartbeats, static runners,
  discovery, capacity-limited persistent sessions, single-shot sessions,
  callbacks, generated proxy URLs, generic trickle channels, and HTTP/SSE/
  WebSocket reverse proxying.
- Outbound runner, generated proxy, static health, and chain RPC destinations
  have separate exact host:port grants. Private and special-use addresses are
  denied by default at dial time.
- `livepeer-chain status` reads and validates the configured chain ID and
  block number through JSON-RPC. `livepeer-chain account` reads the configured
  sender's ETH balance and pending nonce.
- All executables expose version, help, and shell completion. Boa loads strict
  TOML, component-prefixed environment variables, and exact-byte secret files.

The remote signer payment service, on-chain orchestration, ticket redemption,
and the remaining approved chain commands are **not implemented**. The signer
executable exits with an explicit error if asked to start. Relevant trickle
and ticket authorship is preserved in filtered Git history; see
[`docs/history-extraction.md`](docs/history-extraction.md). Do not use this
build as a replacement for an on-chain or paid deployment.

## Build and try the off-chain orchestrator

Go 1.27.1 or newer is required by the pinned Boa fork.

```sh
make build
bin/livepeer version
bin/livepeer orchestrator --help
bin/livepeer completion orchestrator bash
```

Copy `configs/orchestrator/config.example.toml`, set its paths, and start:

```sh
bin/livepeer orchestrator --config /absolute/path/to/orchestrator.toml
```

Use `--print-config` to inspect an audited TOML view. It omits direct secrets,
secret file paths and the static runner file path. Boa reads secret files as
exact bytes, including a trailing newline, so operator-managed files or
secret-manager mounts must contain precisely the intended credential. A direct
environment secret and its file path cannot both be set. Direct secret flags
and TOML values are rejected. The orchestrator requires a bootstrap secret or
a static runner file. Grant a local runner with `runner_grants =
["127.0.0.1:PORT"]`; this does not grant a generated proxy target or a
health-check destination.

The registry accepts up to 256 runners. Trickle storage is bounded to 64
segments per channel, 8 MiB per segment, and 64 MiB total buffered data.

The listener defaults to loopback HTTP. For direct HTTPS, configure
`tls_cert_file` and `tls_key_file` with operator-supplied PEM files and use an
HTTPS `service_url`. To bind plain HTTP beyond loopback, place it behind an
operator-managed TLS terminator and set `behind_tls = true`. The metrics
listener remains loopback and serves only `/metrics`, `/healthz`, and
`/readyz`.

Custom CA bundles can be assigned independently to runner traffic, generated
session proxies, static health checks, and chain RPC. Each bundle extends the
system trust roots for only that destination purpose; certificate verification
remains enabled.

## Current command surface

| Command | State |
| --- | --- |
| `livepeer orchestrator` | Off-chain slice implemented; on-chain payments pending |
| `livepeer signer` | CLI/configuration only; service pending |
| `livepeer chain status` and `account` | Implemented read-only JSON-RPC commands |
| Other approved `livepeer chain ...` commands | Pending |

The 16 chain operations from the extraction plan are approved for the initial
surface. Payment, ticket, signer and chain-watcher state will use SQLite in
each component. The on-chain schema and migration model are still design work.

## Test

```sh
go test -race ./...
go vet ./...
```

The included architecture test checks source-level import boundaries. The
real-process integration test exercises pinned Go and Python SDK revisions
when their checkouts and dependencies are available; CI checks out both
revisions explicitly. Signer fixtures, a test chain, fuzz, load and broader
security suites remain in the migration plan.
