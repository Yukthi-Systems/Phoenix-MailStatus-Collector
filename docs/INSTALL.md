# Installing mailstatus-collector on a mail server

These steps set up a Postfix server so that mailstatus-collector publishes
every outgoing delivery result to RabbitMQ.

Run every command as **root** on the mail server, unless a step says
otherwise. After each step, check the **Expected** result before moving on.
The examples use the server `mx1.example.net` and bundle version `1.0.0`;
replace them with yours.

| Step | What |
|---|---|
| 0 | Copy the bundle, check prerequisites |
| 1 | Postfix settings |
| 2 | RabbitMQ: queue and account |
| 3 | Install (`install.sh`) |
| 4 | Settings file |
| 5 | Dry run on today's log |
| 6 | Start the service and send a test mail |
| 7 | Test the main delivery cases |
| 8 | Daily checks |

---

## Step 0: Copy the bundle, check prerequisites

On **your machine**:

```bash
scp dist/mailstatus-collector-1.0.0-linux-amd64.tar.gz root@mx1.example.net:/root/
```

On the **server**:

```bash
cd /root && tar xzf mailstatus-collector-1.0.0-linux-amd64.tar.gz
cd /root/mailstatus-collector-1.0.0-linux-amd64
sha256sum -c SHA256SUMS

systemctl list-units --all 'postfix*'
journalctl -t postfix/smtp -n 3 --no-pager
```

All remaining steps run from `/root/mailstatus-collector-1.0.0-linux-amd64`.

**Expected:**
- `bin/mailstatus-collector: OK`.
- Postfix is `active (running)`. Note the unit name: `postfix.service`
  (Debian 13) or `postfix@-.service` (older releases). Step 1c needs it.
- Postfix log lines are shown, so Postfix logs to the journal.

---

## Step 1: Postfix settings

Make a backup first: `cp -a /etc/postfix /root/postfix-backup-$(date +%F)`.

### 1a. Log the subject and `X-YSPL-ID`

Postfix only logs a header when a `header_checks` rule matches it. `WARN`
writes the log line and changes nothing else.

```bash
postconf header_checks
cat /etc/postfix/header_checks 2>/dev/null
```

- If `header_checks` is empty and the file doesn't exist, install ours:
  ```bash
  cp deploy/postfix/header_checks /etc/postfix/header_checks
  postconf -e 'header_checks = regexp:/etc/postfix/header_checks'
  ```
- If the server already has a `header_checks` file, **don't overwrite it**. Add
  these two lines to the file that `postconf header_checks` names. A `regexp:`
  table stops at the first matching rule, so put them before any rule that
  could also match `Subject:` or `X-YSPL-ID:`:
  ```
  /^X-YSPL-ID:/    WARN
  /^Subject:/      WARN
  ```
  For a `pcre:` table, the same lines work.

### 1b. Other `main.cf` settings

```bash
postconf -e 'nested_header_checks ='
postconf -e 'always_add_missing_headers = yes'
postconf -e 'enable_long_queue_ids = yes'
```

| Setting | Why |
|---|---|
| `nested_header_checks =` | Otherwise the headers of attached mails are logged too, and could replace the real subject |
| `always_add_missing_headers` | Adds a `Message-ID` when the app sends none. Some receivers (Outlook, Gmail) distrust mail without one. |
| `enable_long_queue_ids` | Queue IDs are never reused, so log lines can't be matched to an older mail |

Don't apply the "Only when Postfix takes over delivery from another MTA" part of
`deploy/postfix/main.cf.snippet` on a server that already delivers through
Postfix: it changes routing.

### 1c. Never drop Postfix log lines

journald discards lines from a service that logs more than about 10,000 lines
in 30 seconds. A dropped line is a lost result, so turn the limit off for
Postfix only. Use the unit name from Step 0:

```bash
# Debian 13: postfix.service.d      Older releases: postfix@.service.d
mkdir -p /etc/systemd/system/postfix.service.d
cp deploy/postfix/50-log-ratelimit.conf /etc/systemd/system/postfix.service.d/
systemctl daemon-reload
```

### 1d. Apply

Restarting stops Postfix for about a second. Queued mail stays on disk.

```bash
postfix check && systemctl restart postfix && postfix status
postconf header_checks nested_header_checks always_add_missing_headers enable_long_queue_ids
systemctl show postfix -p LogRateLimitIntervalUSec
```

Send any mail through the server, then:

```bash
journalctl -t postfix/cleanup -n 20 --no-pager | grep 'warning: header'
```

**Expected:**
- `postfix check` prints nothing, and Postfix reports `running`.
- `nested_header_checks =` is empty, and the other values match.
- `LogRateLimitIntervalUSec=0`.
- For the test mail, one `warning: header Subject: …` line and one
  `warning: header X-YSPL-ID: …` line (if the app set the header).

---

## Step 2: RabbitMQ: queue and account

The collector publishes, but never creates exchanges or queues. On the broker
(or ask its administrator):

