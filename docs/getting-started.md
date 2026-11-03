# Getting started

A walkthrough of the four things this release provides: the transport, the egress policy, secrets
and the error set.

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

Match them with `errors.Is`. `cloudkit.Redact` strips Authorization-style headers, bearer
credentials, credential query parameters and URL userinfo from a string, and `cloudkit.RedactBody`
also keeps at most 512 bytes of a provider's response body. Wrap any error that may carry a request
URL with them before logging it.

## What comes next

The registry, capability probes, plans with drift refusal, the contract test kit, the SSH path, the
cloud-init renderer and the first provider adapters (Hetzner, DigitalOcean, Linode, Vultr) are
planned; see [Architecture](architecture.md).

---

[← Docs index](../README.md#documentation)
