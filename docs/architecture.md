# Architecture

cloudkit is one Go library that two products import, so a provider fix lands once: dbpantry for
provisioning hosts, and mesobia for its provider drivers.

## Scope

| In cloudkit | In the consumer |
|---|---|
| Provider adapters: server lifecycle, firewall, private network, volumes, snapshots, object storage endpoints, DNS | Approvals, confirmation prompts, audit sinks |
| Capability probes with the real token | Where tokens are stored |
| The generic path: SSH to any host, and a cloud-init renderer | What is installed on the host |
| One error model, one retry policy, one redaction rule | CLI, API, UI and configuration loading |
| A contract test kit every adapter passes | |
| Managed databases: finding them and returning connection details | All database administration, managed engines included |
| REST descriptors: providers without a compiled adapter, reads and writes | |

cloudkit has no database logic. It finds managed database instances (RDS, Cloud SQL, DigitalOcean
databases and the like) and says how to reach them, including a provider-issued short-lived connect
credential where one exists; creating users, databases or pools is the consumer's work, done through
its own engine adapters.

## Rules

| # | Rule |
|---|---|
| C1 | Every outbound call uses the `Transport` the caller passes in; never the standard library's default client, transport or dialer |
| C2 | A nil transport is an error at construction, not a fallback |
| C3 | Each adapter declares its API hosts; any other host is refused before the transport sees it |
| C4 | Redirects are refused unless the adapter lists the target host |
| C5 | Credentials arrive per request and never sit in a struct field, log attribute or error string |
| C6 | Every write is planned first; applying a plan whose live state moved is refused |
| C7 | A destroy plan names everything lost, and applying it needs the resource name typed back |
| C8 | SSH always verifies host keys, and never uses password auth |
| C9 | No global state: no `init()` registration, no package-level clients |
| C10 | `context.Context` first on every call; the logger is injected |

C1 to C7, C9 and C10 are implemented in this release; C8 arrives with the SSH path. A policy test
in `src/internal/policy` scans every Go file for the symbols that would break C1, C4, C8, C9 and
C10, with an exemption list that records the exact count it allows. Three more policy tests read
the package's syntax: every interface method, `*Registry` method and exported function field takes
a `context.Context` first (C10); no package-level client or registry exists (C9); and no exported
name the package has shipped disappears.

## Why the caller owns the transport

mesobia sends every outbound connection through its own egress guard, which resolves, pins and
checks each address. Several provider SDKs fall back to the default HTTP client when handed nil, so
a library that accepted nil, or built its own client, would route around that guard without anyone
noticing. Refusing nil and refusing the default transport makes the bypass impossible to write by
accident.

## Why plans carry a digest

Both consumers show a plan to a person, collect an approval, and apply it later, possibly minutes
later and from another process. The digest is SHA-256 over the provider, the account, the changes,
what a destroy loses, and the live state the plan was built against. `Apply` reads the live state
again through the adapter and recomputes it, so one comparison refuses a plan whose infrastructure
moved, a plan edited after approval (a trimmed `Loses` list, another target), and a plan replayed
against another account. The plan ID is derived from the digest, so an approval recorded against an
ID cannot be reused for different content. `Cost` and `Expires` are outside the digest: the first is
informational, and the second is checked on its own.

A destroy is applied only when the consumer passes back the deleted resources' names exactly as
`Plan.ConfirmText` gives them. Approval levels and step-up remain the consumer's; cloudkit checks
only the typed name, and does so before it opens the provider, so a refusal makes no request.

## Why the client is layered as it is

The client an adapter receives puts the egress guard outermost, then retries, then the token, then
the caller's round tripper. A refused host therefore never reaches the credential source or the
retry loop. The token is added only to requests for the descriptor's API hosts, and never to a
request that follows a redirect, even to an allowed redirect host: a token for the API is not a
token for the provider's object store. Retries never resend a non-idempotent request after an
ambiguous failure, so a create is not sent twice; a 429 is retried because the provider did not act
on it.

## Why it is a library without a CLI framework

Each consumer has its own command tree and configuration loader. A library that brought cobra or a
config package would force them on both, so cloudkit takes plain structs and an injected
`*slog.Logger`, and each consumer maps its own configuration onto them.

## Why descriptors may write

Most providers in the long tail have an HTTP API and no maintained Go SDK. A descriptor maps that
API onto the same capability interfaces as a compiled adapter, reads and writes alike, so a new
provider needs no release. Writes keep every guarantee a compiled adapter has: each one is planned
against the live state, a destroy must declare what it loses and needs the name typed back, paths
outside the declared API hosts are rejected, a descriptor never holds a credential, and write
operations load only from a descriptor that is signed by a trusted key or enabled by the consumer
for that provider and account. Every descriptor passes the same contract suite as a compiled adapter
before it loads.

## Layout

| Path | Holds |
|---|---|
| `src/` | package `cloudkit`: transport, egress policy, secrets, errors, redaction, the registry, capabilities, plans and `Apply`, retries |
| `src/internal/policy/` | repository-wide tests: forbidden symbols, workflow guards |

Planned packages are added when they have code: `providers/` (wave 1: Hetzner, DigitalOcean,
Linode, Vultr; plus `generic` for any SSH host), `ssh/`, `cloudinit/`, `transport/`, `descriptor/`
and `cloudkittest/`.

---

[← Docs index](../README.md#documentation)
