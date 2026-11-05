package cloudkit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
)

// fakeRT records every request it sees and answers from respond.
type fakeRT struct {
	seen    []string
	respond func(*http.Request) *http.Response
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.seen = append(f.seen, req.URL.String())
	if f.respond != nil {
		return f.respond(req), nil
	}
	return answer(req, http.StatusOK, ""), nil
}

func answer(req *http.Request, code int, location string) *http.Response {
	h := http.Header{}
	if location != "" {
		h.Set("Location", location)
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req}
}

func dial(context.Context, string, string) (net.Conn, error) { return nil, errors.New("not used") }

func TestTransportValidate(t *testing.T) {
	cases := []struct {
		name string
		tr   Transport
		ok   bool
	}{
		{"both set", Transport{HTTP: &fakeRT{}, Dial: dial}, true},
		{"nil HTTP", Transport{Dial: dial}, false},
		{"default transport", Transport{HTTP: http.DefaultTransport, Dial: dial}, false},
		{"nil Dial", Transport{HTTP: &fakeRT{}}, false},
	}
	for _, c := range cases {
		err := c.tr.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidTransport) {
			t.Errorf("%s: got %v, want ErrInvalidTransport", c.name, err)
		}
	}
}

func TestSecretNeverPrints(t *testing.T) {
	value := rand.Text()
	s := NewSecret([]byte(value))

	var out []string
	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q", "%x", "%X", "%d"} {
		out = append(out, fmt.Sprintf(verb, s), fmt.Sprintf(verb, struct{ Token Secret }{s}))
	}
	// Format shadows String and GoString inside fmt, so call them directly too.
	out = append(out, s.String(), s.GoString())
	if txt, err := s.MarshalText(); err == nil {
		out = append(out, string(txt))
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("call", "token", s)
	out = append(out, logged.String())
	j, err := json.Marshal(struct{ Token Secret }{s})
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, string(j))

	for _, o := range out {
		if strings.Contains(o, value) {
			t.Fatalf("secret value leaked: %q", o)
		}
		if !strings.Contains(o, redacted) {
			t.Errorf("output does not show %s: %q", redacted, o)
		}
	}
	if string(s.Reveal()) != value {
		t.Error("Reveal does not return the value")
	}
	if s.IsZero() || !(Secret{}).IsZero() {
		t.Error("IsZero is wrong")
	}
}

func TestSecretCopiesItsInput(t *testing.T) {
	b := []byte(rand.Text())
	want := string(b)
	s := NewSecret(b)
	clear(b)
	r := s.Reveal()
	if string(r) != want {
		t.Fatal("clearing the input changed the secret")
	}
	clear(r)
	if string(s.Reveal()) != want {
		t.Fatal("clearing a revealed copy changed the secret")
	}
}

