# Contributing to Phoenix MailStatus Collector

Thank you for taking the time to contribute! Bug reports, documentation fixes,
tests and code are all welcome.

By participating you agree to follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

- **Report a bug:** open a [bug report](https://github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/issues/new?template=bug_report.yml).
  Include the version (`mailstatus-collector -version`), your Postfix and
  systemd versions, and the relevant log lines, with addresses removed.
- **Suggest a feature:** open a [feature request](https://github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/issues/new?template=feature_request.yml)
  and describe the problem before the solution.
- **Ask a question:** use <a href="https://discord.com/invite/2BS7Z4FhJ" target="_blank" rel="noopener noreferrer">Discord</a>
  rather than an issue.
- **Report a vulnerability:** never in a public issue. See [SECURITY.md](SECURITY.md).

## Development setup

You need Go 1.25 or later and `make`. A RabbitMQ broker is only needed for the
integration tests.

```bash
git clone https://github.com/<you>/Phoenix-MailStatus-Collector.git
cd Phoenix-MailStatus-Collector
make lint test
./bin/mailstatus-collector -stdin -dry-run < testdata/journal-sample.json   # after make build
```

Integration tests against a throw-away local RabbitMQ broker:

```bash
MAILSTATUS_TEST_RABBITMQ_URL=amqp://guest:guest@127.0.0.1:5672/ make test-integration
```

## Project layout

| Path | Contents |
|---|---|
| `cmd/mailstatus-collector/` | `main`: flags, wiring, signals, logging |
| `internal/<pkg>/` | Implementation packages, imported as `github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/<pkg>` |
| `deploy/` | systemd unit, env template, Postfix/journald snippets |
| `docs/` | Installation guide, benchmark |
| `testdata/` | Sample `journalctl -o json` input |

The [README](README.md#how-it-works) explains the pipeline.

## Pull request workflow

1. **Discuss larger changes first** in an issue, so no effort is wasted.
2. **Fork** and create a branch from `main`: `feat/…`, `fix/…`, `docs/…`.
3. **Make focused commits.** One logical change per pull request.
4. **Add or update tests.** Parser changes need a table-test case with the real
   (anonymised) Postfix log line.
5. **Run `make lint test`** and make sure it passes before you push.
6. **Update the docs** (README, `docs/INSTALL.md`, `deploy/env.example`) if
   behaviour or configuration changes, and add an entry under *Unreleased* in
   [CHANGELOG.md](CHANGELOG.md).
7. **Open the pull request** and fill in the template.

### Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/):

```text
fix(tracker): keep the subject when a content filter re-queues mail

The re-queued mail logs no header lines of its own, so ...

Fixes #123
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`, `chore`.

## Coding guidelines

- Format with `gofmt`; `go vet` must be clean.
- **Every package, type and function has a doc comment**, exported or not,
  written as full sentences that start with the name
  ([Go Doc Comments](https://go.dev/doc/comment)).
- Prefer the standard library. A new dependency needs a reason in the PR.
- Keep the hot path (parse, track) allocation-light; run `BenchmarkPipeline`
  (see [docs/BENCHMARK.md](docs/BENCHMARK.md)) for parser changes.
- Never lose a result: a change to the commit, publish or state-saving path
  must keep the at-least-once guarantee, with a test showing it.
- Use only example data in tests and docs: `example.com`/`.net`/`.org`
  domains and RFC 5737 IP addresses (`192.0.2.0/24`, `198.51.100.0/24`,
  `203.0.113.0/24`). Never real addresses or hostnames.
- New source files start with the GPL-3.0 license header used by the existing files.

## License

By contributing, you agree that your contributions are licensed under the
[GNU General Public License v3.0](LICENSE), the license of this project.
