package cloudkit

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

// Transport is everything the module may use to reach the network. mesobia
// passes netguard's guarded RoundTripper and dialer; dbpantry passes a hardened
// transport of its own. Both fields are required.
type Transport struct {
	// HTTP carries every provider API request.
	HTTP http.RoundTripper
	// Dial opens every SSH connection.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Validate reports whether t can be used. A nil field is an error, never a
// fallback to the standard library's defaults, and so is handing in the
// standard library's default transport itself, which would route around the
// caller's egress controls just as a nil would.
func (t Transport) Validate() error {
	if err := t.validateHTTP(); err != nil {
		return err
	}
	if t.Dial == nil {
		return fmt.Errorf("%w: Dial is nil", ErrInvalidTransport)
	}
	return nil
}

func (t Transport) validateHTTP() error {
	if t.HTTP == nil {
		return fmt.Errorf("%w: HTTP round tripper is nil", ErrInvalidTransport)
	}
	if t.HTTP == http.DefaultTransport {
		return fmt.Errorf("%w: HTTP is http.DefaultTransport; pass the caller's own round tripper", ErrInvalidTransport)
	}
	return nil
}
