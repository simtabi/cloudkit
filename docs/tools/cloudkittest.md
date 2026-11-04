# `cloudkittest`

`cloudkittest` (`github.com/simtabi/cloudkit/src/cloudkittest`) is the contract kit: it runs the nine
contract cases every adapter must pass against any `cloudkit.Provider`, over recordings that have been
checked against the provider's OpenAPI document, with nothing reaching the network.

## Running the suite

`cloudkittest.Run(t, suite)` runs each case as a subtest. `cloudkittest.Check(ctx, suite)` runs them
and returns a `Report` without a `*testing.T`, for code that must refuse an adapter whose recordings
fail, such as the descriptor loader.

```go
func TestHetznerContract(t *testing.T) {
	fsys := os.DirFS("testdata")
	spec, err := cloudkittest.LoadSpec(fsys, "hetzner.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := cloudkittest.LoadRecordings(fsys, "hetzner.recordings.json", spec)
	if err != nil {
		t.Fatal(err)
	}
	cloudkittest.Run(t, cloudkittest.Suite{
		Factory:    hetzner.Factory(),
		Recordings: recs,
		Writes: []cloudkittest.Write{
			{Name: "PlanCreate", Capability: cloudkit.CapServerCreate, Plan: planCreate},
			{Name: "PlanDestroy", Capability: cloudkit.CapServerDestroy, Plan: planDestroy},
		},
		Drift: planCreate,
		Destroy: &cloudkittest.Destroy{Plan: planDestroy, Loses: []cloudkit.Resource{
			{Kind: "server", ID: "42"}, {Kind: "volume", ID: "7"}, {Kind: "ipv4", ID: "9"},
		}},
		CloudInit: &cloudkittest.CloudInit{Render: render, Fields: fields},
	})
}
```

Each case builds its own `cloudkit.Registry` with a token generated for the run, no retries, dialing
refused, and a logger that keeps every record. A case's failure messages never print that token, even
when the adapter leaked it.

## The nine cases

| Case | Passes when |
|---|---|
| `transport-only` | Probe and Inventory reach the injected transport; with a transport that fails every request, every call fails; every write the probe allows has a `Write` in the suite |
| `nil-transport` | `NewRegistry` refuses a nil round tripper and a nil dialer; `Factory.New` refuses an account with a nil client, or a client with no `Transport` (which would use the default one) |
| `egress` | the adapter's client refuses a host outside `APIHosts` before the transport sees it; Probe and Inventory, redirected to an unlisted host, return an error matching `ErrEgressDenied`; the provider describes the same hosts as its factory |
| `read-only-token` | with the read-only cassette, Probe reports every write as `Denied` and every `Write` returns `ErrDenied`, with no write request sent |
| `plan-drift` | `Apply` of the `Drift` plan, after the live state moved, returns `ErrConflict` and sends no write |
| `destroy` | the plan lists every resource in `Destroy.Loses`; `Apply` with nothing or the wrong name typed refuses without a request; with the names typed back it goes through |
| `redaction` | behind a proxy that echoes the request and a provider that echoes the token, no error, log record or panic carries the token, and no URL does |
| `cloud-init` | plain user data renders within `UserDataLimit`; a private key, bearer token or password in any field but `OneTimeToken` is refused; output above the limit is refused |
| `ssh` | under every host-key policy but `accept-new`, a key never seen fails; no policy offers `password` or `keyboard-interactive` |

A read-only adapter (no `Writes`) skips `plan-drift` and `destroy`. `cloud-init` is skipped only when
`UserDataLimit` is 0, and fails when the provider takes user data but the suite has no `CloudInit`.
`ssh` is skipped, with its reason, while `Suite.SSH` is nil. Every cassette a case replays must be
used in full: a recorded request the adapter never sent fails the case, because skipping it (a
live-state read, say) is often the defect.

## Recordings

A recordings file holds named cassettes, each an ordered list of request and response pairs:

