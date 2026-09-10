[![Go 1.25](https://github.com/markmnl/fmsgd/actions/workflows/go1.25.yml/badge.svg)](https://github.com/markmnl/fmsgd/actions/workflows/go1.25.yml)

# fmsgd

Implementation of [fmsg](https://github.com/markmnl/fmsg) host written in Go! Uses local filesystem and PostgreSQL database to store messages.

## Building from source

Tested with Go 1.25 on Linux and Windows, AMD64 and ARM

1. Clone this repository
2. Run `go build ./cmd/fmsgd/`


## Environment

`FMSG_DATA_DIR`, `FMSG_DOMAIN`, `FMSG_ID_URL`, `FMSG_TLS_CERT` and `FMSG_TLS_KEY` are required to be set and valid; otherwise fmsgd will abort on startup. In addition to these `FMSG_` varibles, `PG` variables need to be set for the PostgreSQL database to use, refer to: https://www.postgresql.org/docs/current/libpq-envars.html

| Variable                   | Default | Description                                                                                                                                             |
|----------------------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| FMSG_DATA_DIR              |         | Path where messages will be stored. e.g. /opt/fmsg/data                                                                                                 |
| FMSG_DOMAIN                |         | Domain name this host is located. e.g. example.com                                                                                                      |
| FMSG_ID_URL                |         | Base HTTP URL for fmsg Id API, e.g. http://localhost:5000                                                                                                    |
| FMSG_TLS_CERT              |         | Path to TLS certificate file (PEM). Certificate must match `fmsg.<FMSG_DOMAIN>`.                                                                       |
| FMSG_TLS_KEY               |         | Path to TLS private key file (PEM).                                                                                                                     |
| FMSG_TLS_INSECURE_SKIP_VERIFY | false | Set to "true" to skip TLS certificate verification on outgoing connections. For development/testing only.                                               |
| FMSG_MAX_MSG_SIZE          | 10240   | Bytes. Maximum size above which to reject messages greater than before downloading them.                                                                |
| FMSG_PORT                  | 4930    | TCP port to listen on                                                                                                                                   |
| FMSG_MAX_PAST_TIME_DELTA   | 604800  | Seconds. Duration since message timestamp to reject if greater than. Note sending host could have been holding messages waiting for us to be reachable. |
| FMSG_MAX_FUTURE_TIME_DELTA | 300     | Seconds. Duration from message timestamp to reject if greater than.                                                                                     |
| FMSG_MIN_DOWNLOAD_RATE     | 5000    | Bytes per second. Used in setting download deadlines while downloading a message.                                                                       |
| FMSG_MIN_UPLOAD_RATE       | 5000    | Bytes per second. Used in setting upload deadlines while sending a message.                                                                             |
| FMSG_READ_BUFFER_SIZE      | 1600    | Bytes. Internal read buffer size per incoming connection                                                                                                |
| FMSG_RETRY_INTERVAL        | 20      | Seconds. Minimum time before retrying delivery to a recipient that previously failed.                                                                  |
| FMSG_RETRY_MAX_AGE         | 86400   | Seconds. Maximum age of a message since creation before giving up on delivery retries (default 1 day).                                                 |
| FMSG_POLL_INTERVAL         | 10      | Seconds. How often the sender polls the database for pending messages.                                                                                 |
| FMSG_MAX_CONCURRENT_SEND   | 1024    | Maximum number of concurrent outbound message deliveries.                                                                                              |
| FMSG_SKIP_DOMAIN_IP_CHECK  | false   | Set to "true" to skip verifying this host's external IP is in the fmsg DNS authorised IP set on startup.                                               |
| FMSG_SKIP_AUTHORISED_IPS  | false   | Set to "true" to skip verifying remote hosts IP is in the fmsg DNS authorised IP set during message exchange. WARNING setting this true effectively disables sender verification. |
| FMSG_CHALLENGE_MODE        | HAS_NOT_PARTICIPATED | When to issue an automatic CHALLENGE to the sending host. `HAS_NOT_PARTICIPATED` (default): challenge only when the message has no pid, or no message in the thread is from this host's domain. `ALWAYS`: always challenge. `NEVER`: never challenge. |



## Running

An up and running [fmsg Id API](https://github.com/markmnl/fmsgid) needs to be reachable by fmsgd to know users and their quotas for this fmsgd service. See also [fmsg-docker](https://github.com/markmnl/fmsg-docker) - a docker compose stack for a fmsg host including fmsgid, fmsg-webpi and fmsgd.

IP address to bind to and listen on is the only argument, `127.0.0.1` is used if argument not supplied. e.g. on Linux:

```
./fmsgd "0.0.0.0"
```

on Windows:
```
fmsgd.exe "0.0.0.0"
```

### systemd

An example systemd service to run fmsgd as a service on startup

ASSUMES: 
* Directory `/opt/fmsgd` has been created and contains built executable: `fmsgd`
* Text file `/etc/fmsgd/env` exists containing environment variables (example below)
* User `fmsg` has been created and has
    - read and execute permissions to `/opt/fmsgd/`, e.g. with `chown -R fmsg:fmsg /opt/fmsgd` after `mkdir /opt/fmsgd`
    - write permissions to FMSG_DATA_DIR
    - read permissions to /var/lib/fmsgd/tls
* Directory `/var/lib/fmsgd` has been created and owned by fmsg
* Valid TLS certs (see: [FMSG-001 TCP+TLS Transport and Binding Standard](https://github.com/markmnl/fmsg/blob/main/standards/fmsg-001-transport-and-binding.md#fmsg-001-tcptls-transport-and-binding-standard)) at paths /var/lib/fmsgd/tls/fullchain.pem and /var/lib/fmsgd/tls/privkey.pem


`/etc/systemd/system/fmsgd.service`

```
[Unit]
Description=fmsg Host
After=network-online.target
Wants=network-online.target

[Service]
Type=simple

User=fmsg
Group=fmsg

EnvironmentFile=/etc/fmsgd/env

ExecStart=/opt/fmsgd/fmsgd 0.0.0.0
WorkingDirectory=/opt/fmsgd

Restart=on-failure
RestartSec=3

# --- Filesystem access NOTE location of certs /var/lib/fmsgd/tls---
ReadOnlyPaths=/var/lib/fmsgd/tls
ReadWritePaths=/opt/fmsgd
ReadWritePaths=/var/lib/fmsgd
PrivateTmp=true

# --- Hardening ---
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

# --- Logging ---
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

```
FMSG_DATA_DIR=/var/lib/fmsgd/
FMSG_DOMAIN=example.com
FMSG_ID_URL=http://127.0.0.1:8080

FMSG_TLS_CERT=/var/lib/fmsgd/tls/fullchain.pem
FMSG_TLS_KEY=/var/lib/fmsgd/tls/privkey.pem

FMSG_MAX_MSG_SIZE=10240
FMSG_MAX_PAST_TIME_DELTA=604800
FMSG_MAX_FUTURE_TIME_DELTA=300
FMSG_MIN_DOWNLOAD_RATE=5000
FMSG_MIN_UPLOAD_RATE=5000
FMSG_READ_BUFFER_SIZE=1600

PGHOST=127.0.0.1
PGPORT=5432
PGUSER=
PGPASSWORD=
PGDATABASE=fmsgd
```

```
sudo systemctl daemon-reload
sudo systemctl enable fmsgd
sudo systemctl start fmsgd
```
## Immutable message finalization and upgrades

`fmsg-webapi` finalizes local messages with `pkg/message`: the timestamp, SHA-256,
exact header, and durable wire payloads are committed together, including local-only
messages and reactions. The hash covers the encoded wire header and expanded body
and attachment bytes. Compression and common media type encoding are chosen before
hashing. Add-to exchanges retain independent hashes and reuse the finalized payload.
The daemon reuses these representations for federation and challenge responses;
it refuses a representation that differs from an established hash.

The `wire_message` JSONB columns are versioned internal snapshots containing payload
paths; they are not API objects. `.fmsg-wire-*` directories beside message content
must be retained with the message database and data directory. Both services need
access to the shared files (normally the same service user/group). The API keeps its
expanded downloadable content separately. New received messages also preserve their
wire payloads before expanding the downloadable copies.

Upgrade the daemon, API and schema together while message writes and federation are
paused: install compatible binaries, rerun `dd.sql`, backfill, then resume services.
The schema refuses a newly committed sent message without a 32-byte hash. It is not
compatible with an older API that stamps only `time_sent`. Existing hashes are never
replaced by the migration.

Build the maintenance command with `go build -o fmsg-backfill ./cmd/fmsg-backfill`.
It uses the same standard `PG*` connection variables as the daemon and must have
access to the stored file paths. First inspect, then apply:

```sh
./fmsg-backfill -domain example.com
./fmsg-backfill -domain example.com -apply
```

The default invocation lists pending local messages and batches without writing.
`-apply` preserves timestamps and finalizes parents before children; it can be rerun.
Missing files, inconsistent already-hashed children, or legacy representations that
cannot reproduce an existing hash are reported with a nonzero exit status. Resolve
these records before resuming dependent delivery; hashes are not silently rewritten.
A process crash before commit may leave an unreferenced `.fmsg-wire-*` directory;
only remove such directories after checking both snapshot columns for references.

PostgreSQL tests use an isolated temporary schema in the supplied test database:

```sh
FMSG_TEST_DATABASE_URL=postgres://postgres@localhost/fmsg_test?sslmode=disable go test ./...
```
