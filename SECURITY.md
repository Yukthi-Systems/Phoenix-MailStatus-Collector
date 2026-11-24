# Security policy

## Supported versions

Security fixes are made for the latest release. Please upgrade to it before
reporting an issue.

| Version | Supported |
|---|---|
| Latest release | ✅ |
| Older releases | ❌ |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security problems.**

Report them privately, either:

- through GitHub's [private vulnerability reporting](https://github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/security/advisories/new), or
- by email to <a href="mailto:connect@yukthi.com">connect@yukthi.com</a> with the subject `[SECURITY] Phoenix-MailStatus-Collector`.

Please include:

- the affected version (`mailstatus-collector -version`) and your OS / systemd version
- a description of the issue and its impact
- steps to reproduce, or a proof of concept
- any suggested fix

We will acknowledge your report within **3 working days** and keep you
updated. Once a fix is released we will publish an advisory, crediting you
unless you prefer to stay anonymous.

## Scope and hardening notes

The collector handles mail metadata: addresses, subjects and reference IDs.
Deployments should:

- keep `/etc/mailstatus-collector/env` (it holds the broker password) at mode
  `0600`, owned by root;
- use `amqps://` when the broker is not on a trusted network;
- give the collector's RabbitMQ account write access to its exchange only;
- remember that anyone who can read the systemd journal, or the state file
  in `/var/lib/mailstatus-collector`, can read subjects and addresses.
