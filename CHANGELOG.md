# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `Transport`, the caller-supplied round tripper and dialer every outbound call uses; a nil field or
  the standard library's default transport is refused with `ErrInvalidTransport`.
- `EgressPolicy` and `NewHTTPClient`: HTTPS only, requests limited to an adapter's declared API
  hosts, redirects refused unless their target is listed, at most three hops.
- `Secret`, which redacts itself in fmt, `log/slog`, JSON and text encoding.
- The error set (`ErrDenied`, `ErrNotFound`, `ErrConflict`, `ErrRateLimited`, `ErrEgressDenied`,
  `ErrUnsupported`, `ErrInvalidTransport`), `EgressError`, `Redact` and `RedactBody`.
- Policy tests: forbidden network, TLS, SSH, global-state and logging symbols; CI and release
  workflow triggers; action pinning.
- CI on pull requests, by dispatch and weekly; a release workflow that runs only on a confirmed
  dispatch against a `v` tag.
