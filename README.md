# Livepeer standalone extraction

This repository is the standalone Live Runner extraction from
`go-livepeer`. The source baseline and scope are recorded in
[`docs/source-revision.md`](docs/source-revision.md) and
[`docs/scope-matrix.md`](docs/scope-matrix.md).

## Current implementation

- `livepeer` dispatches only to bundled sibling or `libexec` executables and
  forwards their process status, streams and signals.
- `livepeer-orchestrator` provides Live Runner HTTP routes:
  dynamic registration and authenticated heartbeats, static runners,
  discovery, capacity-limited persistent sessions, single-shot sessions,
  callbacks, generated proxy URLs, generic trickle channels, and HTTP/SSE/
  WebSocket reverse proxying. With payment configuration it issues on-chain
  challenges, validates `live` and `fixed` ticket batches, charges sessions,
  and queues winning tickets for direct Ethereum redemption.
- `livepeer-signer` serves the retained unversioned signing, payment generation
  and discovery routes. SQLite pins signed-state sequence and replay results.
- `livepeer-chain` implements all 17 approved direct Ethereum commands.
  State-changing commands simulate and estimate gas first, and require
  `--submit` to broadcast. `--wait` waits for a receipt.
- Outbound runner, generated proxy, static health, and chain RPC destinations
  have separate exact host:port grants. Private and special-use addresses are
  denied by default at dial time.
- Ethereum contract calls, transaction signing and account access stay in
  `eth`; ticket, sender, recipient and redemption code stays in `pm`.
- All executables expose version, help, and shell completion. Boa loads strict
  TOML, component-prefixed environment variables, and exact-byte secret files.

Relevant trickle and ticket authorship is preserved in filtered Git history;
see [`docs/history-extraction.md`](docs/history-extraction.md). Production
chain cutover still needs deployment-specific staging validation, including
contract addresses, funded accounts, gas behavior and a rollback drill; see
[`docs/cutover.md`](docs/cutover.md).

## Build and run

Go 1.27.1 or newer is required by the pinned Boa fork.

```sh
make build
bin/livepeer version
bin/livepeer orchestrator --help
bin/livepeer completion orchestrator bash
```

Copy the component examples in `configs/`, set their paths, and start:

```sh
bin/livepeer orchestrator --config /absolute/path/to/orchestrator.toml
bin/livepeer signer --config /absolute/path/to/signer.toml
bin/livepeer chain status --config /absolute/path/to/chain.toml
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
session proxies, static health checks, signer discovery and chain RPC. Each bundle extends the
system trust roots for only that destination purpose; certificate verification
remains enabled.

## Paid operation

The orchestrator payment key is the ticket recipient. Its SQLite file holds
challenges, balances, nonce replay protection, winning tickets and redemption
attempts. The signer key is the ticket sender and has its own SQLite state
file. Configure signer RPC, chain ID and controller together to check sender
deposit and reserve before issuing tickets. Both SQLite files must be
owner-only. Set the orchestrator's
`wei_per_usd` from an operator-managed rate source; the process uses that
static rate when a runner registers. Runner prices use USD per hour (`hour`)
or per request (`fixed`).

Winning tickets are claimed in SQLite before broadcasting once. The background
worker checks submitted transaction receipts and records confirmation or
revert. An uncertain RPC result is recorded for operator reconciliation,
without implicit retry.
Back up the component SQLite files before migration. The database tables are
created on first start; this initial version has no cross-version migration
tool.

## Chain management

The approved commands are:

```text
status                         account
orchestrator get               orchestrator activate
orchestrator set-config        orchestrator reward
stake bond                     stake unbond
stake rebond                   stake withdraw
earnings claim                 earnings withdraw-fees
ticketbroker fund              ticketbroker unlock
ticketbroker cancel-unlock     ticketbroker withdraw
round initialize
```

Run a state change without `--submit` to review its simulation,
gas estimate and call data. Add `--submit` and an owner-only
`private_key_file` to broadcast; add `--wait` for confirmation. The sender
must match the key and the configured chain ID must match RPC. No command
starts an HTTP server. Exit status is 0 on success and 2 on command failure.

TODO: add a richer terminal UI after the direct command surface is stable.

## Test

```sh
go test -race ./...
go vet ./...
```

The architecture test checks source-level import boundaries. Real-process and
HTTP integration tests exercise pinned Go and Python SDK revisions when their
checkouts and dependencies are available; CI checks out both revisions.
Payment tests cover Python `live` and `fixed` calls, GPU-filtered discovery,
a Go paid session, and ticket receipt against a deterministic Ethereum interface. The Protobuf
fixture is generated by the pinned Python runner. An external EVM staging
test remains necessary before production cutover.
