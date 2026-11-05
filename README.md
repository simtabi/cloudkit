# simtabi/cloudkit

[![CI](https://github.com/simtabi/cloudkit/actions/workflows/ci.yml/badge.svg)](https://github.com/simtabi/cloudkit/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Two badges, not four: no version is tagged yet, so there is no registry version to show, and tests
and static analysis run in the one CI workflow.

> A Go library for VPS and cloud provider APIs and plain SSH hosts, where every request goes through the caller's own transport.

Requires Go 1.27 or later. Shared by `simtabi/dbpantry` and `simtabi/mesobia`.

## Install

No version is tagged yet. Until `v0.1.0`, pin a commit:

```bash
go get github.com/simtabi/cloudkit/src@main
```

The module lives in the repository's `src/` directory, so its path ends in `/src`.

## Quick start guide and usage

### Getting started

Nothing to publish or configure. Build a `cloudkit.Transport` from your own round tripper and
dialer: cloudkit refuses a nil one and the standard library's defaults, so your proxy, TLS and
egress rules apply to every call.

### Usage

Build the HTTP client an adapter uses, limited to the hosts it declares:

```go
transport := cloudkit.Transport{HTTP: myRoundTripper, Dial: myDialer.DialContext}

client, err := cloudkit.NewHTTPClient(transport, cloudkit.EgressPolicy{
	APIHosts: []string{"api.hetzner.cloud"},
})
if err != nil {
	return err
}
// A request to any other host fails with cloudkit.ErrEgressDenied before
// myRoundTripper sees it, and redirects are refused unless listed.
```

Carry a token as a `cloudkit.Secret`, which prints as `[REDACTED]` in fmt, slog and JSON:

```go
token := cloudkit.NewSecret(tokenBytes)
logger.Info("probing provider", "account", "production", "token", token) // token=[REDACTED]
```

Full walkthrough: [docs/getting-started.md](docs/getting-started.md). Everything else is in the
[Documentation](#documentation) index.

## <a name="documentation"></a>Documentation

Hosted at <https://opensource.simtabi.com/documentation/simtabi/cloudkit/>.

### Guides

- [Installation](docs/installation.md) — the module path, Go version and pinning before `v0.1.0`
- [Getting started](docs/getting-started.md) — transports, the registry, plans and applying them, retries, secrets and errors
- [Architecture](docs/architecture.md) — the rules every adapter follows, and why
- [Release](docs/release.md) — how a version is tagged and released

### Reference

- [`cloudkittest`](docs/tools/cloudkittest.md) — the contract suite every adapter passes, the replaying recorder and the OpenAPI validator

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately, as
[SECURITY.md](SECURITY.md) describes; conduct is covered by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

MIT. See [LICENSE](LICENSE).
