package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/simtabi/cloudkit/src"
)

func httpTransport(t *testing.T, tr cloudkit.Transport) *http.Transport {
	t.Helper()
	rt, ok := tr.HTTP.(*http.Transport)
	if !ok {
		t.Fatalf("HTTP is %T, want *http.Transport", tr.HTTP)
	}
	return rt
}

func tlsServer(t *testing.T, maxVersion uint16) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	if maxVersion != 0 {
		// The server's own floor is lowered too, so a refused handshake can
		// only come from the client's floor.
		srv.TLS = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: maxVersion} //nolint:gosec // the server side of the TLS floor test
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, tr cloudkit.Transport, u string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.HTTP.RoundTrip(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok" {
		t.Fatalf("body %q, want ok", b)
	}
	return nil
}

// The zero value is usable, passes cloudkit's own validation, and is never
// the standard library's default transport.
func TestZeroOptionsGiveAHardenedTransport(t *testing.T) {
	tr, err := Hardened(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	rt := httpTransport(t, tr)
	if rt.Proxy != nil {
		t.Error("a proxy is set although none was named")
	}
	if rt.TLSClientConfig == nil || rt.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Error("the TLS floor is not 1.2")
	}
	for name, got := range map[string]any{
		"TLS handshake":   rt.TLSHandshakeTimeout,
		"response header": rt.ResponseHeaderTimeout,
		"idle":            rt.IdleConnTimeout,
	} {
		want := map[string]any{"TLS handshake": TLSHandshakeTimeout, "response header": ResponseHeaderTimeout, "idle": IdleConnTimeout}[name]
		if got != want {
			t.Errorf("%s timeout is %v, want %v", name, got, want)
		}
	}
}

// With HTTP_PROXY and HTTPS_PROXY set, a request to a provider's API host
// still gets no proxy. The host is not a loopback address, which the
// standard library's environment proxy would skip on its own.
func TestEnvironmentProxyIsIgnored(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example.test:3128")
	t.Setenv("HTTP_PROXY", "http://proxy.example.test:3128")
	t.Setenv("NO_PROXY", "")
	tr, err := Hardened(Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt := httpTransport(t, tr)
	if rt.Proxy == nil {
		return
	}
	got, err := rt.Proxy(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.hetzner.cloud/v1/servers", nil))
	if err != nil || got != nil {
		t.Fatalf("a request would go through %v (err %v); the environment's proxy must be ignored", got, err)
	}
}

func TestCertificatesAreVerified(t *testing.T) {
	srv := tlsServer(t, 0)
	tr, err := Hardened(Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = get(t, tr, srv.URL)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("a self-signed server was accepted or failed for another reason: %v", err)
	}
}

func TestTLSBelowOnePointTwoIsRefused(t *testing.T) {
	srv := tlsServer(t, tls.VersionTLS11)
	tr, err := Hardened(Options{ExtraRoots: []*x509.Certificate{srv.Certificate()}})
	if err != nil {
		t.Fatal(err)
	}
	err = get(t, tr, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("a TLS 1.1 server was accepted, or refused for another reason: %v", err)
	}
}

// The transport works behind cloudkit's own egress guard, end to end.
func TestBehindTheEgressGuard(t *testing.T) {
	srv := tlsServer(t, 0)
	tr, err := Hardened(Options{ExtraRoots: []*x509.Certificate{srv.Certificate()}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cloudkit.NewHTTPClient(tr, cloudkit.EgressPolicy{APIHosts: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestExplicitProxy(t *testing.T) {
	want, _ := url.Parse("http://proxy.example.test:3128")
	tr, err := Hardened(Options{Proxy: want})
	if err != nil {
		t.Fatal(err)
	}
	rt := httpTransport(t, tr)
	got, err := rt.Proxy(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.test/", nil))
	if err != nil || got.String() != want.String() {
		t.Fatalf("proxy = %v, %v; want %v", got, err, want)
	}
	want.Host = "changed.example.test" // a later change by the caller is not seen
	if got, _ := rt.Proxy(nil); got.Host != "proxy.example.test:3128" {
		t.Fatalf("the transport kept the caller's URL, now %v", got)
	}
	for _, bad := range []string{"socks5://proxy.example.test:1080", "http://", "proxy.example.test:3128"} {
		u, _ := url.Parse(bad)
		if _, err := Hardened(Options{Proxy: u}); !errors.Is(err, cloudkit.ErrInvalidTransport) {
			t.Errorf("proxy %q: err = %v, want ErrInvalidTransport", bad, err)
		}
	}
}

func TestNilExtraRootIsRefused(t *testing.T) {
	if _, err := Hardened(Options{ExtraRoots: []*x509.Certificate{nil}}); !errors.Is(err, cloudkit.ErrInvalidTransport) {
		t.Fatalf("err = %v, want ErrInvalidTransport", err)
	}
}

func TestAllowHostsLimitsSSHDials(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	tr, err := Hardened(Options{AllowHosts: []string{"127.0.0.1", "Bastion.Example.Test."}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := tr.Dial(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("an allowed host was refused: %v", err)
	}
	_ = c.Close()

	_, err = tr.Dial(t.Context(), "tcp", net.JoinHostPort("localhost", port))
	var egress *cloudkit.EgressError
	if !errors.As(err, &egress) || !errors.Is(err, cloudkit.ErrEgressDenied) || egress.Host != "localhost" {
		t.Fatalf("a host outside AllowHosts: err = %v, want an EgressError for localhost", err)
	}

	// Without AllowHosts every host may be dialled.
	open, _ := Hardened(Options{})
	c, err = open.Dial(t.Context(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("dial without AllowHosts: %v", err)
	}
	_ = c.Close()
}

func TestAllowHostsEntriesAreValidated(t *testing.T) {
	if _, err := Hardened(Options{AllowHosts: []string{"::1", "10.0.0.5", "db.example.test"}}); err != nil {
		t.Fatalf("valid entries refused: %v", err)
	}
	for _, bad := range []string{"", "*.example.test", "host.example.test:22", "user@host", "ssh://host"} {
		if _, err := Hardened(Options{AllowHosts: []string{bad}}); !errors.Is(err, cloudkit.ErrInvalidTransport) {
			t.Errorf("entry %q: err = %v, want ErrInvalidTransport", bad, err)
		}
	}
}
