#!/usr/bin/env python3
"""Generate a synthetic `journalctl -o json` export for benchmarks.

Each mail produces the 5 Postfix lines a typical Postfix server logs:
cleanup (reference header), cleanup (message-id), qmgr (queued),
smtp (status=sent), qmgr (removed).

    scripts/gen-journal.py 100000 > /tmp/bench.json     # 100k mails = 500k lines
"""
import json
import sys

mails = int(sys.argv[1]) if len(sys.argv) > 1 else 100_000
base_usec = 1_760_860_800_000_000
n = 0
out = sys.stdout


def entry(ident, msg):
    global n
    out.write(json.dumps({
        "__CURSOR": f"s=0f1e;i={n:x};b=9a8b;m={n:x};t=6418;x=77aa",
        "__REALTIME_TIMESTAMP": str(base_usec + n * 1000),
        "PRIORITY": "6",
        "SYSLOG_FACILITY": "2",
        "SYSLOG_IDENTIFIER": ident,
        "_PID": "900",
        "_HOSTNAME": "mx1.example.net",
        "_SYSTEMD_UNIT": "postfix@-.service",
        "MESSAGE": msg,
    }) + "\n")
    n += 1


for i in range(mails):
    q = f"B{i:09X}"
    rcpt = f"user{i}@example.org"
    entry("postfix/cleanup", f"{q}: warning: header X-YSPL-ID: TestEmail{i} from app01.example.com[192.0.2.10]; from=<emailmon@example.com> to=<{rcpt}> proto=ESMTP helo=<app01.example.com>")
    entry("postfix/cleanup", f"{q}: message-id=<{i}@app>")
    entry("postfix/qmgr", f"{q}: from=<emailmon@example.com>, size=979, nrcpt=1 (queue active)")
    entry("postfix/smtp", f"{q}: to=<{rcpt}>, relay=mx.example.org[198.51.100.25]:25, delay=1.8, delays=0.14/0.02/0.7/0.92, dsn=2.0.0, status=sent (250 2.0.0 Ok: queued as 58B0C1F58F)")
    entry("postfix/qmgr", f"{q}: removed")