func TestRedact(t *testing.T) {
	v := rand.Text()
	cases := []string{
		"Authorization: Bearer " + v,
		"authorization=" + v,
		"X-Auth-Token: " + v,
		"request failed: bearer " + v + " rejected",
		"GET https://api.example.test/v1/servers?page=2&token=" + v + "&per_page=50",
		"https://api.example.test/upload?X-Amz-Signature=" + v,
		"dial https://user:" + v + "@api.example.test/v1",
	}
	for _, c := range cases {
		got := Redact(c)
		if strings.Contains(got, v) {
			t.Errorf("Redact(%q) = %q still holds the credential", c, got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("Redact(%q) = %q does not mark the redaction", c, got)
		}
	}
	plain := "GET https://api.example.test/v1/servers?page=2: 404 not found"
	if got := Redact(plain); got != plain {
		t.Errorf("Redact changed text with no credential: %q", got)
	}
}

func TestRedactBodyIsBounded(t *testing.T) {
	body := strings.Repeat("é", MaxBodyBytes) // two bytes per rune
	got := RedactBody([]byte(body))
	if len(got) > MaxBodyBytes+len("…") {
		t.Fatalf("RedactBody kept %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "…") || !strings.HasPrefix(body, strings.TrimSuffix(got, "…")) {
		t.Fatal("RedactBody did not cut on a rune boundary")
	}
	if got := RedactBody([]byte("short")); got != "short" {
		t.Fatalf("short body changed: %q", got)
	}
}

func policy() EgressPolicy {
	return EgressPolicy{APIHosts: []string{"api.example.test"}, RedirectHosts: []string{"objects.example.test"}}
}

func TestNewHTTPClientRefusesBadInput(t *testing.T) {
	if _, err := NewHTTPClient(Transport{}, policy()); !errors.Is(err, ErrInvalidTransport) {
		t.Errorf("nil transport: %v", err)
	}
	if _, err := NewHTTPClient(Transport{HTTP: http.DefaultTransport}, policy()); !errors.Is(err, ErrInvalidTransport) {
		t.Errorf("default transport: %v", err)
	}
	for _, p := range []EgressPolicy{
		{},
		{APIHosts: []string{"*.example.test"}},
		{APIHosts: []string{"https://api.example.test"}},
		{APIHosts: []string{"api.example.test:443"}},
		{APIHosts: []string{"api.example.test"}, RedirectHosts: []string{""}},
	} {
		if _, err := NewHTTPClient(Transport{HTTP: &fakeRT{}}, p); err == nil {
			t.Errorf("policy %+v accepted", p)
		}
	}
}

func get(t *testing.T, c *http.Client, url string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func TestEgressAllowsOnlyListedHosts(t *testing.T) {
	rt := &fakeRT{}
	c, err := NewHTTPClient(Transport{HTTP: rt}, policy())
	if err != nil {
		t.Fatal(err)
	}
	if err := get(t, c, "https://API.example.test./v1/servers"); err != nil {
		t.Fatalf("listed host refused: %v", err)
	}
	for _, url := range []string{
		"https://evil.example.test/v1",
		"http://api.example.test/v1",
		"https://objects.example.test/bucket", // redirect-only
		"https://169.254.169.254/latest/meta-data",
	} {
		err := get(t, c, url)
		var ee *EgressError
		if !errors.Is(err, ErrEgressDenied) || !errors.As(err, &ee) {
			t.Errorf("%s: got %v, want an EgressError", url, err)
		}
	}
	if len(rt.seen) != 1 {
		t.Fatalf("the transport saw %d requests, want only the allowed one: %v", len(rt.seen), rt.seen)
	}
}

func TestEgressRedirects(t *testing.T) {
	cases := []struct {
		name     string
		location string
		ok       bool
	}{
		{"to a listed redirect host", "https://objects.example.test/bucket", true},
		{"to an unlisted host", "https://evil.example.test/", false},
		{"to the API host, unlisted as a redirect", "https://api.example.test/other", false},
		{"downgrade to http", "http://objects.example.test/bucket", false},
	}
	for _, c := range cases {
		rt := &fakeRT{respond: func(req *http.Request) *http.Response {
			if req.URL.Path == "/v1/start" {
				return answer(req, http.StatusFound, c.location)
			}
			return answer(req, http.StatusOK, "")
		}}
		cl, err := NewHTTPClient(Transport{HTTP: rt}, policy())
		if err != nil {
			t.Fatal(err)
		}
		err = get(t, cl, "https://api.example.test/v1/start")
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok {
			if !errors.Is(err, ErrEgressDenied) {
				t.Errorf("%s: got %v, want ErrEgressDenied", c.name, err)
			}
			if len(rt.seen) != 1 {
				t.Errorf("%s: the transport saw the redirect target: %v", c.name, rt.seen)
			}
		}
	}
}

func TestEgressBoundsRedirectChains(t *testing.T) {
	rt := &fakeRT{respond: func(req *http.Request) *http.Response {
		return answer(req, http.StatusFound, "https://objects.example.test/loop")
	}}
	cl, err := NewHTTPClient(Transport{HTTP: rt}, policy())
	if err != nil {
		t.Fatal(err)
	}
	if err := get(t, cl, "https://api.example.test/v1"); !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("got %v, want ErrEgressDenied", err)
	}
	if len(rt.seen) > maxRedirects+1 {
		t.Fatalf("followed %d hops", len(rt.seen))
	}
}

// RedactError strips credentials from the message and keeps the chain, so an
// adapter can redact a transport error and its caller still matches
// ErrEgressDenied.
func TestRedactErrorKeepsTheChain(t *testing.T) {
	if RedactError(nil) != nil {
		t.Fatal("RedactError(nil) is not nil")
	}
	tok := rand.Text()
	cause := &EgressError{Host: "elsewhere.test", Reason: "proxy said Authorization: Bearer " + tok}
	err := RedactError(fmt.Errorf("probe: %w", cause))
	if strings.Contains(err.Error(), tok) {
		t.Errorf("the token survived: %q", err.Error())
	}
	if !errors.Is(err, ErrEgressDenied) {
		t.Error("errors.Is no longer reaches ErrEgressDenied")
	}
	var eg *EgressError
	if !errors.As(err, &eg) || eg.Host != "elsewhere.test" {
		t.Errorf("errors.As lost the EgressError: %v", eg)
	}
}
