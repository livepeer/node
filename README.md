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
  discovery, capacity-limited persistent and transient single-shot sessions,
  callbacks, automatic and explicitly generated proxy URLs, generic trickle channels, and HTTP/SSE/
  WebSocket reverse proxying. With payment configuration it issues on-chain
  challenges, validates `live` and `fixed` ticket batches, charges sessions,
  and queues winning tickets for direct Ethereum redemption.
- `livepeer-signer` serves the retained unversioned signing, payment generation
  and discovery routes, with optional authorization webhooks. Payment state is
  client-held and signed; replicas sharing a key and webhook configuration are
  interchangeable.
- `livepeer-chain` implements all 17 approved direct Ethereum commands.
  State-changing commands simulate and estimate gas first, and require
  `--submit` to broadcast. `--wait` waits for inclusion in a successful receipt.
- Outbound runner, generated proxy, static health, and chain RPC destinations
  have separate exact host:port grants. Private and special-use addresses are
  denied by default at dial time.
- Ethereum contract calls, transaction signing and account access stay in
  `eth`; ticket, sender, recipient and redemption code stays in `pm`.
- All executables expose version, help, and shell completion. Boa loads strict
  TOML, component-prefixed environment variables, and exact-byte secret files.

Relevant trickle and ticket authorship is preserved in filtered Git history;
the original `go-livepeer/trickle` implementation and tests are also present
in `trickle/`. See [`docs/history-extraction.md`](docs/history-extraction.md). Production
chain cutover still needs deployment-specific staging validation, including
contract addresses, funded accounts, gas behavior and a rollback drill; see
[`docs/cutover.md`](docs/cutover.md).

The trickle server retains five segments per channel and rejects a segment
above 10 MB by default. There is no global byte cap; size channel capacity
and memory accordingly.

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

The registry accepts up to 256 runners and 10,000 sessions, with 25 generated
proxies per session. Control requests have 64 admission slots and a five-second
body deadline; streams have 256 separate slots. Trickle keeps 64 segments per
channel, an 8 MiB rolling window per segment, and 64 MiB total buffered data
across at most 1,024 channels (256 reserved for runner controls). Segments can
stream beyond the window; lagging readers receive 470 before response headers,
or a terminated response after streaming begins. An empty long poll times out
after 25 seconds; an active stream can continue until session termination.

A session owns its price, cancellation and resources. Client/runner stop,
expiry, failed health, unregister, exhausted credit and shutdown cancel active
HTTP, SSE and WebSocket proxy traffic and remove its channels/proxies. This
also applies to transient single-shot sessions. A runner stopping its own
in-flight request should expect that request to end abruptly.

`proxy = true` creates an automatic proxy. `proxy_url_template` accepts
`https://{proxy}.apps.example` or `https://apps.example/proxy/{proxy}`; DNS,
TLS and forwarding to this listener must be configured by the operator.
Without a template, generated URLs use `service_url/run/{proxy}`. The prior
`/proxy/{proxy}` path remains accepted as an alias for active proxies.
Hostname matching uses the actual Host header. Single-shot domain proxies
require a lowercase DNS-label runner ID of at most 63 characters. Normal
application/control routes also support a base path in `service_url`. Set
`runner_service_url` when runners must reach the listener through a different
address or path. It is advertised in the heartbeat, O2R channel, session
control header and trickle `internal_url`; client URLs stay on `service_url`.

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
attempts. The signer key is the ticket sender. Configure signer RPC, chain ID
and controller together: paid generation
requires a current coherent sender-funds observation. Without RPC, signing
identity and discovery remain available, but paid generation returns 482.
The orchestrator SQLite file must be owner-only.

Configure exactly one orchestrator conversion source: a fixed `wei_per_usd`,
or an explicit Chainlink-compatible `eth_usd_feed` contract on the payment
chain. The feed is checked every 30 seconds, with `price_max_age` defaulting
to two hours. Startup requires a valid observation; stale rates suspend new
priced discovery/reservations. Existing sessions retain their agreed wei
price. USD/hour discovery is normalized to USD/second alongside wei/second.
Off-chain operation accepts quoted runner prices but advertises free service.

Paid signer configuration requires positive `max_live_price_usd_per_second` and
`max_fixed_price_usd` and exactly one `wei_per_usd` or `eth_usd_feed`. Feed
observations refresh every 30 seconds and expire after `price_max_age` (two
hours by default). Rates and ceilings are compared exactly. An unavailable
fresh rate returns 503 and makes the signer unready; a price above a ceiling
returns 481. Request and webhook wei ceilings apply as additional limits.

The signer limits per-ticket EV, batch EV and face value relative to deposit;
see `max_ticket_ev`, `max_batch_ev`, and `deposit_multiplier` in its example.
It issues enough tickets to reach at least the greater of the fee and one
ticket's expected value, then carries unused expected value in signed state.
The state is not a replay database: reusing an earlier signed state can yield
another batch, and retries need not return identical bytes. The recipient
rejects duplicate tickets.
The recipient checks claimable reserve, withdrawal timing and outstanding
winning liability across sessions. Ethereum reads use canonical block-hash
snapshots with Arbitrum's L1 clock and the last initialized round. Refresh
returns 480 before parameter/auth expiry and resets the nonce on new randomness.

Optional signer authorization uses `auth_webhook_file`, purpose-scoped grants
and CA, and a file-backed JSON map in `auth_webhook_headers_file`. The webhook
receives request headers and the proposed updated payment state after ticket
calculation, and returns `status`,
`reason`, Unix-second `expiry`, `auth_id`, and optional `maxPrice` (wei per
second or fixed request). Successful authorization is cached in signed state
until expiry; identity and price ceilings remain enforced. Changing the
webhook URL or configured headers invalidates this cache. Redirects are refused.

Winning tickets wait for parameter expiry before their randomness is exposed
on-chain. Signed transaction bytes and hash are saved before broadcast; safe
preparation failures retry, uncertain writes require reconciliation or an
explicit identical-byte retry. Only finalized, canonical receipts settle
liability. See [payment recovery](docs/payment-recovery.md) for commands and
upgrade behavior. Back up the orchestrator database before upgrading. Legacy
signed client state is not a migration format for this signer's signed state.

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
`private_key_file` to broadcast; add `--wait` for a successful receipt.
The submission hash is printed before waiting and remains available on errors.
With JSON output, submission and receipt are separate JSON-line records.
An approval needed by `stake bond` is submitted first; without `--wait`, rerun
the bond after that approval is confirmed. `stake rebond --delegate ADDRESS`
uses `rebondFromUnbonded`; omit the delegate for an already bonded account.
Before `orchestrator activate`, self-bond using `stake bond --delegate YOUR_ADDRESS`
and set the public URI with `orchestrator set-config --service-uri URL`. The sender
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
Payment tests cover Python `live` and `fixed` calls, sustained refresh across
nonce/L1/auth boundaries, GPU-filtered discovery, a Go paid session, concurrent
liability, and crash/reorg/finality scenarios against deterministic Ethereum
interfaces. Both SDKs exercise persistent/single-shot and proxy modes.
The HTTP matrix additionally covers static registration and domain templates.
Set `PYTHON_RUNNER_USE_WORKING_TREE=1` to test the local SDK edits explicitly.
CLI help and completion snapshots can be deliberately refreshed with
`UPDATE_CLI_GOLDENS=1 go test ./cmd/livepeer -run TestRealBinaryOffchainFlow`. The Protobuf
fixture is generated by the pinned Python runner. An external EVM staging
test remains necessary before production cutover.
