# Configuration and secrets

Each component reads strict TOML through `--config`. Unknown keys are rejected.
Use the [orchestrator](../configs/orchestrator/config.example.toml),
[signer](../configs/signer/config.example.toml), and
[chain](../configs/chain/config.example.toml) examples as starting points.
Paths in configuration are resolved from the process's working directory;
use absolute paths in deployed configurations.

## Names and environment variables

Orchestrator TOML keys use snake case, such as `service_url`. Signer and chain
keys use names such as `RPCURLFile` and `KeyFile`. Command-line flags use kebab
case. Environment variables use the component prefix and underscores:

| Component | Prefix | Example |
| --- | --- | --- |
| Orchestrator | `LIVEPEER_ORCHESTRATOR_` | `LIVEPEER_ORCHESTRATOR_BOOTSTRAP_SECRET` |
| Signer | `LIVEPEER_SIGNER_` | `LIVEPEER_SIGNER_RPC_URL` |
| Chain | `LIVEPEER_CHAIN_` | `LIVEPEER_CHAIN_RPC_URL` |

Run `bin/livepeer COMPONENT --help` for flags and defaults. The orchestrator and
chain support `--print-config` to print configuration with secrets omitted:

```sh
bin/livepeer orchestrator --config /etc/livepeer/orchestrator.toml --print-config
bin/livepeer chain --config /etc/livepeer/chain.toml --print-config
```

Printed configuration also omits secret file paths and the static runner file
path. Chain action inputs, transaction switches, `--output`, and `--print-config`
must be supplied on the command line.

## Destination grants

Grants permit connections to local, private, or other restricted addresses;
public destinations need none. Each grant matches a hostname and port for one
purpose, such as runner traffic or signer discovery, including redirects.

Grants use `host[:port]`, optionally prefixed with `http://` or `https://`.
The scheme defaults to HTTPS; omitted ports default to 443 for HTTPS or 80 for
HTTP.

## Credential files

RPC URLs, bootstrap credentials, authorization webhook URLs and headers, and
Kafka credentials are secrets. Supply each through its file option or direct
environment variable. Direct secret flags and TOML values are rejected, and a
direct environment secret cannot be set together with its file option.

Credential files are read as exact bytes, including trailing newlines. For
example, create a bootstrap credential without a newline:

```sh
umask 077
openssl rand -hex 32 | tr -d '\n' > runner-bootstrap.secret
```

Set `bootstrap_secret_file` to that file's absolute path and give the same
credential to runners during initial registration. Secret-manager mounts must
also contain exactly the intended bytes.

## Ethereum key files

`KeyFile` on the signer and chain, and `payment_key_file` on the orchestrator,
contain a raw Ethereum private key: 64 hexadecimal digits, optionally prefixed
with `0x`. Encrypted JSON keystores are not accepted. The key loader trims
surrounding whitespace, unlike the credential-file loader above.

Keep the file readable only by its owner, for example:

```sh
chmod 600 /run/secrets/livepeer-signer-key
```

The signer key signs outgoing tickets; its account supplies the TicketBroker
deposit and reserve. The orchestrator key receives and redeems tickets.
A chain sender must match its signing key. Keep a redemption key exclusive
to its orchestrator worker; using it concurrently from a CLI or another process
can conflict with nonce tracking.

## Local storage and probes

Create database parent directories before starting a paid orchestrator or a
signer with Kafka. Keep their SQLite files owner-only and on persistent storage.
For signer accounting, use an owner-only parent directory and preserve those
permissions on SQLite's WAL and SHM files during restore.
See [payment recovery](payment-recovery.md) for backup and restart procedures.

Health and metrics endpoints use separate loopback listeners:

| Component | Default application address | Default metrics address |
| --- | --- | --- |
| Orchestrator | `127.0.0.1:8935` | `127.0.0.1:8936` |
| Signer | `127.0.0.1:8937` | `127.0.0.1:8938` |

```sh
curl -fsS http://127.0.0.1:8936/readyz
curl -fsS http://127.0.0.1:8938/readyz
```

Both metrics listeners serve `/healthz`, `/readyz`, and `/metrics`.
Orchestrator readiness reports that the listener is running; it does not check
runner health or payment dependencies. Signer readiness checks its payment
dependencies and, when enabled, local accounting storage. See the component
guides for those checks.
