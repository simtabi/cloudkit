package cloudkit

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// maxRedirects bounds a redirect chain even when every hop is allowed.
const maxRedirects = 3

// EgressPolicy lists the hosts an adapter may reach. Requests go only to
// APIHosts, over HTTPS. A redirect is refused unless its target is in
// RedirectHosts, and a host listed only there is reachable only by redirect.
type EgressPolicy struct {
	APIHosts      []string // e.g. "api.hetzner.cloud"
	RedirectHosts []string // redirect targets the adapter expects, if any
}

// Validate reports whether every host is a bare, lower-case-able host name:
// no scheme, port, path or wildcard.
func (p EgressPolicy) Validate() error {
	if len(p.APIHosts) == 0 {
		return errors.New("cloudkit: egress policy lists no API hosts")
	}
	for _, h := range slices.Concat(p.APIHosts, p.RedirectHosts) {
		if h == "" || strings.ContainsAny(h, "*/:@ ") {
			return fmt.Errorf("cloudkit: egress policy host %q must be a bare host name", h)
		}
	}
	return nil
}

// NewHTTPClient builds the only kind of http.Client an adapter uses: the
// caller's round tripper behind the host allowlist, with the redirect policy.
// Neither can be turned off by an adapter.
func NewHTTPClient(t Transport, p EgressPolicy) (*http.Client, error) {
	// Only HTTP is needed here; Validate also insists on Dial, which the SSH
	// path checks when it dials.
	if err := t.validateHTTP(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	g := &guard{next: t.HTTP, api: normalise(p.APIHosts), redirect: normalise(p.RedirectHosts)}
	return &http.Client{Transport: g, CheckRedirect: g.checkRedirect}, nil
}

type guard struct {
	next     http.RoundTripper
	api      []string
	redirect []string
}

// RoundTrip refuses any request whose host is not allowed, before the caller's
// transport sees it. http.Client sets req.Response on a request it creates to
// follow a redirect, which is how a redirect-only host is told apart.
func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	host := hostOf(req)
	viaRedirect := req.Response != nil
	if err := g.allow(req, host, viaRedirect); err != nil {
		if req.Body != nil {
			_ = req.Body.Close() // RoundTrip must close the body, even on error
		}
		return nil, err
	}
	return g.next.RoundTrip(req)
}

func (g *guard) allow(req *http.Request, host string, viaRedirect bool) error {
	if req.URL == nil || req.URL.Scheme != "https" {
		return &EgressError{Host: host, Redirect: viaRedirect, Reason: "only https is allowed"}
	}
	if slices.Contains(g.api, host) || (viaRedirect && slices.Contains(g.redirect, host)) {
		return nil
	}
	return &EgressError{Host: host, Redirect: viaRedirect, Reason: "host is not in the adapter's egress policy"}
}

func (g *guard) checkRedirect(req *http.Request, via []*http.Request) error {
	host := hostOf(req)
	if len(via) >= maxRedirects {
		return &EgressError{Host: host, Redirect: true, Reason: fmt.Sprintf("more than %d redirects", maxRedirects)}
	}
	if req.URL.Scheme != "https" {
		return &EgressError{Host: host, Redirect: true, Reason: "only https is allowed"}
	}
	if !slices.Contains(g.redirect, host) {
		return &EgressError{Host: host, Redirect: true, Reason: "redirect target is not listed by the adapter"}
	}
	return nil
}

func hostOf(req *http.Request) string {
	if req.URL == nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(req.URL.Hostname()), ".")
}

func normalise(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, strings.TrimSuffix(strings.ToLower(h), "."))
	}
	return out
}
