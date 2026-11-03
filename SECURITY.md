# Security policy

Where this file is silent, the [Simtabi security policy](https://github.com/simtabi/.github/blob/HEAD/SECURITY.md) applies.

## Reporting a vulnerability

The preferred channel is GitHub private vulnerability reporting: open a private report at
<https://github.com/simtabi/cloudkit/security/advisories/new>. The report stays attached to the
repository, with a draft advisory and a CVE request path. Email **security@simtabi.com** if you do
not use GitHub.

Do **not** open a public issue for a security report.

Include, where possible: a description of the issue, the affected version or commit, reproduction
steps, and any suggested remediation. We aim to acknowledge reports within a few business days.

## Supported versions

No version is tagged yet. Until `v0.1.0`, fixes land on `main` only.

## Scope notes for this project

cloudkit holds provider credentials and makes network calls on behalf of the products that import
it. These invariants are security-load-bearing; please report any way around them:

- **Egress.** A request to a host outside an adapter's `EgressPolicy`, or a redirect to an unlisted
  host, is refused before the caller's transport sees it. Any way to reach another host through the
  module is in scope.
- **No default network.** The module never uses the standard library's default HTTP client,
  transport or dialer, and refuses a nil transport.
- **Credentials.** A `Secret` must never print its value through fmt, `log/slog`, JSON or text
  encoding, and `Redact` must remove credentials from errors. A leak through any path is in scope.
- **TLS and SSH.** Certificate verification and SSH host-key checks are never disabled.
