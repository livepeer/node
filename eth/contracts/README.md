# Contract bindings

The ten generated Go files in this directory and `chainlink/` are copied
unchanged from `livepeer/go-livepeer` commit
`bd645a09266833fb859053445d9ac85846330756`:

- `controller.go`
- `bondingManager.go`
- `ticketBroker.go`
- `roundsManager.go`
- `serviceRegistry.go`
- `livepeerToken.go`
- `minter.go`
- `poll.go`
- `LivepeerGovernor.go`
- `chainlink/AggregatorV3Interface.go`

Their filtered source history is a parent of the binding import commit. It
preserves the authors, committers, dates, and messages, with `Original-Commit`
trailers linking each retained commit to `livepeer/go-livepeer`. Contributors
include Eric Tang, Yondon Fu, Nico Vergauwen, Rafał Leszko, Rick Staa, and
Victor Elias. The original MIT notice is in
[`LICENSE`](../../LICENSE).

Keep generated schemas and methods unchanged. On a deliberate upstream update,
copy these files from the selected committed revision, review its contract/API
changes, and update this provenance. For example, from a checkout of this repo:

```sh
git -C /path/to/go-livepeer show REVISION:eth/contracts/bondingManager.go > eth/contracts/bondingManager.go
```

Application policy lives in the parent `eth` package: canonical snapshots,
transaction planning, hints, fee limits, and saved transaction identities.
Generated files include unused upstream methods. The supported CLI operations
are listed in [chain commands](../../docs/chain.md).
