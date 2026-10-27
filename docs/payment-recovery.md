# Payment state and transaction recovery

The orchestrator database contains winning ticket signatures and the recipient
randomness needed to redeem those winners. Keep it owner-only and backed up.
Payment parameters, authentication keys, session balances, and ticket nonce
replay guards are not persisted. Restarting rotates the keys and requires new
paid sessions; stored winning tickets remain redeemable.

Clients carry the signer's signed payment state, which is a bearer credential
while authorization is cached. When Kafka is enabled, each signer also owns a
durable accounting-event outbox. Back up that outbox and the clearinghouse's
accounting state independently. Start fresh client sessions after restoring
recipient state or migrating from legacy signed state.

See [orchestrator setup](orchestrator.md#enable-payments) and
[signer setup](signer.md#kafka-accounting-and-budgets) for configuration.

## Signer accounting recovery

Keep `Kafka.OutboxDB` on persistent local storage, with one file per signer replica.
Missing parent directories are created at startup. New files have mode 0600;
existing files must be owner-only regular files. SQLite uses WAL and FULL synchronous
commits. Back up with SQLite's backup API or stop the signer before copying the database
and any WAL/SHM files. Monitor both pending payload bytes and actual disk usage.

Use a dedicated owner-only directory (mode 0700) and preserve owner-only
permissions on the database, `-wal`, and `-shm` files during backup and restore.
Startup rejects nonregular or permissive database and sidecar files. The WAL
can contain accounting payloads.

Restart with the same signer key, Kafka broker, topic and outbox path. Pending events
resume automatically with their original IDs and exact payloads. A nonempty outbox
rejects a changed signer address, broker or topic. Drain it before changing those
settings; credential rotation does not change its destination binding. Broker
addresses learned from Kafka metadata can change without changing the bootstrap
configuration. Never delete pending rows or replace the outbox to clear an outage.

Only acknowledged events are removed. A crash after Kafka accepts a batch but before
local deletion causes replay; the consumer must deduplicate event IDs. A partial or
uncertain write retains the entire batch. The outbox is a delivery queue, not an audit
archive: acknowledged payloads are deleted. Restoring a backup can replay old events
and omit events created after that backup, so preserve the latest outbox during rollback.

Kafka outages allow signing to continue until the outbox runs out of capacity. Failed
local persistence returns 503 without payment credentials. Events are saved before the
HTTP response; connection loss after saving an event does not retract its charge.
Separate client retries emit separate events. Payment state can still move between
replicas with the same key and webhook configuration; outbox files must not be shared.

## Orchestrator transaction recovery

The keystore account signs redemptions; tickets retain their original recipient,
even if `OrchestratorAddress` or the keystore account changes. Redemption attempts
use these states:

| State | Meaning and recovery |
| --- | --- |
| Queued (no attempt) | Wait for parameter expiry. Preparation failures leave the ticket eligible for another attempt. |
| `prepared` | Signed bytes, nonce and hash are durable before sending; restart resumes the saved transaction. |
| `broadcast` | Send in progress or interrupted before recording its outcome; restart resumes the saved transaction. |
| `submitted` | RPC acknowledged the hash; monitor inclusion and replace on timeout. |
| `retryable` | Submission, waiting or replacement failed; retry the ticket automatically. Older hashes remain under receipt reconciliation. |
| `mined` | Canonical inclusion observed; wait for finalization without fee replacement. A reorg resumes recovery. |
| `awaiting_finality` | Latest state says the ticket is used; pause submission until finalized state confirms use or a reorg removes it. |
| `confirmed` / `used` | Successful canonical finalized receipt or finalized ticket-use observation settles liability. |
| `reverted` / `invalid` | Finalized transaction failure or an invalid ticket; stop submitting and reconcile unresolved hashes. |
| `expired` | Ticket creation round is outside its validity window; reconcile unresolved hashes. |
| `superseded` | Finalized inclusion consumed the same sender/nonce, or finalized ticket settlement made this transaction unnecessary. |

Only canonical finalized state settles a ticket. RPC failures, missing receipts,
unfinalized inclusions and reorgs retain uncertainty and liability. Each ticket
counts once even when it has several signed transactions. Previously exposed
recipient randomness stays closed to new payments throughout retries.

`RedeemerTxTimeout` (`--redeemer-tx-timeout`) defaults to five minutes per
inclusion attempt. `RedeemerMaxTxReplacements` (`--redeemer-max-tx-replacements`)
defaults to one; zero disables replacements. Replacements retain the nonce and
calldata and raise both fee caps by geth's default 10% threshold, rounding up
with a strict increase.
Geth estimates initial fees; `RedeemerMaxFeePerGas` caps initial and replacement
fees in wei per gas. Finalization waits do not consume the inclusion timeout.

Restart restores deadlines, replacement budgets and nonce reservations for
acknowledged sends. Rejected submissions do not reserve a nonce. A rotated key can
resend saved bytes and reconcile receipts, but cannot replace the previous account's transactions.

Migration 002 preserves existing transactions. Its rollback refuses to discard
transactions or metadata that the old schema cannot represent. Retain the upgraded
database for a compatible binary when rollback is refused.

Inspect status without a key or RPC connection:

```sh
livepeer orchestrator redemptions
```

Automatic recovery normally handles rebroadcast. To explicitly resend the
current retryable transaction using its saved bytes and fees:

```sh
livepeer orchestrator redemptions \
  --retry-transaction 0xTRANSACTION_HASH --submit \
  --rpc-url-file /run/secrets/livepeer-payment-rpc-url \
  --chain-id 42161
```

Recovery and migration commands default to `arbitrum-one-mainnet`, using
`~/.lpData/arbitrum-one-mainnet/orchestrator/`. For other networks or directories,
pass the same `--config`, or `--network` and `--data-dir`, as the service.
`--redeemer-db` selects a different database. Recovery reads `RPCURLFile` and `ChainID`
for retries; `--rpc-url-file` and `--chain-id` override them. It does not unlock
the account or read unrelated service credentials. RPC endpoints use normal
system TLS trust. Historical hashes remain inspectable; manual retry cannot
reopen a terminal ticket or displace a newer active transaction.

In-memory control state is bounded: payment sessions have a 100,000-entry quota
and 24-hour inactivity retention; ticket replay guards have a 1,000,000-nonce
quota and are pruned at payment parameter expiry. Active session accounting keeps
session activity current. Expiry is monotonic within a recipient lifetime so
a backward chain observation cannot revive parameters after their replay guards
are removed. Auth-token refresh never resets nonce tracking. Winning tickets
and uncertain transactions are retained for audit/reconciliation, so back up
and monitor database disk use independently of these control-state quotas.
