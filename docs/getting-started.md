# Getting started

A walkthrough of what this release provides: the transport, the egress policy, the registry, plans
and applying them, retries, secrets and the error set.

## The transport

`cloudkit.Transport` is the only way the module reaches the network. It has two required fields:

| Field | Used for |
|---|---|
| `HTTP http.RoundTripper` | every provider API request |
| `Dial func(ctx, network, addr) (net.Conn, error)` | every SSH connection |

`Transport.Validate` fails with `cloudkit.ErrInvalidTransport` when either is nil, or when `HTTP`
is the standard library's default transport. Neither case falls back to a default, because a
fallback would route around your proxy, TLS settings and egress controls.

```go
transport := cloudkit.Transport{
	HTTP: myRoundTripper,
	Dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
}
if err := transport.Validate(); err != nil {
	return err
}
```

## The egress policy

An adapter declares the hosts it talks to. `cloudkit.NewHTTPClient` wraps your round tripper so
nothing else is reachable:

```go
client, err := cloudkit.NewHTTPClient(transport, cloudkit.EgressPolicy{
	APIHosts:      []string{"api.digitalocean.com"},
	RedirectHosts: []string{"objects.example-cdn.test"},
})
```

| Rule | Behaviour |
|---|---|
| Scheme | HTTPS only; `http://` is refused |
| Hosts | exact, case-insensitive matches against `APIHosts`; no wildcards, ports or schemes in the list |
| Redirects | refused unless the target is in `RedirectHosts`, over HTTPS, at most three hops |
| Redirect-only hosts | a host listed only in `RedirectHosts` is reachable by redirect, never directly |
| Refusal | before your round tripper sees the request, as a `*cloudkit.EgressError` matching `cloudkit.ErrEgressDenied` |

## The registry

`cloudkit.Registry` is built by you, from `cloudkit.Options` and one `cloudkit.Factory` per provider.
Nothing registers itself, and there is no package-level registry or client.

```go
reg, err := cloudkit.NewRegistry(cloudkit.Options{
	Transport:   transport,                     // required; nil or the default transport is refused
	Credentials: vaultTokens,                   // required; a cloudkit.CredentialSource
	Logger:      logger,                        // required; slog.New(slog.DiscardHandler) to silence
	Retry:       cloudkit.RetryPolicy{},        // zero value: 3 tries, jittered backoff
	UserAgent:   "dbpantry/1.4.0",
}, hetznerFactory, digitaloceanFactory)
```

`NewRegistry` refuses invalid options, a factory whose descriptor is incomplete, a factory with no
`New` function, and two factories with the same ID. A descriptor ID is lower-case words joined by
hyphens (`hetzner`, `digitalocean`), never empty.

