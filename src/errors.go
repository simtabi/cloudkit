package cloudkit

import (
	"errors"
	"fmt"
)

// The errors every adapter returns, matched with errors.Is. Each one leaves the
// module through [Redact], so none carries a credential.
var (
	// ErrInvalidTransport means a Transport was missing a field or used a
	// standard-library default.
	ErrInvalidTransport = errors.New("cloudkit: invalid transport")
	// ErrDenied means the token lacks the scope for the call.
	ErrDenied = errors.New("cloudkit: denied")
	// ErrNotFound means the resource is gone.
	ErrNotFound = errors.New("cloudkit: not found")
	// ErrConflict means a plan's digest moved, or the provider reported a
	// conflicting action.
	ErrConflict = errors.New("cloudkit: conflict")
	// ErrRateLimited means the provider throttled the call after the retry
	// budget was spent.
	ErrRateLimited = errors.New("cloudkit: rate limited")
	// ErrEgressDenied means the egress policy refused a request or a redirect
	// before it reached the transport.
	ErrEgressDenied = errors.New("cloudkit: egress denied")
	// ErrUnsupported means the adapter does not implement the capability.
	ErrUnsupported = errors.New("cloudkit: unsupported")
)

// EgressError names the host an [EgressPolicy] refused. It matches
// [ErrEgressDenied].
type EgressError struct {
	Host     string // the refused host, lower-cased
	Redirect bool   // true when the request was a redirect
	Reason   string // why it was refused
}

func (e *EgressError) Error() string {
	kind := "request"
	if e.Redirect {
		kind = "redirect"
	}
	return fmt.Sprintf("cloudkit: egress denied: %s to %q: %s", kind, e.Host, e.Reason)
}

// Unwrap lets errors.Is match [ErrEgressDenied].
func (e *EgressError) Unwrap() error { return ErrEgressDenied }
