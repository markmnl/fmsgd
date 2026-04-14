# fmsgd

[![Go 1.25](https://github.com/markmnl/fmsgd/actions/workflows/go1.25.yml/badge.svg)](https://github.com/markmnl/fmsgd/actions/workflows/go1.25.yml)


Implementation of [fmsg](https://github.com/markmnl/fmsg) host written in Go! Uses local filesystem and PostgreSQL database to store messages per the [fmsg-store-postgres standard](https://github.com/markmnl/fmsg/blob/main/STANDARDS.md).

## Building from source

Tested with Go 1.25 on Linux and Windows, AMD64 and ARM

1. Clone this repository
2. Navigate to src/
2. Run `go build .`


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
* Text file `/opt/fmsgd/env` exists containing environment variables
* User `fmsg` has been created and has
    - read and execute permissions to `/opt/fmsgd/`, e.g. with `chown -R fmsg:fmsg /opt/fmsgd` after `mkdir /opt/fmsgd`
    - write permissions to FMSG_DATA_DIR

`/etc/systemd/system/fmsgd.service`

```
[Unit]
Description=fmsg Host
After=network.target

[Service]
EnvironmentFile=/opt/fmsgd/env
ExecStart=/opt/fmsgd/fmsgd "0.0.0.0"
User=fmsg
Group=fmsg

[Install]
WantedBy=multi-user.target
```

```
sudo systemctl daemon-reload
sudo systemctl enable fmsgd
sudo systemctl start fmsgd
```