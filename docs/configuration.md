# Configuration and secrets

Each component reads strict TOML through `--config`. Unknown keys are rejected.
Use the [orchestrator](../configs/orchestrator/config.example.toml),
[signer](../configs/signer/config.example.toml), and
[chain](../configs/chain/config.example.toml) examples as starting points.
Paths in configuration are resolved from the process's working directory;
use absolute paths in deployed configurations.

## Names and environment variables

Orchestrator TOML keys use snake case, such as `service_url`. Signer and chain
keys use names such as `RPCURLFile` and `KeystoreFile`. Command-line flags use kebab
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

## Ethereum keystores

All three apps load one encrypted geth account JSON file, including Web3 v3
files created by go-livepeer. Point to the individual account file inside the
keystore directory. Directory discovery and account creation are handled by
external tooling.

| App | Keystore TOML key | Password-file TOML key |
| --- | --- | --- |
| Signer and chain | `KeystoreFile` | `KeystorePasswordFile` |
| Orchestrator | `keystore_file` | `keystore_password_file` |

All three apps use `--keystore-file` and `--keystore-password-file`. Path
environment variables use the existing app prefix, such as
`LIVEPEER_SIGNER_KEYSTORE_PASSWORD_FILE`. CLI values override environment
values, which override TOML.

Both inputs must be regular files readable only by their owner; `0400` and
`0600` are accepted. Secret-manager symlinks are accepted when their target
files meet these requirements. Password files are read as exact bytes, including
whitespace and trailing newlines. Supply the actual password without an
unintended newline. There is no literal-password flag, password environment
value, or interactive unlock prompt. An existing keystore using an empty
password requires an explicitly supplied empty password file.

The signer requires both files at startup. The orchestrator requires both when
payments are configured. The chain requires both only with `--submit`; reads,
simulations, help, completion, and `--print-config` do not open signing files.
Configuration printing omits both paths. Decrypted keys remain in process
memory and are never written back to disk. Changing either file requires a
service restart, or a new chain invocation.

The signer account supplies the TicketBroker deposit and reserve. The
orchestrator account receives and redeems tickets. A chain sender must match
its signing account. Keep a redemption key exclusive to its orchestrator worker;
using it concurrently from a CLI or another process can conflict with nonce
tracking.

### Migrate raw-key configurations

Raw hexadecimal key files are no longer accepted. The former `KeyFile`,
`--private-key-file`, `payment_key_file`, and `--payment-key-file` settings are
removed. Update deployments to the new keystore path environment variables
when configuring paths through the environment.

Reuse the account's existing go-livepeer encrypted JSON file when available.
Otherwise, import the existing raw key with trusted geth tooling, using its
standard encryption parameters:

```sh
umask 077
geth --keystore /path/to/encrypted-keystore account import /path/to/raw-key
```

This external command asks for the encryption password and creates an account
JSON file. Select that file, prepare an owner-only file containing exactly the
password used during import, and replace the old raw-key configuration with the
two new paths. Preserve the Ethereum address: do not create a new account or
change the signer sender, recipient, deposits, or payment database. Confirm the
address printed by geth matches the current account before restarting. Back up
the encrypted account and its password separately before retiring the raw-key
file. Payment and accounting databases need no schema migration.

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
