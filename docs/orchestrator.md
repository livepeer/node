# Running an orchestrator

The orchestrator registers HTTP runners, advertises them through discovery,
reserves sessions, and forwards application traffic. It supports persistent
sessions and single-shot requests, HTTP streaming, server-sent events (SSE),
WebSockets, callbacks, and [Trickle channels](../trickle/README.md).

Start with the [local example](../README.md#run-a-local-example). For deployment,
copy the [configuration example](../configs/orchestrator/config.example.toml),
set its paths and destinations, and run:

```sh
bin/livepeer orchestrator --config /etc/livepeer/orchestrator.toml
```

## Register runners

Configure a bootstrap credential, a static runner file, or both.

Dynamic runners register through `POST /runners/heartbeat` using the bootstrap
credential. The response supplies a credential scoped to that runner for later
heartbeats and unregister requests. Set `bootstrap_secret_file` as described in
[configuration and secrets](configuration.md#credential-files). If you use only
dynamic runners, omit `runner_config`.

Static runners are loaded from `runner_config`. Use the
[runner example](../configs/orchestrator/runners.example.toml) to set each
runner's ID, URL, application, mode, capacity, and health check. If you use only
static runners, omit `bootstrap_secret_file`. Their health URLs are checked
every `heartbeat_interval` (five seconds by default). A runner with a health URL
is unavailable until its first successful check; failed checks release its
sessions.

Discovery at `GET /discovery` lists available runners. Off-chain operation
accepts quoted runner prices but advertises free service. Enable the payment
settings below to charge clients.

## Allow local and private destinations

Runner forwarding, callback-created session proxies, and static health checks
have separate [destination grants](configuration.md#destination-grants). Use
the corresponding setting to allow connections to local or private addresses:

| Setting | Permits private destinations for |
| --- | --- |
| `runner_grants` | Application requests to runners |
| `session_proxy_grants` | Targets supplied when creating a session proxy |
| `health_grants` | Static runner health checks |

For a runner and health endpoint at `127.0.0.1:9000`, set both:

```toml
runner_grants = ["127.0.0.1:9000"]
health_grants = ["127.0.0.1:9000"]
```

Custom CA bundles use `runner_ca_file`, `session_proxy_ca_file`, or
`health_ca_file`. Ethereum RPC connections do not require grants.

## Public URLs and TLS

`service_url` is the public base URL advertised to clients. It can include a
deployment base path. Set `runner_service_url` if runners reach the listener
through a different URL or path. That URL is used for heartbeat responses,
session callbacks, Trickle `internal_url`, and the orchestrator-to-runner (O2R)
control channel. Client discovery, session, proxy, and channel URLs continue
to use `service_url`.

The application listener defaults to loopback HTTP. For direct HTTPS, set
`tls_cert_file` and `tls_key_file` to PEM files and use an HTTPS `service_url`.
For a non-loopback HTTP listener behind a TLS terminator, set `behind_tls = true`.
The separate metrics listener must remain on loopback; see
[probe addresses](configuration.md#local-storage-and-probes).

## Sessions and proxies

Each session keeps the price agreed when it was reserved. Later runner
heartbeats or exchange-rate updates affect new sessions. Stopping a session,
expiry, failed health, unregistering a runner, exhausted credit, or shutdown
cancels active HTTP, SSE, and WebSocket requests and removes session channels
and proxies. Single-shot requests use the same lifecycle. A runner stopping
its own active request should expect that request to end abruptly.

Setting `proxy = true` on a runner creates an automatic proxy. Without a URL
template, generated URLs use `service_url/run/{proxy}`. The earlier
`/proxy/{proxy}` path remains an alias for active proxies. A runner can also
create a session proxy through its callback route with a `target_url`.

`proxy_url_template` accepts a hostname label or final path segment, for example
`https://{proxy}.apps.example` or `https://apps.example/proxy/{proxy}`. Configure
DNS, TLS, and forwarding to this listener for those URLs. Hostname routing uses
the actual Host header. Single-shot domain proxies require a runner ID that is
a lowercase DNS label of at most 63 characters.

## Enable payments

Paid operation requires all of these settings:

- `payment_db`: persistent SQLite path with an existing parent directory.
- `keystore_file` and `keystore_password_file`: encrypted
  recipient account JSON and its password file; see
  [Ethereum keystores](configuration.md#ethereum-keystores).
- `payment_rpc_url_file` or `LIVEPEER_ORCHESTRATOR_PAYMENT_RPC_URL`: RPC credential.
- `payment_chain_id` and `payment_controller_address`: the payment chain and Controller.
- `ticket_face_value` and `ticket_win_prob`: positive decimal ticket parameters.
- Exactly one conversion source: `wei_per_usd` or `eth_usd_feed`.

Incomplete paid configuration fails startup. The recipient must be active on
the configured chain. Runner prices use USD per hour for live work or USD per
fixed request. Hourly discovery prices are normalized to USD and wei per second.

`wei_per_usd` sets a fixed conversion. Alternatively, set `eth_usd_feed` to a
Chainlink-compatible ETH/USD feed on the payment chain. The feed is checked
every 30 seconds; `price_max_age` defaults to two hours. Startup requires a
valid observation. Stale rates suspend new priced discovery and reservations;
existing sessions retain their agreed wei price.

`payment_max_fee_per_gas` optionally caps redemption fees in wei per gas. The
cap is per gas, not a total spending budget. Winning tickets wait until payment
parameters expire before redemption exposes their randomness on-chain. Only a
successful, canonical, finalized receipt settles their liability.

Challenges, balances, and replay guards live in memory; restart requires new
paid sessions. Winning tickets and transaction identities survive in SQLite.
See [payment recovery](payment-recovery.md) for backup and reconciliation, and
[staging and cutover](cutover.md) before moving production traffic.

## Resource limits

The registry accepts up to 256 runners and 10,000 sessions, with 25 generated
proxies per session. Control requests have 64 admission slots and a five-second
body deadline. Streaming requests have 256 separate slots.

The session channel API checks a 1,024-entry channel quota with 256 entries
reserved for runner controls. This is not a global Trickle memory limit.
Trickle retains five segments per channel and limits each segment to 10 MB
(10,000,000 bytes). There is no global byte cap or rolling window that allows a
segment to exceed that limit. Size memory for the channels and segments in use.
See the [protocol reference](../trickle/README.md) for missing-segment and
streaming behavior.
