// Package transport provides the module's one ready-made [cloudkit.Transport]:
// a hardened round tripper and dialer for a caller that has no egress layer of
// its own. It never returns the standard library's default transport.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/simtabi/cloudkit/src"
)

// The timeouts every hardened transport uses.
const (
	DialTimeout           = 10 * time.Second
	TLSHandshakeTimeout   = 10 * time.Second
	ResponseHeaderTimeout = 30 * time.Second
	IdleConnTimeout       = 90 * time.Second
)

// Options configures [Hardened]. The zero value is valid: no proxy, the
// system's roots, and SSH dials to any host.
type Options struct {
	// Proxy is the one proxy requests go through, or nil for none. The
	// environment's HTTP_PROXY and HTTPS_PROXY are never read.
	Proxy *url.URL
	// ExtraRoots are trusted in addition to the system's roots, for a
	// provider or proxy behind a private certificate authority.
	ExtraRoots []*x509.Certificate
	// AllowHosts, when set, limits SSH dials to these hosts (names or IP
	// literals). Provider API requests are limited separately, by each
	// adapter's egress policy.
	AllowHosts []string
}

// Hardened returns a Transport with no proxy unless one is named, TLS 1.2 or
// newer verified against the system's roots plus ExtraRoots, and fixed dial,
// handshake, response-header and idle timeouts. There is no option that turns
// certificate verification off.
func Hardened(o Options) (cloudkit.Transport, error) {
	proxy, err := proxyFunc(o.Proxy)
	if err != nil {
		return cloudkit.Transport{}, err
	}
	roots, err := rootPool(o.ExtraRoots)
	if err != nil {
		return cloudkit.Transport{}, err
	}
	allow, err := allowList(o.AllowHosts)
	if err != nil {
		return cloudkit.Transport{}, err
	}
	dialer := &net.Dialer{Timeout: DialTimeout, KeepAlive: 30 * time.Second}
	rt := &http.Transport{
		Proxy:       proxy,
		DialContext: dialer.DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
		TLSHandshakeTimeout:   TLSHandshakeTimeout,
		ResponseHeaderTimeout: ResponseHeaderTimeout,
		IdleConnTimeout:       IdleConnTimeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          100,
		ForceAttemptHTTP2:     true,
	}
	return cloudkit.Transport{HTTP: rt, Dial: sshDial(dialer, allow)}, nil
}

func proxyFunc(u *url.URL) (func(*http.Request) (*url.URL, error), error) {
	if u == nil {
		return nil, nil
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: proxy must be an http or https URL with a host", cloudkit.ErrInvalidTransport)
	}
	fixed := *u
	return func(*http.Request) (*url.URL, error) { return &fixed, nil }, nil
}

// rootPool returns nil, which means the system's roots, unless extra roots
// are given; then it is the system's roots plus those.
func rootPool(extra []*x509.Certificate) (*x509.CertPool, error) {
	if len(extra) == 0 {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("transport: reading the system's roots: %w", err)
	}
	for _, c := range extra {
		if c == nil {
			return nil, fmt.Errorf("%w: ExtraRoots holds a nil certificate", cloudkit.ErrInvalidTransport)
		}
		pool.AddCert(c)
	}
	return pool, nil
}

func allowList(hosts []string) ([]string, error) {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		n := normalise(h)
		if n == "" || (net.ParseIP(n) == nil && strings.ContainsAny(n, "*/:@ ")) {
			return nil, fmt.Errorf("%w: AllowHosts entry %q must be a bare host name or IP address", cloudkit.ErrInvalidTransport, h)
		}
		out = append(out, n)
	}
	return out, nil
}

// sshDial dials through d, refusing any host outside allow when it is set.
func sshDial(d *net.Dialer, allow []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if len(allow) == 0 {
		return d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("transport: dial %q: %w", addr, err)
		}
		host = normalise(host)
		if !slices.Contains(allow, host) {
			return nil, &cloudkit.EgressError{Host: host, Reason: "SSH host is not in the transport's AllowHosts"}
		}
		return d.DialContext(ctx, network, addr)
	}
}

func normalise(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}
