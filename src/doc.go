// Package cloudkit talks to VPS and cloud provider APIs, and to plain hosts
// over SSH, on behalf of a caller that owns the network.
//
// Every outbound call goes through the [Transport] the caller passes in; the
// package never reaches for the standard library's default client, default
// transport or dialer, and a missing transport is an error rather than a
// fallback. Requests are limited to the hosts an adapter declares
// ([EgressPolicy]), redirects are refused unless their target is listed, and
// credentials travel as [Secret] values that redact themselves in fmt,
// log/slog and JSON.
//
// This release carries the contracts that need no provider. The registry,
// plans, provider adapters, the SSH path and the cloud-init renderer follow.
package cloudkit
