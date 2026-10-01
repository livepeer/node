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
  and discovery routes, with required on-chain configuration and optional
  authorization webhooks. Payment state is
  client-held and signed; replicas sharing a key and webhook configuration are
  interchangeable.
- `livepeer-chain` implements all 17 approved direct Ethereum commands.
  State-changing commands simulate and estimate gas first, and require
  `--submit` to broadcast. `--wait` waits for inclusion in a successful receipt.
- Outbound runner, generated proxy, and static health destinations have
  separate exact host:port grants. Private and special-use addresses are denied
  by default at dial time for these clients. Ethereum RPC, discovery, and
  webhooks use operator-configured endpoints without destination grants.
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

The orchestrator and chain commands support `--print-config` to inspect an
audited TOML view. It omits direct secrets, secret file paths and the static
runner file path. Boa reads secret files as
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
session proxies, and static health checks. Each bundle extends the system trust
roots for that destination purpose; certificate verification remains enabled.
Ethereum RPC clients use system trust roots without custom CA flags.

## Paid operation

The orchestrator payment key is the ticket recipient. PM parameter generation
and authentication retain go-livepeer's HMAC-SHA256 derivation, with a fresh
256-bit cryptographic secret per process. A separate in-memory HMAC key signs
session auth tokens. Challenges, balances and nonce replay guards stay in
memory; SQLite holds winning tickets (including their redemption randomness),
redemption attempts and chain activity observations. Restarting requires new
paid sessions while stored winners remain redeemable. The signer key is the
ticket sender. Paid generation requires a
current coherent sender-funds observation. The signer requires
on-chain configuration at startup, including when used for identity signing
or discovery. `ChainID` optionally asserts the RPC's chain ID; when omitted,
the signer uses the RPC's chain. Controller and ETH/USD feed defaults match
go-livepeer's Arbitrum mainnet addresses: respectively
`0xD8E8328501E9645d16Cf49539efC04f734606ee4` and
`0x639Fe6ab55C921f74e7fac1ee960C0B6293ba612`. Override both when using another
chain. Its TOML keys use implicit Go field names, as shown in
[`configs/signer/config.example.toml`](configs/signer/config.example.toml).
The orchestrator SQLite file must be owner-only.

Configure exactly one orchestrator conversion source: a fixed `wei_per_usd`,
or an explicit Chainlink-compatible `eth_usd_feed` contract on the payment
chain. The feed is checked every 30 seconds, with `price_max_age` defaulting
to two hours. Startup requires a valid observation; stale rates suspend new
priced discovery/reservations. Existing sessions retain their agreed wei
price. USD/hour discovery is normalized to USD/second alongside wei/second.
Off-chain operation accepts quoted runner prices but advertises free service.

Signer configuration requires `KeyFile`, an RPC URL through `RPCURLFile` or
`LIVEPEER_SIGNER_RPC_URL`, and positive `MaxHourlyPrice` (USD/hour) and
`MaxFixedPrice` (USD/request), with USD implicit. Live ceilings are divided by
3600 before comparison with wei/second quotes. `ETHUSDFeed` supplies the rate
unless `WeiPerUSD` overrides it with a fixed conversion, mostly for testing;
the fixed rate never refreshes or expires. Feed observations refresh every
30 seconds and expire after `ETHUSDMaxAge` (two hours by default). Rates and
ceilings are compared exactly.

If the feed is unavailable at startup, the signer starts unready and retries.
A failed refresh preserves the last observation only until its expiry. Once
that observation is stale, `/readyz` and payment generation return HTTP 503;
identity signing and discovery remain available. A fresh successful observation
restores rate availability. A price above a ceiling
returns 481. Request and webhook wei ceilings apply as additional limits.
`/readyz` also makes a bounded canonical chain/funds read: RPC or contract
failures, an empty deposit or total reserve, and an imminent withdrawal return
503. Readiness recovers on the next successful probe. Recipient-specific
claimable reserve is still checked when paying that recipient. `/healthz`
reports process liveness independently of payment readiness.

The signer limits per-ticket EV, batch EV and face value relative to deposit;
see `MaxTicketEV`, `MaxBatchEV`, and `DepositMultiplier` in its example.
`DepositMultiplier` must be at least one; explicitly setting zero is rejected.
It issues enough tickets to reach at least the greater of the fee and one
ticket's expected value, then carries unused expected value in signed state.
The state is not a replay database: reusing an earlier signed state can yield
another batch, and retries need not return identical bytes. The recipient
rejects duplicate tickets.
The recipient checks claimable reserve, withdrawal timing and outstanding
winning liability across sessions. Ethereum reads use canonical block-hash
snapshots with Arbitrum's L1 clock and the last initialized round. Ticket
recipients must match the OrchestratorInfo address. Parameters must use the
last initialized round and its canonical hash; older rounds, mismatched hashes,
and expiring parameters/auth return 480 for refresh. New randomness resets the
nonce. Parameter expiry is supplied by the caller, not signed by the orchestrator;
these checks do not establish the provenance of an arbitrary challenge.

