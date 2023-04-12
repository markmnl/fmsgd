# fmsgd

[![go 1.19](https://github.com/markmnl/fmsgd/actions/workflows/go1.19.yml/badge.svg)](https://github.com/markmnl/fmsgd/actions/workflows/go1.19.yml)


Implementation of [fmsg](https://github.com/markmnl/fmsg) host written in go! Uses local filesystem and PostgreSQL database to store messages per the [fmsg-store-postgres standard](https://github.com/markmnl/fmsg/blob/main/STANDARDS.md).

## Building from source

Tested with go 1.18, 1.19 and 1.20 on Linux (AMD64, ARM64) and Windows AMD64

1. Clone this repository
2. Navigate to src/
2. Run `go build .`


## Environment

fmsgd uses environment variables as a control surface. `FMSG_DATA_DIR` and `FMSG_DOMAIN` are required to be set; otherwise the host will fail to start. In addition to these `FMSG_` varibles, `PG` variables need to be set for the PostgreSQL database to use, refer to: https://www.postgresql.org/docs/current/libpq-envars.html

| Variable                   | Default | Description                                                                                                                                             |
|----------------------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| FMSG_DATA_DIR              |         | Path where messages will be stored. e.g. /opt/fmsg/data                                                                                                 |
| FMSG_DOMAIN                |         | Domain name this host is located. e.g. example.com                                                                                                      |
| FMSG_MAX_MSG_SIZE          | 10240   | Bytes. Maximum size above which to reject messages greater than before downloading them.                                                                |
| FMSG_PORT                  | 36900   | TCP port to listen on                                                                                                                                   |
| FMSG_MAX_PAST_TIME_DELTA   | 604800  | Seconds. Duration since message timestamp to reject if greater than. Note sending host could have been holding messages waiting for us to be reachable. |
| FMSG_MAX_FUTURE_TIME_DELTA | 300     | Seconds. Duration from message timestamp to reject if greater than.                                                                                     |
| FMSG_MIN_DOWNLOAD_RATE     | 5000    | Bytes per second. Used in setting download deadlines while downloading a message.                                                                       |
| FMSG_MIN_UPLOAD_RATE       | 5000    | Bytes per second. Used in setting upload deadlines while sending a message.                                                                             |
| FMSG_READ_BUFFER_SIZE      | 1600    | Bytes. Internal read buffer size per incoming connection                                                                                                |


## Running

IP address to bind to and listen on must be supplied as the first argument, e.g.:

```
./fmsgd "0.0.0.0"
```

### systemd

An example systemd service to run fmsgd as a daemon.

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