1. A **durable queue** for the results, for example `mail_status_log`. With
   the default exchange, the collector's routing key is this queue name. With
   your own exchange, bind the queue to it with the routing key you will use.
2. An **account** for each mail server, allowed to publish (write) to that
   exchange (the default exchange: write on the queue).

From the mail server, check the broker can be reached:

```bash
timeout 5 bash -c '</dev/tcp/RABBITMQ_HOST/5672' && echo reachable
```

**Expected:** `reachable`. Otherwise open port 5672 (5671 for TLS) from this
server to the broker.

---

## Step 3: Install

```bash
sudo ./install.sh
```

It checks the bundle's checksum and CPU type, creates the
`mailstatus-collector` system user (the unit adds it to the `systemd-journal`
group, so it can read the journal), and installs:

| File | |
|---|---|
| `/usr/local/bin/mailstatus-collector` | The binary |
| `/etc/systemd/system/mailstatus-collector.service` | The unit; creates `/var/lib/mailstatus-collector` and `/var/log/mailstatus-collector` |
| `/etc/mailstatus-collector/env` | Settings, from `deploy/env.example`, first install only |
| `/usr/local/share/doc/mailstatus-collector/` | These docs, the license and the env template |

It does nothing with Postfix (no changes, no checks; that is Step 1, by hand),
never changes RabbitMQ, and doesn't start the service on a
first install.

**Expected:** `installing mailstatus-collector 1.0.0`, then the next steps.
`mailstatus-collector -version` prints `mailstatus-collector 1.0.0`.

---

## Step 4: Settings file

```bash
vi /etc/mailstatus-collector/env
grep -v '^#' /etc/mailstatus-collector/env | grep .
```

Set the two `CHANGE_ME` values:

```
MAILSTATUS_RABBITMQ_URL=amqp://mailstatus:<password>@rabbitmq.example.net:5672/<vhost>
MAILSTATUS_RABBITMQ_ROUTING_KEY=mail_status_log
MAILSTATUS_REFERENCE_HEADER=X-YSPL-ID
MAILSTATUS_INTERNAL_RELAYS=dkim-filter.example.net   # only with a content filter
MAILSTATUS_SUCCESS_CODE=250
```

- **URL:** `amqps://` for TLS. Characters like `@`, `:` or `/` in the password
  must be URL-encoded (`%40`, `%3A`, `%2F`). The default vhost `/` is written
  `%2F`. The file is readable by root only.
- **`MAILSTATUS_INTERNAL_RELAYS`:** the host name of every next hop that gives
  the mail back to this Postfix, as it appears in `relay=`. Check with
  `journalctl -t postfix/smtp -n 50 | grep -o 'relay=[^,]*' | sort | uniq -c`.
  Leave it unset on a server without such a filter.
- **`MAILSTATUS_NODE`** defaults to the hostname. It must be unique per server
  and never change, so leave it unset unless the hostname is wrong.

**Expected:** no `CHANGE_ME` left: `grep -c CHANGE_ME /etc/mailstatus-collector/env` prints `0`.

---

## Step 5: Dry run on today's log

This prints the messages that would be published, and publishes nothing:

```bash
( set -a; . /etc/mailstatus-collector/env; set +a
  journalctl -o json --since today -t postfix/cleanup -t postfix/qmgr -t postfix/smtp -t postfix/error \
  | mailstatus-collector -stdin -dry-run ) | tail -n 5
```

**Expected:** JSON lines like:

```json
{"euid":"f0767ff1-…","from_email_id":"sender@…","to_email_ids":["alice@outlook.com"],"type":"Outgoing","status":"Delivered","subject":"Test Email …","meta_data":{"queue_id":"8FA5B3EAE6",…}}
```

- Mail sent after Step 1: `euid` and `subject` are filled.
- With a content filter: no `"status":"Delivered"` message names the filter in
  `mx_responses[].relay`. If one does, `MAILSTATUS_INTERNAL_RELAYS` doesn't
  match the `relay=` host name.
- Mail sent before Step 1 has no subject or `euid`. That's expected.

---

## Step 6: Start the service and send a test mail

```bash
systemctl enable --now mailstatus-collector
systemctl status mailstatus-collector --no-pager
tail -n 20 /var/log/mailstatus-collector/collector.log
```

**Expected:**
- `active (running)`.
- `"msg":"resuming"` (`"cursor":""` on the first start), `"msg":"started"`
  with `"mode":"live"`, **`"output":"rabbitmq"`** and your host and
  `routing_key`, then `"msg":"starting journalctl"`. If it stops with
  `MAILSTATUS_RABBITMQ_URL is required`, fix the settings file (Step 4) and
  restart.

The first start reads only **new** log lines. Mail already in the queue is
published with an empty sender, subject and `euid`.

Now **send a test mail with an `X-YSPL-ID` header** from your application, wait
about 10 seconds, and check:

```bash
ls -l /var/lib/mailstatus-collector/state.json
grep -c '"level":"ERROR"' /var/log/mailstatus-collector/collector.log
grep '"msg":"published"' /var/log/mailstatus-collector/collector.log | tail -n 3
```

