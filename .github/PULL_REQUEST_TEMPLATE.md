<!--
Thanks for contributing! Please read CONTRIBUTING.md first.
Use a Conventional Commit style title, e.g. "fix(tracker): keep subject after re-queue".
-->

## Summary

<!-- What does this change, and why? -->

Fixes #<!-- issue number -->

## Type of change

- [ ] 🐛 Bug fix (non-breaking change that fixes an issue)
- [ ] ✨ New feature (non-breaking change that adds functionality)
- [ ] 💥 Breaking change (message format, configuration or behaviour changes for existing users)
- [ ] 📝 Documentation only
- [ ] 🔧 Refactoring, tests or build

## How was this tested?

<!-- Commands you ran, journal excerpts used with -stdin -dry-run, broker tests, ... -->

- [ ] `make lint test` passes
- [ ] Integration tests against a broker (`make test-integration`), if publishing changed

## Checklist

- [ ] New and changed code has doc comments and tests
- [ ] Docs updated (README, `docs/INSTALL.md`, `deploy/env.example`) if behaviour or settings changed
- [ ] Entry added under *Unreleased* in `CHANGELOG.md`
- [ ] No real email addresses, hostnames, IPs or secrets in code, tests or docs
- [ ] The at-least-once delivery guarantee still holds

## Notes for reviewers

<!-- Anything that needs special attention, follow-up work, screenshots of messages, ... -->
