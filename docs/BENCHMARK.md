# Performance benchmark

**Measured:** 2026-10-20, after the parser and JSON-decoding optimisation.

The numbers below cover the parts of the pipeline that don't depend on the
output: reading the journal, decoding, parsing and tracking. The earlier
end-to-end runs used a PostgreSQL output, which has since been removed.
End-to-end figures for RabbitMQ have not been measured yet. Publishing adds one
broker confirm round trip per batch (up to 500 results), so on a local network
the input side stays the limit. Contributions of RabbitMQ end-to-end
measurements are welcome.

## Summary

```mermaid
flowchart LR
    j["journalctl -o json<br/>~2,400 mails/s<br/>(slowest step)"] --> c["collector decode + parse + track<br/>~18,000 mails/s per core"]
    c --> r[(RabbitMQ)]
```

| Volume | Log lines/s | journalctl CPU | Collector CPU |
|---|---|---|---|
| 1,000 mails / minute | ~85 | < 1 % of a core | ~0.2 % |
| 1,000 mails / second | ~5,000 | ~40 % of a core | ~10 % |

- **Memory:** about 15–17 MB RSS at any volume. Results are sent in batches
  and nothing piles up in memory.
- **These are lower bounds.** The tests ran on a small 2-core VM with
  everything on the same two cores.

## Test environment

| | |
|---|---|
| Machine | VM, 2 vCPU Intel Xeon (Skylake), 3.7 GB RAM |
| OS | Debian 13 (trixie) |
| Go | 1.25.3, static build (`make build`, `CGO_ENABLED=0`) |
| Collector settings | defaults: `MAILSTATUS_BATCH_SIZE=500`, `MAILSTATUS_FLUSH_INTERVAL=1s` |

## Test data

Generated with [`scripts/gen-journal.py`](../scripts/gen-journal.py):
**100,000 mails = 500,000 journal entries** (193 MB of `journalctl -o json`).
Each mail has the 5 Postfix lines a typical server logs:

```text
postfix/cleanup  <qid>: warning: header X-YSPL-ID: TestEmail<n> from ...; from=<emailmon@example.com> to=<...>
postfix/cleanup  <qid>: message-id=<...>
postfix/qmgr     <qid>: from=<emailmon@example.com>, size=979, nrcpt=1 (queue active)
postfix/smtp     <qid>: to=<...>, relay=mx.example.org[198.51.100.25]:25, ..., status=sent (250 2.0.0 Ok: ...)
postfix/qmgr     <qid>: removed
```

## Results

| Test | What it measures | Result |
|---|---|---|
| `journalctl -o json`, 200k entries | How fast systemd's journal reader produces JSON (the collector's input) | 16.5 s → **~12,000 lines/s ≈ 2,400 mails/s** (1 core, 100 %) |
| `BenchmarkPipeline` | Decode + parse + track per line, no I/O | **10.8 µs/line** → ~92,000 lines/s per core ≈ 18,000 mails/s |
| Dry run, 100k mails (`-stdin -dry-run`) | Whole pipeline, printing messages instead of publishing | 9.6–11.9 s → **~9,000 mails/s** |

### Optimisation in this version

A CPU profile showed that JSON decoding (into a map of every journal field)
and regular expressions took about 75 % of the collector's CPU. The fix:

- decode only the 4 fields the collector uses (`__CURSOR`,
  `__REALTIME_TIMESTAMP`, `SYSLOG_IDENTIFIER`, `MESSAGE`)
- replace the per-line regular expressions with plain string checks

| | Before | After | Change |
|---|---|---|---|
| CPU per log line | 18.8 µs | 10.8 µs | **−43 %** |
| Memory allocations per line | 52 | 22 | −58 % |
| Bytes allocated per 50k lines | 149 MB | 74 MB | −50 % |

## Conclusions

1. **Volume:** 1,000 mails per minute uses well under 1 % CPU. Even 1,000
   mails per second fits within the measured limits.
2. **Limit:** the ceiling is `journalctl` at about 2,400 mails/s per core on
   this VM. The collector itself is about 8 times faster. If more is ever
   needed, the journal could be read through the sd-journal C API, or from
   `/var/log/mail.log` directly.

## How to reproduce

```bash
make build
scripts/gen-journal.py 100000 > /tmp/bench.json          # 100k mails, 500k lines

# Whole pipeline without a broker
time bin/mailstatus-collector -stdin -dry-run < /tmp/bench.json > /dev/null

# Per-line CPU benchmark with allocations (add -cpuprofile=cpu.prof for a profile)
head -50000 /tmp/bench.json > /tmp/bench-50k.json
MAILSTATUS_BENCH_FILE=/tmp/bench-50k.json go test -run='^$' -bench=Pipeline -benchtime=3x ./internal/collector/

# journalctl's own speed on the server
time journalctl -o json -n 200000 --no-pager > /dev/null

# End to end into a THROW-AWAY RabbitMQ queue
MAILSTATUS_RABBITMQ_URL=amqp://guest:guest@127.0.0.1:5672/ MAILSTATUS_RABBITMQ_ROUTING_KEY=bench \
  bash -c 'time bin/mailstatus-collector -stdin < /tmp/bench.json'
```

**Checking a production server under real load:** set
`MAILSTATUS_STATS_INTERVAL=30s` and watch `lag` in the collector's `stats` log
line during a bulk send. If `lag` stays at a few seconds, the collector is
keeping up.