On the broker, the queue has one new message (management UI, **Get messages**
with *Ack mode: Nack message requeue true*, or
`rabbitmqctl list_queues name messages`).

**Expected:**
- The state file exists and was just modified.
- `0` errors.
- A `"msg":"published"` line with your `euid` and the queue name as `routing_key`.
- One message for the mail: `"status":"Delivered"`, your `euid` and subject,
  and `meta_data.queue_id` of the outgoing hop.

If the log shows `output write failed, retrying`, see
[Troubleshooting](#troubleshooting).

---

## Step 7: Test the main delivery cases

Send each of these with an `X-YSPL-ID` header, wait about 10 seconds, then
check the queue:

| Send to | Expected message |
|---|---|
| A real address | `Delivered`, `error_code` `250` |
| A made-up address at a real domain, e.g. `nobody-xyz-123@gmail.com` | `Bounced`, `550` |
| A domain that doesn't exist, e.g. `test@no-such-domain-xyz.com` | `Bounced`, or `Deferred` (`451`) and later `Expired` |
| One mail to 2 Gmail addresses and 1 made-up one | One `Delivered` with 2 recipients, one `Bounced` with 1 |

Restart test:

```bash
systemctl stop mailstatus-collector
# send 2 mails now
systemctl start mailstatus-collector
```

**Expected:** both mails are published after the start. At most the last
second before the stop can arrive twice; consumers drop it by
`meta_data.event_uids`.

Broker outage test (optional, on a test broker): stop RabbitMQ, send a mail,
start RabbitMQ.

**Expected:** `output write failed, retrying` while it is down, then
`output write recovered` and the message arrives. Nothing is lost.

---

## Step 8: Daily checks

```bash
systemctl is-active mailstatus-collector postfix
journalctl -u mailstatus-collector --since today -p warning --no-pager
journalctl -u systemd-journald | grep -i suppressed
grep '"msg":"stats"' /var/log/mailstatus-collector/collector.log | tail -n 1
```

**Expected:** two `active` lines, no output from the next two commands, and a
`stats` line with `"lag"` of a few seconds while mail is flowing. The state
file's time (`ls -l /var/lib/mailstatus-collector/state.json`) should be
recent while mail is flowing; an old time means the collector is stuck or down.

---

## Upgrading

Installing a newer bundle:

```bash
cd /root/mailstatus-collector-<new version>-linux-amd64
sudo ./install.sh        # keeps /etc/mailstatus-collector/env, restarts the service
```

Release notes: [CHANGELOG.md](../CHANGELOG.md).

---

## Troubleshooting

| Symptom | Check / fix |
|---|---|
| `invalid configuration … MAILSTATUS_RABBITMQ_ROUTING_KEY` | Set it (Step 4). |
| `stopped … connect to rabbitmq: … connection refused` | Broker down, wrong host/port, or a firewall. Step 2's check. systemd retries every 5 s. |
| `… Exception (403) Reason: "username or password not allowed"` | Wrong account or password, or the vhost in the URL is wrong (the default vhost is `%2F`). |
| `output write failed, retrying … could not be routed` | No queue is bound for the routing key. Check the queue name and `MAILSTATUS_RABBITMQ_EXCHANGE`. Nothing is lost; it catches up once the queue exists. |
| `output write failed, retrying … ACCESS_REFUSED` | The account may not write to the exchange. |
| `MAILSTATUS_RABBITMQ_URL is required` | Set it in `/etc/mailstatus-collector/env` (Step 4). |
| `MAILSTATUS_STATE_FILE must be set` | The collector was started by hand outside systemd. Use the service, or set `MAILSTATUS_STATE_FILE` to a path the user can write. |
| `read state … invalid character` | The state file is damaged. Move it away and restart; the collector then reads from "now". |
| Service won't start, `permission denied` reading the journal | `getent group systemd-journal` must exist, and the unit must have `SupplementaryGroups=systemd-journal`. |
| Messages have no `euid` or `subject` | Step 1a: `journalctl -t postfix/cleanup -n 20 \| grep 'warning: header'` must show both headers, and `MAILSTATUS_REFERENCE_HEADER` must be `X-YSPL-ID`. Mail queued before the collector started has neither. |
| A `Delivered` message for the content-filter hop, and a second one for the real delivery | `MAILSTATUS_INTERNAL_RELAYS` is missing or doesn't match the host name in `relay=`. |
| `delivery for a message whose sender was not seen` | Normal on the first start for mail already in the queue. |
| `Assertion failed on job for postfix@-.service` | This server runs `postfix.service`. Put the Step 1c file in `postfix.service.d/`. |
| No messages at all, collector running | `journalctl -t postfix/smtp -f` while sending: if no lines appear, Postfix isn't logging to the journal. |

More detail: [README.md](../README.md) (message format, configuration,
operations) and [BENCHMARK.md](BENCHMARK.md) (performance). Still stuck? Ask on
<a href="https://discord.com/invite/2BS7Z4FhJ" target="_blank" rel="noopener noreferrer">Discord</a>
or open an [issue](https://github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/issues).
