# Phoenix MailStatus Collector

**Turn Postfix delivery logs into a reliable stream of RabbitMQ events.**

[![Go Reference](https://pkg.go.dev/badge/github.com/Yukthi-Systems/Phoenix-MailStatus-Collector.svg)](https://pkg.go.dev/github.com/Yukthi-Systems/Phoenix-MailStatus-Collector)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)

[Features](#features) ·
[Quick start](#quick-start) ·
[Message format](#message-format) ·
[Configuration](#configuration) ·
[Contributing](#contributing) ·
[Community](#community)

---

`mailstatus-collector` follows the systemd journal on a Postfix server, works
out what happened to every outgoing mail (**delivered**, **deferred**,
**bounced** or **expired**), and publishes one JSON message per mail and
outcome to RabbitMQ. Your application can then show delivery status, retry
logic or analytics without parsing mail logs itself.

It is a single static Go binary with no cgo, uses about 15–17 MB of memory,
and never loses a result: each batch is confirmed by the broker before the
journal position moves on.

## Table of contents

- [Features](#features)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Message format](#message-format)
- [Delivery guarantees](#delivery-guarantees)
- [Postfix settings](#postfix-settings)
- [Configuration](#configuration)
- [Operations](#operations)
- [Development](#development)
- [Roadmap](#roadmap)
- [Contributing](#contributing)
- [Security](#security)
- [Community](#community)
- [License](#license)

## Features

- **Complete outcomes:** `Delivered`, `Deferred`, `Bounced` and `Expired`,
  with the remote server's SMTP reply, DSN code and relay for every recipient.
- **Business context:** each message carries your application's reference
  header (for example `X-YSPL-ID`), the subject and the `Message-ID`, so you can
  match results to your own records.
- **At-least-once, no loss:** publisher confirms, mandatory publishing, and an
  atomically written state file. Deterministic per-recipient IDs make
  de-duplication trivial for consumers.
- **Outage-tolerant:** during a broker outage the collector stops reading and
  the journal holds the backlog. Memory stays flat and no spool files are needed.
- **Content-filter aware:** results for internal hops (such as a DKIM-signing
  filter that re-queues mail) are skipped, and their context is carried over
  to the real outgoing delivery.
- **Operations-friendly:** hardened systemd unit, structured JSON logs with
  built-in rotation, dry-run preview and backfill from journal exports.

## How it works

```mermaid
flowchart LR
    app[Your application] -->|SMTP| pf[Postfix]
    pf -->|optional<br/>content filter| filter["Filter<br/>(e.g. DKIM)"]
    filter -->|re-queued| pf
    pf -->|delivery| mx["Recipient MX<br/>(Gmail, Outlook, ...)"]
    pf -. log lines .-> jd[(systemd journal)]
    jd -->|journalctl -f -o json| mc[mailstatus-collector]
    mc -->|publisher confirms| mq[(RabbitMQ queue)]
    mc <-->|position +<br/>in-flight mail| st[(state.json)]
    mq --> consumer[Your consumer]
```

Postfix logs the sender, the headers and each delivery attempt on separate
lines, tied together only by the queue ID. The collector rebuilds the full
picture of every mail as it moves through the queue:

```mermaid
flowchart LR
    subgraph collector[mailstatus-collector: one goroutine]
        direction LR
        r[journal.Reader] --> p[postfix.Parser]
        p --> t[tracker.Tracker]
        t --> c[collector.Collector<br/>batch ≤ 500 / 1 s]
    end
    c --> pub[rabbitmq.Sink]
    pub -->|1. publish + confirm| mq[(RabbitMQ)]
    pub -->|2. then save| sf[(statefile)]
```

| Postfix log line | Service | What the collector takes from it |
|---|---|---|
| `warning: header X-YSPL-ID: …` | `cleanup` | Your reference ID (`euid`) |
| `warning: header Subject: …` | `cleanup` | Subject (MIME-decoded) |
| `message-id=<…>` | `cleanup` | `Message-ID` |
| `from=<…>, size=…, nrcpt=…` | `qmgr` | Envelope sender, first-seen time |
| `to=<…>, relay=…, dsn=…, status=…` | `smtp`, `lmtp`, … | One result per recipient per attempt |
| `from=<…>, status=expired` | `qmgr` | All still-deferred recipients become `Expired` |
| `removed` | `qmgr` | The mail left the queue; its context is dropped |

The life of one recipient, as published:

```mermaid
stateDiagram-v2
    [*] --> Queued: qmgr from=<...>
    Queued --> Delivered: status=sent
    Queued --> Bounced: status=bounced
    Queued --> Deferred: status=deferred
    Deferred --> Deferred: retry fails again
    Deferred --> Delivered: retry succeeds
    Deferred --> Bounced: permanent failure
    Deferred --> Expired: maximal_queue_lifetime reached
    Delivered --> [*]
    Bounced --> [*]
    Expired --> [*]
```

## Requirements

- Linux with **systemd-journald**, and Postfix logging to the journal (the
  Debian/Ubuntu default).
- A **RabbitMQ** queue and an account allowed to publish to it. The collector
  never declares exchanges or queues.
- To build from source: **Go 1.25** or later.

## Quick start

### Option 1: release bundle with systemd (recommended)

```bash
make dist VERSION=1.0.0                      # or download a release bundle
tar xzf dist/mailstatus-collector-1.0.0-linux-amd64.tar.gz
cd mailstatus-collector-1.0.0-linux-amd64
sudo ./install.sh                            # binary, systemd unit, /etc/mailstatus-collector/env
sudo vi /etc/mailstatus-collector/env        # RabbitMQ URL and routing key
sudo systemctl enable --now mailstatus-collector
```

**[docs/INSTALL.md](docs/INSTALL.md)** is the full step-by-step guide, with
the Postfix changes and a check after each step.

### Option 2: build from source

```bash
git clone https://github.com/Yukthi-Systems/Phoenix-MailStatus-Collector.git
cd Phoenix-MailStatus-Collector
make build                                   # ./bin/mailstatus-collector
```

### Try it without a broker

`-dry-run` prints the messages instead of publishing them:

```bash
./bin/mailstatus-collector -stdin -dry-run < testdata/journal-sample.json
```

## Message format

One message per mail and outcome. Example body:

```json
{
  "euid": "f0767ff1-5ba4-4b43-8fad-63dee139795a",
  "message_id": "20261107112219.1@app01.example.com",
  "from_email_id": "sender@example.net",
  "to_email_ids": ["alice@outlook.com"],
  "email_domains": ["example.net", "outlook.com"],
  "type": "Outgoing",
  "status": "Delivered",
  "status_description": "Mail delivered successfully",
  "subject": "Your invoice for November",
  "log_timestamp": "2026-11-07T11:22:23+05:30",
  "email_timestamp": "2026-11-07T11:22:19+05:30",
  "meta_data": {
    "queue_id": "8FA5B3EAE6",
    "error_code": "250",
    "server": "mx1.example.net",
    "event_uids": { "f6ae687e-aff6-5656-8e23-72fafe52f95e": "alice@outlook.com" },
    "mx_responses": [
      {
        "event_uid": "f6ae687e-aff6-5656-8e23-72fafe52f95e",
        "to": "alice@outlook.com",
        "relay": "outlook-com.olc.protection.outlook.com[203.0.113.161]:25",
        "dsn": "2.6.0",
        "response": "250 2.6.0 <…> Queued mail for delivery"
      }
    ]
  }
}
```

| Field | Value |
|---|---|
| `euid` | Value of the reference header (`MAILSTATUS_REFERENCE_HEADER`). Empty if the mail has none. |
| `message_id` | `Message-ID` header without `<>`. Empty when the mail has none. |
| `from_email_id` | Envelope sender |
| `to_email_ids` | Recipients with this outcome: `orig_to` if Postfix rewrote the address, else `to` |
| `email_domains` | Recipient domains and the sender's, lower case, sorted, unique |
| `type` | `MAILSTATUS_LOG_TYPE` (default `Outgoing`) |
| `status` | `Delivered`, `Deferred`, `Bounced` or `Expired` (see below) |
| `status_description` | `Mail delivered successfully`, or Postfix's reason text |
| `subject` | `Subject` header, MIME-decoded; `No Subject` when absent. Postfix logs at most ~190 characters. |
| `log_timestamp` | When Postfix logged the newest result in this message (RFC 3339 with offset, in `MAILSTATUS_TIME_ZONE`) |
| `email_timestamp` | When this server first logged the mail (same format) |
| `meta_data.queue_id` | Postfix queue ID of the delivering hop (unique per server only) |
| `meta_data.error_code` | Remote SMTP reply code; `MAILSTATUS_SUCCESS_CODE` for `Delivered` |
| `meta_data.server` | `MAILSTATUS_NODE` of the delivering server |
| `meta_data.event_uids` | Stable ID of each recipient result → recipient. **De-duplicate on these keys.** |
| `meta_data.mx_responses` | Per recipient: `event_uid`, `to`, `relay`, `dsn` and `response` (the remote reply, or Postfix's own reason when no server answered) |

| `status` | Postfix log | Meaning | Final? | `error_code` |
|---|---|---|---|---|
| `Delivered` | `status=sent` | The receiving server accepted the mail | Yes | `MAILSTATUS_SUCCESS_CODE` |
| `Deferred` | `status=deferred` | Temporary failure; Postfix retries | **No** | Remote reply code, else `451` |
| `Bounced` | `status=bounced` | Permanently rejected | Yes | Remote reply code, else `554` |
| `Expired` | `qmgr: status=expired` | Still failing when `maximal_queue_lifetime` ran out | Yes | Last deferral's code, else `554` |

**AMQP properties:** `content_type=application/json`, persistent delivery,
`mandatory`, `timestamp`, `app_id=mailstatus-collector@<node>`, `message_id` =
`euid` (or a stable generated ID when `euid` is empty), and the header
`type` = `MAILSTATUS_RABBITMQ_HEADER_TYPE` when set.

### Grouping rules

- Results are merged into one message when they belong to the **same mail**
  (queue ID), have the **same status, reason and code**, and arrive in the
  **same batch** (about 1 s). A mail to three Gmail recipients and one unknown
  user gives one `Delivered` message for three recipients and one `Bounced`.
- A recipient can be `Deferred` several times before its final status.
  Consumers should keep the latest status per `euid` and recipient, and never
  let a `Deferred` replace a final status.
- Postfix's own bounce and delay notices (null sender `<>`) and
  address-verification probes are **not** published.

### Content filters (`MAILSTATUS_INTERNAL_RELAYS`)

A content filter such as a DKIM signer receives the mail from Postfix and
re-injects it under a new queue ID. Without help, that hop would look like a
delivery:

```mermaid
sequenceDiagram
    participant P as Postfix
    participant F as Filter (internal relay)
    participant MX as Recipient MX
    participant C as Collector
    P->>F: queue ID A: status=sent ... queued as B
    Note over C: A → B hand-off: not published.<br/>Sender, subject and euid move to B.
    F->>P: re-injected as queue ID B
    P->>MX: queue ID B: status=sent
    C->>C: publish Delivered for B, with A's context
```

List the filter's host name (as it appears in `relay=`) in
`MAILSTATUS_INTERNAL_RELAYS`. If the filter defers or rejects a mail, that
result is still published: the mail never went out.

## Delivery guarantees

```mermaid
sequenceDiagram
    participant J as Journal
    participant C as Collector
    participant R as RabbitMQ
    participant S as state.json
    J->>C: log lines
    C->>C: parse, track, batch (≤ 500 or 1 s)
    C->>R: publish batch (mandatory, persistent)
    R-->>C: confirm all
    C->>S: save cursor + in-flight mail (atomic rename)
    Note over C,S: Crash before the save → the batch is re-sent<br/>(same event_uids). Never lost.
```

| Property | How |
|---|---|
| **No lost results** | The journal position is saved only after the broker confirmed the whole batch. After a crash or restart, reading resumes from the last confirmed line. |
| **At-least-once** | A crash between confirm and save re-sends about one second of results with the same deterministic IDs (UUIDv5 of node, journal cursor and recipient). |
| **Broker outages** | The client reconnects by itself; batches are retried with backoff from 1 s to 30 s. The collector stops reading meanwhile, so the journal holds the backlog. |
| **No silent drops** | Messages are mandatory: if no queue is bound for the routing key, the broker returns them and the batch is retried. |
| **Restart mid-message** | The context of mail still in the queue is saved with the position and reloaded on start. |

## Postfix settings

| Setting | Where | Why |
|---|---|---|
| `/^X-YSPL-ID:/ WARN` and `/^Subject:/ WARN` | [`deploy/postfix/header_checks`](deploy/postfix/header_checks) | Postfix logs only the headers a `header_checks` rule matches. `WARN` just logs. Use your own header name. |
| `header_checks = regexp:/etc/postfix/header_checks` | `main.cf` | Turns the rules on |
| `nested_header_checks =` | `main.cf` | Otherwise headers of attached mails are logged too |
| `always_add_missing_headers = yes` | `main.cf` | Adds a `Message-ID` when the application sends none |
| `enable_long_queue_ids = yes` | `main.cf` | Queue IDs are never reused |
| `LogRateLimitIntervalSec=0` | [`deploy/postfix/50-log-ratelimit.conf`](deploy/postfix/50-log-ratelimit.conf) | journald drops lines from a service that logs too fast; a dropped line is a lost result |

> **Privacy note:** the subject and reference header end up in the journal,
> so anyone who can read the journal can read them.

## Configuration

All settings are environment variables, normally in
`/etc/mailstatus-collector/env` (template:
[`deploy/env.example`](deploy/env.example)).

| Variable | Default | Description |
|---|---|---|
| `MAILSTATUS_RABBITMQ_URL` | — | **Required.** `amqp://user:password@host:5672/vhost` (`amqps://` for TLS) |
| `MAILSTATUS_RABBITMQ_ROUTING_KEY` | — | **Required.** With the default exchange, the queue name |
| `MAILSTATUS_RABBITMQ_EXCHANGE` | empty (default exchange) | Exchange to publish to. Must already exist. |
| `MAILSTATUS_RABBITMQ_HEADER_TYPE` | `fuglu` | AMQP header `type`, for consumers that route on it. Empty: no header. |
| `MAILSTATUS_LOG_TYPE` | `Outgoing` | Value of `type` in the body |
| `MAILSTATUS_TIME_ZONE` | `Asia/Kolkata` | Zone of the timestamps; `UTC` for `+00:00` |
| `MAILSTATUS_REFERENCE_HEADER` | `X-YSPL-ID` | Header that fills `euid`. Must match `header_checks`. |
| `MAILSTATUS_INTERNAL_RELAYS` | — | Comma-separated host names of [content filters](#content-filters-mailstatus_internal_relays) |
| `MAILSTATUS_SUCCESS_CODE` | `255` | `error_code` for `Delivered` (the env template sets `250`) |
| `MAILSTATUS_NODE` | hostname | Name of this server; part of every event ID. **Unique per server; never change it.** |
| `MAILSTATUS_STATE_FILE` | — (unit and image set it) | Journal position and in-flight mail. Required to follow the journal. |
| `MAILSTATUS_BATCH_SIZE` | `500` | Results per batch (upper bound) |
| `MAILSTATUS_FLUSH_INTERVAL` | `1s` | Longest time a result waits before it is published |
| `MAILSTATUS_STATS_INTERVAL` | `5m` | Throughput log line (`0` = off) |
| `MAILSTATUS_STATE_MAX_AGE` | `720h` | Forget queue IDs not seen this long. Longer than `maximal_queue_lifetime`. |
| `MAILSTATUS_POSTFIX_SYSLOG_NAME` | `postfix` | Postfix `syslog_name` (multi-instance setups) |
| `MAILSTATUS_POSTFIX_SERVICES` | `cleanup,qmgr,smtp,lmtp,local,virtual,pipe,error,discard` | Journal identifiers followed |
| `MAILSTATUS_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `MAILSTATUS_LOG_FORMAT` | `json` | `json` or `text` (logfmt) |
| `MAILSTATUS_LOG_FILE` | — (unit sets `/var/log/mailstatus-collector/collector.log`) | Log file; warnings and errors also go to stderr/journal |
| `MAILSTATUS_LOG_MAX_SIZE_MB` | `100` | Rotate at this size |
| `MAILSTATUS_LOG_MAX_FILES` | `10` | Rotated files kept (`0` = no limit) |
| `MAILSTATUS_LOG_MAX_AGE` | `720h` | Remove rotated files older than this (`0` = no limit) |
| `MAILSTATUS_LOG_COMPRESS` | `true` | gzip rotated files |
| `MAILSTATUS_JOURNALCTL` | `journalctl` | Path to the binary |

### Command-line flags

| Flag | Effect |
|---|---|
| *(none)* | Follow the journal live (the systemd service) |
| `-stdin` | Read a `journalctl -o json` export from stdin and publish it (backfill) |
| `-dry-run` | Print the messages as JSON lines instead of publishing; no broker needed |
| `-version` | Print the version and exit |

## Operations

```bash
systemctl status mailstatus-collector
tail -f /var/log/mailstatus-collector/collector.log   # JSON lines; "stats" every 5m
journalctl -u mailstatus-collector -f                 # start/stop, warnings and errors
jq '{cursor, time, in_queue: (.messages | length)}' /var/lib/mailstatus-collector/state.json
```

The `stats` line reports `entries` (journal lines read), `events` (recipient
results), `written` (confirmed by the broker), `commits` and `lag` (how far
behind the newest log line is). The collector rotates its own log file; don't
add a logrotate rule.

| Log message | Meaning or action |
|---|---|
| `output write failed, retrying` | See the `error` field: broker down, wrong credentials, or `could not be routed` (queue or binding missing). Nothing is lost while the journal still has the lines. |
| `rabbitmq returned a message: no queue is bound for it` | Check the exchange, routing key and that the queue exists. |
| `saved journal cursor is no longer valid, resuming by timestamp` | The journal was vacuumed during a long outage; the overlap is sent again. |
| `delivery for a message whose sender was not seen` | The mail was queued before the collector first started. |
| `forgot stale messages` | `removed` lines were never seen. Check the journald rate limit (`journalctl -u systemd-journald \| grep -i suppressed`). |

### Re-publishing a time range

```bash
# as root, on the server whose log it is (same MAILSTATUS_NODE, so IDs match)
( set -a; . /etc/mailstatus-collector/env; set +a
  journalctl -o json --since "2026-11-07 00:00" --until "2026-11-07 12:00" \
    -t postfix/cleanup -t postfix/qmgr -t postfix/smtp -t postfix/error \
  | mailstatus-collector -stdin -dry-run )     # drop -dry-run to publish
```

This does not touch the service's state file. Consumers must de-duplicate on
`event_uids`.

More: [docs/INSTALL.md](docs/INSTALL.md) (installation and troubleshooting) and
[docs/BENCHMARK.md](docs/BENCHMARK.md) (performance).

## Development

```text
.
├── cmd/mailstatus-collector/   main: flags, wiring, signals, logging
├── internal/
│   ├── collector/              batching, commit/retry loop, shutdown
│   ├── config/                 environment variables → Config
│   ├── journal/                journalctl JSON decoding, follow/resume
│   ├── logfile/                size-rotated, gzipped log file writer
│   ├── mailstatus/             Event type, deterministic UUIDv5 IDs
│   ├── postfix/                log line parser, Subject decoding
│   ├── rabbitmq/               message grouping, confirmed publishing
│   ├── sink/                   Batch and the Sink interface
│   ├── statefile/              atomic local state file
│   └── tracker/                per-queue-ID state machine → events
├── deploy/                     systemd unit, env template, Postfix/journald config
├── docs/                       installation guide, benchmark
├── scripts/gen-journal.py      synthetic load generator for benchmarks
└── testdata/                   sample `journalctl -o json` export
```

All packages live under `internal/` and are imported as
`github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/<pkg>`. They
are not a public API and may change between releases.

```bash
make help                 # list targets
make lint test            # gofmt, go vet, unit tests with -race (no broker needed)
make cover                # coverage report
make dist VERSION=1.0.0   # release bundle in dist/

# integration tests against a throw-away broker
MAILSTATUS_TEST_RABBITMQ_URL=amqp://guest:guest@127.0.0.1:5672/ make test-integration
```

Design notes:

- **journalctl instead of sd-journal bindings:** no cgo, no libsystemd, a
  fully static binary; rotation and format changes are journalctl's job.
- **One goroutine for parse/track/publish:** log order is preserved and the
  tracker needs no locks.
- **Fail fast, let systemd restart:** if `journalctl` exits or the RabbitMQ
  client cannot recover, the process exits non-zero and resumes from the
  saved state.

## Roadmap

Ideas we'd welcome help with. Open an issue before starting on one:

- Prometheus metrics endpoint
- Reading `/var/log/mail.log` directly, for hosts without journald
- More output sinks behind the `sink.Sink` interface

## Contributing

Contributions are welcome: bug reports, documentation, tests and code. Please
read [CONTRIBUTING.md](CONTRIBUTING.md) and our
[Code of Conduct](CODE_OF_CONDUCT.md) first.

1. Fork the repository and create a branch: `git checkout -b feat/my-change`
2. Make your change with tests: `make lint test`
3. Commit using [Conventional Commits](https://www.conventionalcommits.org/)
4. Open a pull request; the template will guide you

## Security

Please **do not** report security issues in public GitHub issues. See
[SECURITY.md](SECURITY.md) for how to report them privately.

## Community

- 💬 **Discord:** <a href="https://discord.com/invite/2BS7Z4FhJ" target="_blank" rel="noopener noreferrer">join the Yukthi Systems community</a>
- ✉️ **Email:** <a href="mailto:connect@yukthi.com">connect@yukthi.com</a>
- 🐛 **Bugs and feature requests:** [GitHub Issues](https://github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/issues)

## License

Copyright © 2026 Yukthi Systems Private Limited.

This program is free software: you can redistribute it and/or modify it under
the terms of the **GNU General Public License version 3** as published by the
Free Software Foundation. See [LICENSE](LICENSE) for the full text.
