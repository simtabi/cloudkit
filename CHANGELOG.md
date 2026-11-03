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
- A weekly and on-demand CI leg on macOS and Windows; pull requests still test on Linux only.
- `Options`, `CredentialSource` and `Registry` (`NewRegistry`, `Open`, `Descriptors`): the caller
  builds the registry from one `Factory` per provider; it refuses invalid options, incomplete
  descriptors, empty or non-slug provider IDs and duplicate IDs, and keeps no package-level state.
- `Provider`, `Descriptor`, `TokenModel`, `Module`, `Resource`, `Filter` and `Account`: each `Open`
  gives an adapter a fresh client that is limited to its declared hosts, fetches the account's token
  per request, sends it only to API hosts and never on a redirect, and retries.
- `Capability`, `Access` (`Allowed`, `Denied`, `Unknown`) and `CapSet`, with the capability catalogue;
  an unknown capability counts as a write.
- `Plan`, `Change`, `Money`, `NewPlan` and a SHA-256 digest over the provider, account, changes, what a
  destroy loses and the live state; plans expire after `PlanTTL`.
- `Registry.Apply` with the `Applier` interface: refuses an altered or expired plan and a plan whose
  live state moved (`ErrConflict`, `PlanError`), and a destroy whose resource names were not typed back
  (`ErrConfirmationRequired`, `Confirmation`), before anything is sent.
- `RetryPolicy` and `RateLimitError`: jittered backoff, `Retry-After` honoured up to `MaxDelay`, and no
  retry of a non-idempotent request after an ambiguous failure.
- `UnsupportedError`, matching `ErrUnsupported`.
- Policy tests: context first on every interface and registry method (C10), no package-level client
  or registry (C9), and no shipped exported name removed.

### Changed

- The architecture guide states the scope split: cloudkit finds and connects to managed databases
  but never administers them, and REST descriptors (planned) support writes behind signing or
  explicit enablement, plans and the contract suite.
- The architecture guide explains the plan digest and the client's layering, and the getting-started
  guide covers the registry, plans, `Apply` and retries.