Optional signer authorization uses `AuthWebhookFile` and
`AuthWebhookHeadersFile`. Headers use go-livepeer's comma-separated
`Header: value` syntax, with CSV quoting for entries containing commas.
Names are case-insensitive, repeated names retain multiple values, and malformed
entries fail configuration parsing. URL and header credentials can also come
from their component-prefixed environment variables. The webhook receives
request headers and the proposed updated payment state. We deliberately generate
and sign the tickets **before authorization so we know what is being authorized**.
Payment, credentials and updated signed state are released only after approval.
The webhook returns `status`, `reason`, Unix-second `expiry`, `auth_id`, and optional
`maxPrice` (wei per second or fixed request). **Signed state is a bearer credential**:
while authorization is cached, possession of it permits continuation without
presenting the original credentials. Keep it out of logs and shared storage.
Caching is generally not recommended outside specific, deliberately scoped
trusted-client cases. Omit `expiry` or return zero to authorize each payment;
a future expiry caches approval and delays credential revocation and budget
rechecks until that time. A supplied `Signer-Auth-Id` must match the state, but
omitting it does not authenticate a caller. If a proxy supplies identity, it
must authenticate each request and overwrite caller-supplied identity headers.
Price ceilings continue to apply during caching. Changing the webhook URL or
configured headers invalidates the cache. Redirects
are refused. The signer has no shared bearer-token or TLS-assertion flags;
listener access and TLS termination are configured externally. Response writes
are bounded by the request deadline (at most 30 seconds), and cancellation
interrupts blocked writes. None of the signer routes stream beyond the request.

Run the signer alongside a clearinghouse such as
[livepeer/clearinghouse-batteries](https://github.com/livepeer/clearinghouse-batteries)
to manage authorization, allocations and cumulative spending. Price and ticket
limits constrain individual quotes/batches, not a customer's total budget.
That clearinghouse's documented accounting pipeline consumes signing usage
events through Kafka: this signer supports its webhook shape but currently has
no Kafka usage-event producer. Connect and verify the accounting path before
relying on end-to-end budget enforcement; configuring only the webhook is not
the complete integration. An alternative is a synchronous authorization service
with a durable ledger that atomically reserves each payment against a budget,
deduplicates retries, and shares accounting across replicas. This still requires
state somewhere; client-carried signed state alone cannot enforce cumulative caps.

Winning tickets wait for parameter expiry before their randomness is exposed
on-chain. Signed transaction bytes and hash are saved before broadcast; safe
preparation failures retry, uncertain writes require reconciliation or an
explicit identical-byte retry. Only finalized, canonical receipts settle
liability. See [payment recovery](docs/payment-recovery.md) for commands and
restart behavior. Back up the orchestrator database. Legacy
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
`KeyFile` to broadcast; add `--wait` for a successful receipt.
The submission hash is printed before waiting and remains available on errors.
With JSON output, submission and receipt are separate JSON-line records.
Add `--quiet` to suppress normal transaction output, including dry-run plans,
submission hashes and receipt records. Errors still go to stderr and return
exit status 2; broadcast and receipt errors retain the transaction hash.
Transaction switches and action inputs are accepted only on the command line.
TOML contains shared operator settings: RPC connection, expected chain ID,
Controller, sender, key location, and an optional maximum fee per gas.
`--output` and `--print-config` are also
command-line controls. Shared flags can appear before or after chain subcommands:

```sh
livepeer chain --config /path/to/chain.toml --output json stake bond ORCHESTRATOR_ADDRESS --amount 1000000000000000000
livepeer chain ticketbroker fund --config /path/to/chain.toml --amount 1000000000000000000 --reserve 0 --submit --quiet
```

An approval needed by `stake bond` is submitted first; without `--wait`, rerun
the bond after that approval is confirmed. `stake rebond --delegate ADDRESS`
uses `rebondFromUnbonded`; omit the delegate for an already bonded account.
Before `orchestrator activate`, self-bond using `stake bond YOUR_ADDRESS --amount AMOUNT`
and set the public URI with `orchestrator set-config --service-uri URL`. The sender
must match the key and the configured chain ID must match RPC. No command
starts an HTTP server. Exit status is 0 on success and 2 on command failure.

Transactions require dynamic fees and fail if the RPC header has no base fee.
`--max-fee-per-gas` (TOML `MaxFeePerGas`) sets an optional
ceiling in wei per gas; a higher estimate fails before signing. Staking and
reward commands calculate pool-position hints. `stake bond` still requires a
positive new-token amount. The `eth` APIs also support explicit delegation
changes, voting, and delegated reward callers; their CLI commands follow later.

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
