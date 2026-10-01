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

Keep generated schemas and methods unchanged. On a deliberate upstream update,
copy these files from the selected committed revision, review its contract/API
changes, and update this provenance. For example, from a checkout of this repo:

```sh
git -C /path/to/go-livepeer show REVISION:eth/contracts/bondingManager.go > eth/contracts/bondingManager.go
```

Application policy lives in the parent `eth` package: canonical snapshots,
transaction planning, hints, fee limits, and saved transaction identities.
Whole generated files retain unused methods; their presence does not add CLI
commands or authorize broader protocol behavior.