```json
{
  "provider": "fake",
  "cassettes": {
    "read-write": [
      {"request": {"method": "GET", "url": "https://api.fake.test/v1/token"},
       "response": {"status": 200, "headers": {"Content-Type": ["application/json"]}, "body": {"write": true}}}
    ],
    "destroy": [
      {"request": {"method": "DELETE", "url": "https://api.fake.test/v1/resources/s1"},
       "synthetic": {"status": 200, "example": "deleted"}}
    ]
  }
}
```

| Cassette | Holds |
|---|---|
| `read-write` | Probe, then Inventory, with a token that may write |
| `read-only` | Probe with a read-only token, then what each `Write` sends before it is refused |
| `drift` | what the `Drift` plan sends, then the live-state read `Apply` makes, returning a different state |
| `destroy` | what the `Destroy` plan sends, then the live-state read and the deletes of applying it |

`ParseRecordings` is the only way to build `Recordings`, and it refuses:

| Refused | Error |
|---|---|
| an `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Auth-Token`, `X-Api-Key` or `Api-Key` header, in a request or a response | `ErrCredentialInRecording` |
| a URL, header value or body that `cloudkit.Redact` would change (a bearer token, a credential query parameter, URL userinfo) | `ErrCredentialInRecording` |
| a request or status the provider's document does not describe | `ErrNotInSpec` |
| a body that breaks its response schema | `ErrSchemaViolation` |
| unknown fields, an interaction with both or neither of `response` and `synthetic` | an error naming the cassette and position |

A `synthetic` entry is generated by `Spec.Synthesize` from the document's examples and marked
`Synthetic`, for a write path with no recording yet. The example is checked against its own
schema, so a wrong example is refused like a wrong recording.

## `Recorder`

`NewRecorder(recs, cassette)` returns an `http.RoundTripper` over one cassette. A request is answered by
the first unused interaction with the same method, URL (scheme, host, path, and query in any order) and
JSON body; each interaction answers once, in order. Any other request returns an
`*UnexpectedRequestError`, matching `ErrUnexpectedRequest`, and is sent nowhere. Headers are not
compared, so the `Authorization` header cloudkit adds is ignored and never stored.

| Method | Returns |
|---|---|
| `Requests()` | every request seen, URLs redacted, refused ones flagged `Unexpected` |
| `Unexpected()` | the requests the cassette did not hold |
| `Unused()` | the interactions nothing asked for |
| `Err()` | every unexpected request as one error, or nil |

Recording new responses from a live provider is the job of a scripted recording run, which arrives with
the first provider adapter.

## The OpenAPI validator

`ParseSpec` reads an OpenAPI 3 document in JSON and compiles every schema a response reaches.
`Spec.ValidateResponse(method, url, status, header, body)` finds the server (scheme, host and base
path), the path template (a literal segment beats a `{param}`), the method and the response (the exact
status, then `2XX`, then `default`), and checks the body.

| Keywords | Treatment |
|---|---|
| `type`, `nullable`, `required`, `properties`, `additionalProperties`, `items`, `enum`, `$ref` (within the document) | checked |
| `description`, `title`, `example`, `default`, `format`, `deprecated`, `readOnly`, `writeOnly` | accepted, not checked |
| anything else (`allOf`, `oneOf`, `pattern`, `minimum`, ...) | the document is refused with `ErrUnsupportedKeyword` |

Refusing an unknown keyword, rather than skipping it, means a response is either checked in full or not
loaded at all. When a provider's document needs a keyword, the validator grows to check it. Only JSON
media types are read, and a response object may not itself be a `$ref`.

## Errors

| Error | Meaning |
|---|---|
| `ErrUnexpectedRequest` | a recorder was sent a request its cassette does not hold |
| `ErrCredentialInRecording` | a recording carries a credential header or value |
| `ErrSchemaViolation` | a body breaks its schema; `*SchemaError` names the JSON path |
| `ErrNotInSpec` | a server, path, method, status or content type the document does not describe |
| `ErrUnsupportedKeyword` | a schema keyword outside the subset |

---

[← Docs index](../../README.md#documentation)