| `Options` field | Required | Notes |
|---|---|---|
| `Transport` | yes | both `HTTP` and `Dial`; see above |
| `Credentials` | yes | `Token(ctx, account)` is called for every request, so a rotated token is used on the next one |
| `Logger` | yes | cloudkit never uses the default logger |
| `Retry` | no | see [Retries](#retries) |
| `UserAgent` | no | sent as `User-Agent` when set |

### Credentials

```go
type vault struct{ store *Vault }

func (v vault) Token(ctx context.Context, account string) (cloudkit.Secret, error) {
	b, err := v.store.Read(ctx, "providers/"+account)
	if err != nil {
		return cloudkit.Secret{}, err
	}
	return cloudkit.NewSecret(b), nil
}
```

An empty token is refused rather than sent, and a failing source is reported without repeating its
message, so nothing the vault says about a token reaches an error string.

### Opening a provider

```go
p, err := reg.Open(ctx, "hetzner", "production")
if err != nil {
	return err // cloudkit.ErrNotFound for an unknown provider
}
caps, err := p.Probe(ctx)
if caps.Allowed(cloudkit.CapServerCreate) {
	// offer "create server"
}
servers, err := p.Inventory(ctx, cloudkit.Filter{Kinds: []string{"server"}})
```

Each `Open` builds a fresh client, so two accounts never share one. The adapter receives a
`cloudkit.Account` whose `HTTP` client:

| Layer, outermost first | Does |
|---|---|
| Egress guard | refuses any host outside the descriptor's `APIHosts`, and redirects outside `RedirectHosts` |
| Retry | the registry's `RetryPolicy` |
| Authenticator | adds `Authorization: Bearer <token>` from the credential source, only for API hosts and never on a redirect |
| Your round tripper | everything else: proxy, TLS, timeouts, your own egress controls |

### Capabilities

`Probe` returns a `cloudkit.CapSet`: each capability it tested and whether the token is `Allowed`
or `Denied`. A capability it did not test reads as `Unknown`, which is not allowed. A read-only
token shows every write as `Denied`. `Capability.IsWrite` says which capabilities change anything;
a name cloudkit does not know counts as a write.

## Plans and applying them

Every write is planned first. An adapter builds a plan with `cloudkit.NewPlan` from the changes,
what a destroy loses, and the live state it read:

```go
plan, err := lifecycle.PlanDestroy(ctx, "srv-4821")
// plan.Changes: delete server web-1
// plan.Loses:   server web-1, volume data-1, ipv4 203.0.113.7
// plan.ConfirmText(): "web-1"
```

| `Plan` field | Holds |
|---|---|
| `ID` | stable, derived from the digest; approvals refer to it |
| `Provider`, `Account` | where it applies; `Apply` opens the same account |
| `Changes` | each create, update or delete, with the resource before and after |
| `Loses` | for a destroy, every resource deleted and everything that goes with it |
| `Cost` | monthly delta, when the provider reports one; not covered by the digest |
| `Digest` | SHA-256 over the provider, account, changes, loses and the live state |
| `Expires` | 15 minutes after the plan was built (`cloudkit.PlanTTL`) |

Show the plan, collect the approval your product requires, then apply it:

```go
result, err := reg.Apply(ctx, plan, cloudkit.Confirmation{Typed: typedByThePerson})
switch {
case errors.Is(err, cloudkit.ErrConfirmationRequired):
	// a destroy, and the name was not typed back exactly
case errors.Is(err, cloudkit.ErrConflict):
	// the live state moved, the plan expired, or it was altered: plan again
}
```

`Apply` refuses before anything changes, in this order:

| Refusal | Error |
|---|---|
| the plan is malformed, was altered after `NewPlan`, or has expired | `ErrConflict` (`*PlanError`) |
| a destroy whose `ConfirmText` was not typed back exactly | `ErrConfirmationRequired` |
| the provider cannot apply plans | `ErrUnsupported` (`*UnsupportedError`) |
| the live state the adapter reads again no longer matches the digest | `ErrConflict` (`*PlanError`) |

`Result` comes back only when the provider reports the work done, not merely accepted.

### Writing the apply side of an adapter

A provider that applies plans implements `cloudkit.Applier`:

```go
type Applier interface {
	LiveState(ctx context.Context, p Plan) ([]Resource, error) // same shape as passed to NewPlan
	Execute(ctx context.Context, p Plan) (Result, error)       // waits for the provider's actions
}
```

`NewPlan` refuses a delete that is not listed in `loses`, or whose resource has no name to type
back.

## Retries

The registry's clients retry by `cloudkit.RetryPolicy`. The zero value is 3 tries, jittered
exponential backoff from 500 ms, and `Retry-After` honoured up to 30 s; `MaxAttempts: 1` turns
retries off.

| Response | Retried for |
|---|---|
| 429 | any request whose body can be replayed; the provider did not act on it |
| 502, 503, 504, or a transport error | idempotent methods only (GET, HEAD, OPTIONS, PUT, DELETE), so a create is never sent twice |
| anything else | not retried |

A 429 that outlasts the budget, or asks to wait longer than `MaxDelay`, ends in a
`*cloudkit.RateLimitError` matching `cloudkit.ErrRateLimited`, with the `RetryAfter` the provider
asked for.

## Secrets

`cloudkit.Secret` holds a credential and prints as `[REDACTED]` in every fmt verb, in `log/slog`,
and in JSON and text encoding:

```go
token := cloudkit.NewSecret(tokenBytes) // copies tokenBytes
req.Header.Set("Authorization", "Bearer "+string(token.Reveal()))
```

`Reveal` returns a copy, for the one place that sends the value.

## Errors

| Error | Meaning |
|---|---|
| `ErrInvalidTransport` | a transport field is missing or a standard-library default |
| `ErrDenied` | the token lacks the scope |
| `ErrNotFound` | the resource is gone |
| `ErrConflict` | a plan's digest moved, or the provider reported a conflicting action |
| `ErrRateLimited` | throttled after the retry budget |
| `ErrEgressDenied` | the egress policy refused a request or redirect |
| `ErrUnsupported` | the adapter does not implement the capability |
| `ErrConfirmationRequired` | a destroy plan was applied without its names typed back |

Match them with `errors.Is`. `cloudkit.Redact` strips Authorization-style headers, bearer
credentials, credential query parameters and URL userinfo from a string, and `cloudkit.RedactBody`
also keeps at most 512 bytes of a provider's response body. Wrap any error that may carry a request
URL with them before logging it.

## What comes next

The contract test kit, the hardened transport, the SSH path, the cloud-init renderer, the first
provider adapters (Hetzner, DigitalOcean, Linode, Vultr), managed database discovery and REST
descriptors are planned; see [Architecture](architecture.md).

---

[← Docs index](../README.md#documentation)
