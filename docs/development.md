# Development

The module requires Go 1.27.1 or newer. Build all four executables with:

```sh
make build
bin/livepeer version
bin/livepeer orchestrator --help
bin/livepeer completion orchestrator bash
```

The dispatcher runs bundled sibling executables or executables under `libexec`;
it does not search `PATH`. Keep the component executables with the dispatcher
when packaging the bundle. It forwards process status, streams, and signals.
Use `VERSION` and `COMMIT` make variables to set release metadata.

## Checks

```sh
go test -race ./...
go vet ./...
go build ./...
```

[CI](../.github/workflows/ci.yml) also runs Staticcheck and Govulncheck, provisions
the SDK fixtures below, and saves test artifacts. Import boundaries are checked
by `TestImportLattice` in [cmd/livepeer/imports_test.go](../cmd/livepeer/imports_test.go).

## SDK integration fixtures

Integration tests use these pinned revisions:

| SDK | Revision | Environment variable |
| --- | --- | --- |
| Go: `livepeer/golang-runner` | `c3be5a14a91f8a3437133419becaced324d804fe` | `GO_RUNNER_SDK_DIR` |
| Python: `livepeer/livepeer-python-gateway` | `44df06157fcdb864e37d971e8caba86b2a7dc92e` | `PYTHON_RUNNER_SDK_DIR` |

Use clean checkouts at those revisions. To provision fresh fixtures in a
temporary directory (requires Git, Python 3.12, and `uv`):

```sh
livepeer_fixture_dir=$(mktemp -d)
export GO_RUNNER_SDK_DIR="$livepeer_fixture_dir/golang-runner"
export PYTHON_RUNNER_SDK_DIR="$livepeer_fixture_dir/livepeer-python-gateway"
git clone https://github.com/livepeer/golang-runner.git "$GO_RUNNER_SDK_DIR"
git -C "$GO_RUNNER_SDK_DIR" checkout --detach c3be5a14a91f8a3437133419becaced324d804fe
git clone https://github.com/livepeer/livepeer-python-gateway.git "$PYTHON_RUNNER_SDK_DIR"
git -C "$PYTHON_RUNNER_SDK_DIR" checkout --detach 44df06157fcdb864e37d971e8caba86b2a7dc92e
uv sync --project "$PYTHON_RUNNER_SDK_DIR" --locked --no-dev --python 3.12
export PYTHON_RUNNER_PYTHON="$PYTHON_RUNNER_SDK_DIR/.venv/bin/python"
go test -race ./...
```

Without explicit paths, tests look for sibling `golang-runner` and
`python-runner` checkouts and the Python checkout's `.venv/bin/python`. SDK
coverage can be skipped when the checkouts or dependencies are absent, so read
the test logs before reporting a compatibility result. Python fixtures normally
export the committed SDK tree. Set `PYTHON_RUNNER_USE_WORKING_TREE=1` to exercise
local SDK edits.

The fixtures cover persistent and single-shot sessions, proxy modes, discovery,
runner callbacks, Trickle, and Python media/control round trips. Payment tests
cover live/fixed calls, sustained refresh, sender exposure, concurrent liability,
and crash/reorg/finality handling with deterministic Ethereum interfaces. These
do not replace [staging against real contracts](cutover.md).

## CLI and wire snapshots

Refresh CLI help and completion snapshots after an intentional interface change:

```sh
UPDATE_CLI_GOLDENS=1 go test ./cmd/livepeer -run TestRealBinaryOffchainFlow
```

Review the resulting diff. Payment Protobuf fixtures are produced by the pinned
Python SDK; codec tests compare selected-field decoding with the generated
runtime. See the [HTTP API reference](http-api.md).
