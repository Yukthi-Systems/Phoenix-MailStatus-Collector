# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- First public, open-source release under the GNU GPL v3.0.
- `internal/sink` package with the `Batch` type and the `Sink` interface.
- Community files: contributing guide, code of conduct, security policy,
  issue and pull request templates.

### Changed

- **Breaking:** the Go module path is now
  `github.com/Yukthi-Systems/Phoenix-MailStatus-Collector`.
- **Breaking:** `MAILSTATUS_RABBITMQ_URL` is required, except with `-dry-run`.
- `INSTALL.md` and `BENCHMARK.md` moved to `docs/`.
- The release bundle now also contains `LICENSE`.

### Removed

- **Breaking:** the PostgreSQL output (`internal/store`, `deploy/schema.sql`,
  `deploy/postgres/`, `DATABASE_URL` and the `PG*` variables). RabbitMQ is now
  the only output. A server that still writes to PostgreSQL must be switched
  to RabbitMQ before upgrading.

## [1.0.1] and [1.0.0]

Internal releases, before the project was open-sourced.

[Unreleased]: https://github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/compare/v1.0.1...HEAD
