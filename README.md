[![Build & Test](https://github.com/markmnl/fmsgd/actions/workflows/build-test.yml/badge.svg?branch=main)](https://github.com/markmnl/fmsgd/actions/workflows/build-test.yml?query=branch%3Amain)
[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)

# fmsgd

Implementation of [fmsg](https://github.com/markmnl/fmsg) host written in Go! Uses local filesystem and PostgreSQL database to store messages.

## Building from source

Requires Go 1.27 or newer.

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
| FMSG_PORT                  | 4930    | TCP port to listen on, and UDP port for [indicators](#indicators-fmsg-006). 4930 is the IANA-registered port for service name `fmsg`.                  |
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
| FMSG_SKIP_AUTHORISED_IPS  | false   | Set to "true" to skip verifying remote hosts IP is in the fmsg DNS authorised IP set during message exchange, and an indicator datagram's source address. WARNING setting this true effectively disables sender verification. |
| FMSG_CHALLENGE_MODE        | HAS_NOT_PARTICIPATED | When to issue an automatic CHALLENGE to the sending host. `HAS_NOT_PARTICIPATED` (default): challenge only when the message has no pid, or no message in the thread is from this host's domain. `ALWAYS`: always challenge. `NEVER`: never challenge. |
| FMSG_INDICATORS            | true    | Set to "false" or "0" to turn off [indicators](#indicators-fmsg-006): fmsgd then neither listens on UDP nor sends indicator datagrams. |



## Quotas and usage

For each local recipient of a message from another host, fmsgd asks fmsgid for the address's receive limits and usage, and rejects the recipient with `101` (user full) if one more message of this size would exceed its messages per day, size per day or total size (`-1` is unlimited). The size is the stored size: the message body plus every attachment, decompressed. Once the message is stored, fmsgd records that size for each recipient that accepted it with `POST /fmsgid/recv`, timestamped when this host accepted the message (not the sender's timestamp), so a backdated message still counts towards today. fmsgd doesn't record send usage or mail between local addresses: the client that sends and delivers locally (such as fmsg-webapi) does.

## Indicators (FMSG-006)

fmsgd implements [FMSG-006 Indicators](https://github.com/markmnl/fmsg/blob/main/standards/fmsg-006-indicators.md): short-lived, single-emoji states such as ✍️ (composing a reply) that a participant shows on a message. They travel host to host as single UDP datagrams on port 4930 (`FMSG_PORT`), at the same address fmsgd listens on for TCP. They are never acknowledged, answered or stored.

**Receiving.** Each datagram is checked in this order and dropped silently at the first failure:

1. a per-source-address rate limit (50 datagrams per second, burst 100);
2. the datagram is version 1, exactly its fields with no trailing bytes, and its data is empty (clear) or a single emoji of at most 64 bytes;
3. its hash matches a message this host holds, or one of its add-to batch messages;
4. its index names a participant of that message (from, to, then add to from and add to for a batch message), and that participant is not on this host's domain;
5. the source address is in `fmsg.<domain>` of that participant's domain (cached for 60 seconds; skipped with `FMSG_SKIP_AUTHORISED_IPS`).

The ttl is clamped to 120 seconds and the indicator is published to the client API (see below).

**Sending.** For an indicator set by a participant on this host's domain, fmsgd sends one datagram to each other participant domain of the message, naming the participant by their lowest index in the message that domain holds: the original message when the domain's participants and the indicating participant are among its from and to, otherwise the first add-to batch message that has both. It sends to one address of `fmsg.<domain>` (cached for 60 seconds), alternating between addresses, from the UDP socket it listens on. Rate limits:

- at most one datagram per participant per message every 2 seconds; more frequent changes are coalesced and only the latest is sent when the interval allows. Clearing (empty) datagrams are sent at once;
- at most 20 datagrams per second to any one receiving domain; the excess is dropped.

**Client API.** fmsgd and the client API (such as fmsg-webapi) share the PostgreSQL NOTIFY channel `indicator`. Its payload is JSON:

```json
{"id": 123, "from": "@bob@example.com", "emoji": "✍️", "ttl": 6}
```

`id` is this host's `msg.id` of the message, `from` the indicating participant, `emoji` the indicator, or `""` with `ttl` `0` to clear, and `ttl` the seconds it lasts (at most 120). The client API notifies when a local participant sets or clears an indicator, and fmsgd sends it on; fmsgd notifies when it receives and verifies a datagram, and the client API delivers it to connected local participants. fmsgd only sends for addresses on its own domain, so the indicators it publishes are never sent again.

Firewalls in front of fmsgd need UDP 4930 open inbound and outbound, as well as TCP 4930, for indicators to work. Without it, message exchange is unaffected; indicators are simply lost.

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
