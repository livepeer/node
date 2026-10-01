# Chain commands

The chain CLI inspects Ethereum accounts and Livepeer contracts and manages
stake, earnings, TicketBroker funds, and rounds. It does not start an HTTP
server. Use the [configuration example](../configs/chain/config.example.toml)
and [key and secret guide](configuration.md).

## Commands

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

Run `bin/livepeer chain COMMAND --help` for each command's arguments. Shared
flags can appear before or after subcommands:

```sh
bin/livepeer chain status --config /etc/livepeer/chain.toml
bin/livepeer chain --config /etc/livepeer/chain.toml --output json stake bond ORCHESTRATOR_ADDRESS --amount 1000000000000000000
```

## Preview and submit

Run a state change without `--submit` to inspect its simulation, gas estimate,
and call data. Add `--submit` and `KeyFile` to broadcast; add `--wait` to wait
for inclusion in a successful receipt. The sender must match the signing key,
and an expected chain ID must match the RPC.

The submission hash is printed before waiting and remains available on errors.
JSON output uses separate JSON-line records for submission and receipt.
`--quiet` suppresses normal transaction output, including dry-run plans,
submission hashes, and receipts. Errors go to stderr; broadcast and receipt
errors retain the transaction hash. Exit status is 0 on success and 2 on failure.

Transaction switches, action inputs, `--output`, and `--print-config` are
command-line controls. TOML holds shared settings: RPC connection, expected
chain ID, Controller, sender, key path, and an optional maximum fee per gas.

```sh
bin/livepeer chain ticketbroker fund --config /etc/livepeer/chain.toml --amount 1000000000000000000 --reserve 0 --submit --quiet
```

Amounts and fee ceilings are decimal integers in the contract's base units;
for ETH, 1 ETH is 1,000,000,000,000,000,000 wei. Check the command's units before
submitting. Transactions require dynamic fees and fail if the RPC header has
no base fee. `--max-fee-per-gas` (TOML `MaxFeePerGas`) caps fees in wei per gas;
an estimate above the cap fails before signing. This does not cap the total
transaction cost or cumulative spending.

## Staking and activation

If `stake bond` needs token approval, it submits approval first. Without
`--wait`, rerun the bond after approval is confirmed. With `--submit --wait`,
the command continues after the approval's successful receipt. Bonding requires
a positive amount of new tokens.

`stake rebond --delegate ADDRESS` uses `rebondFromUnbonded` for an unbonded
account; omit the delegate for an already bonded account. Staking and reward
commands calculate sorted-pool position hints.

Before `orchestrator activate`, self-bond with
`stake bond YOUR_ADDRESS --amount AMOUNT` and set the public URI with
`orchestrator set-config --service-uri URL`.

The Go `eth` APIs also support explicit delegation changes, voting, and
delegated reward callers. Those operations do not yet have CLI commands.
CLI receipt waiting checks successful inclusion; payment redemption uses the
stricter finalized-receipt policy described in [payment recovery](payment-recovery.md).
