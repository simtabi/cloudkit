package cloudkit

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
)

// authenticator adds the account's token to each request bound for one of the
// descriptor's API hosts, fetching it from the CredentialSource every time, so
// a rotated token is used on the next request and none is kept (C5).
type authenticator struct {
	next      http.RoundTripper
	creds     CredentialSource
	account   string
	hosts     []string // normalised API hosts
	userAgent string
}

// RoundTrip never modifies req; it sends a clone. A request that follows a
// redirect, or goes to any host but an API host, carries no credential, even
// when the redirect target is allowed: a token for the API is not a token for
// the provider's object store or CDN.
func (a *authenticator) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	if a.userAgent != "" {
		out.Header.Set("User-Agent", a.userAgent)
	}
	out.Header.Del("Authorization")
	if req.Response == nil && slices.Contains(a.hosts, hostOf(req)) {
		tok, err := a.creds.Token(req.Context(), a.account)
		if err == nil && tok.IsZero() {
			err = errEmptyToken
		}
		if err != nil {
			if req.Body != nil {
				_ = req.Body.Close() // RoundTrip must close the body, even on error
			}
			return nil, &credentialError{account: a.account, err: err}
		}
		v := tok.Reveal()
		out.Header.Set("Authorization", "Bearer "+string(v))
		clear(v)
	}
	return a.next.RoundTrip(out)
}

// errEmptyToken is the cause when a credential source returns no token: a
// request is never sent unauthenticated instead.
var errEmptyToken = errors.New("cloudkit: the credential source returned an empty token")

// credentialError reports a failed credential source without repeating its
// message: whatever a vault or keychain says about a token stays out of the
// error string, while errors.Is and errors.As still reach the cause.
type credentialError struct {
	account string
	err     error
}

func (e *credentialError) Error() string {
	return fmt.Sprintf("cloudkit: the credential source failed for account %q", e.account)
}

func (e *credentialError) Unwrap() error { return e.err }

// redactedError carries an error whose message has been through [Redact],
// while errors.Is and errors.As still see the original chain.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactErr is how an error from outside the module (a provider, an adapter,
// a credential source) leaves it.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: Redact(err.Error()), err: err}
}
