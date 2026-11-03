package cloudkit

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func validOptions() Options {
	return Options{
		Transport:   Transport{HTTP: newWorld(), Dial: noDial},
		Credentials: newCreds(),
		Logger:      slog.New(slog.DiscardHandler),
	}
}

// C2, C5, C10: a registry is never built over a missing or default
// transport, a missing credential source or a missing logger.
func TestNewRegistryRefusesBadOptions(t *testing.T) {
	cases := map[string]func(*Options){
		"nil HTTP":          func(o *Options) { o.Transport.HTTP = nil },
		"default transport": func(o *Options) { o.Transport.HTTP = http.DefaultTransport },
		"nil Dial":          func(o *Options) { o.Transport.Dial = nil },
		"nil credentials":   func(o *Options) { o.Credentials = nil },
		"nil logger":        func(o *Options) { o.Logger = nil },
		"negative retries":  func(o *Options) { o.Retry.MaxAttempts = -1 },
		"base above max":    func(o *Options) { o.Retry = RetryPolicy{BaseDelay: time.Minute, MaxDelay: time.Second} },
	}
	for name, mutate := range cases {
		o := validOptions()
		mutate(&o)
		if _, err := NewRegistry(o, fakeFactory(nil)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := NewRegistry(validOptions(), fakeFactory(nil)); err != nil {
		t.Fatalf("valid options refused: %v", err)
	}
	if _, err := NewRegistry(Options{Transport: Transport{Dial: noDial}}); !errors.Is(err, ErrInvalidTransport) {
		t.Errorf("nil HTTP: got %v, want ErrInvalidTransport", err)
	}
}

// C9: the registry is the only place providers are known, so it refuses two
// factories with one ID and any ID that is empty or not a lower-case slug.
func TestNewRegistryRefusesBadFactories(t *testing.T) {
	with := func(mutate func(*Factory)) Factory {
		f := fakeFactory(nil)
		mutate(&f)
		return f
	}
	cases := map[string][]Factory{
		"duplicate ID":       {fakeFactory(nil), fakeFactory(nil)},
		"empty ID":           {with(func(f *Factory) { f.Descriptor.ID = "" })},
		"blank ID":           {with(func(f *Factory) { f.Descriptor.ID = " " })},
		"upper-case ID":      {with(func(f *Factory) { f.Descriptor.ID = "Hetzner" })},
		"ID with a slash":    {with(func(f *Factory) { f.Descriptor.ID = "simtabi/fake" })},
		"ID with a space":    {with(func(f *Factory) { f.Descriptor.ID = "fake cloud" })},
		"trailing hyphen":    {with(func(f *Factory) { f.Descriptor.ID = "fake-" })},
		"ID too long":        {with(func(f *Factory) { f.Descriptor.ID = strings.Repeat("a", maxIDLen+1) })},
		"no title":           {with(func(f *Factory) { f.Descriptor.Title = "" })},
		"http docs":          {with(func(f *Factory) { f.Descriptor.DocsURL = "http://docs.fake.test" })},
		"no API hosts":       {with(func(f *Factory) { f.Descriptor.APIHosts = nil })},
		"wildcard host":      {with(func(f *Factory) { f.Descriptor.APIHosts = []string{"*.fake.test"} })},
		"bad redirect host":  {with(func(f *Factory) { f.Descriptor.RedirectHosts = []string{"https://x.test"} })},
		"no token model":     {with(func(f *Factory) { f.Descriptor.TokenModel = 0 })},
		"unknown token":      {with(func(f *Factory) { f.Descriptor.TokenModel = Unscoped + 1 })},
		"negative user data": {with(func(f *Factory) { f.Descriptor.UserDataLimit = -1 })},
		"nil New":            {with(func(f *Factory) { f.New = nil })},
	}
	for name, fs := range cases {
		if _, err := NewRegistry(validOptions(), fs...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	other := with(func(f *Factory) { f.Descriptor.ID = "fake-two" })
	r, err := NewRegistry(validOptions(), fakeFactory(nil), other)
	if err != nil {
		t.Fatalf("two distinct factories refused: %v", err)
	}
	ds := r.Descriptors()
	if len(ds) != 2 || ds[0].ID != "fake" || ds[1].ID != "fake-two" {
		t.Fatalf("Descriptors = %+v", ds)
	}
	ds[0].APIHosts[0] = "evil.test"
	if r.Descriptors()[0].APIHosts[0] != fakeHost {
		t.Fatal("Descriptors hands out the registry's own slices")
	}
}

// C9: nothing is shared between registries or between Opens.
func TestRegistryHoldsNoSharedClient(t *testing.T) {
	h := newHarness(t, Options{})
	a, b := h.open(), h.open()
	if a.acct.HTTP == b.acct.HTTP {
		t.Fatal("two Opens share one http.Client")
	}
	if a.acct.Provider != "fake" || a.acct.Name != "production" || a.acct.Logger == nil {
		t.Fatalf("account = %+v", a.acct)
	}
	h2 := newHarness(t, Options{})
	if h2.open().acct.HTTP == a.acct.HTTP {
		t.Fatal("two registries share one http.Client")
	}
}

func TestOpenRefuses(t *testing.T) {
	h := newHarness(t, Options{})
	ctx := context.Background()
	if _, err := h.reg.Open(ctx, "nope", "production"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown provider: %v", err)
	}
	if _, err := h.reg.Open(ctx, "fake", ""); err == nil {
		t.Error("empty account accepted")
	}

	token := rand.Text()
	cases := map[string]func(context.Context, Account) (Provider, error){
		"nil provider": func(context.Context, Account) (Provider, error) { return nil, nil },
		"wrong ID": func(context.Context, Account) (Provider, error) {
			d := fakeDescriptor()
			d.ID = "other"
			return readOnlyProvider{d}, nil
		},
		"factory error": func(context.Context, Account) (Provider, error) {
			return nil, errors.New("dial failed: Authorization: Bearer " + token)
		},
	}
	for name, newFn := range cases {
		r, err := NewRegistry(validOptions(), Factory{Descriptor: fakeDescriptor(), New: newFn})
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Open(ctx, "fake", "production")
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("%s: error leaks the token: %v", name, err)
		}
	}
}

// C5: the token is fetched for every request, so rotation needs no restart,
// and it travels only in the Authorization header.
func TestClientAuthenticatesEveryRequest(t *testing.T) {
	h := newHarness(t, Options{UserAgent: "dbpantry/1.4.0"})
	p := h.open()
	ctx := context.Background()
	if _, err := p.Inventory(ctx, Filter{}); err != nil {
		t.Fatal(err)
	}
	first := h.creds.token
	second := h.creds.rotate()
	if _, err := p.Inventory(ctx, Filter{}); err != nil {
		t.Fatal(err)
	}
	reqs := h.world.requests()
	if len(reqs) != 2 || h.creds.count() != 2 {
		t.Fatalf("requests %d, token fetches %d; want 2 and 2", len(reqs), h.creds.count())
	}
	if reqs[0].Auth != "Bearer "+first || reqs[1].Auth != "Bearer "+second {
		t.Fatal("the rotated token was not used on the next request")
	}
	if reqs[0].UserAgent != "dbpantry/1.4.0" {
		t.Errorf("User-Agent = %q", reqs[0].UserAgent)
	}
}

// C5 with C4: a redirect to an allowed object host is followed, but carries
// no credential, even one the adapter set by hand.
func TestTokenNeverFollowsARedirect(t *testing.T) {
	h := newHarness(t, Options{})
	inner := h.world
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == fakeHost && req.URL.Path == "/v1/download" {
			resp := reply(req, http.StatusFound, nil)
			resp.Header.Set("Location", "https://objects.fake.test/blob")
			_, _ = inner.RoundTrip(req) // record it
			return resp, nil
		}
		if req.URL.Host == "objects.fake.test" {
			_, _ = inner.RoundTrip(req)
			return reply(req, http.StatusOK, nil), nil
		}
		return inner.RoundTrip(req)
	})
	o := Options{Transport: Transport{HTTP: rt, Dial: noDial}, Credentials: h.creds, Logger: slog.New(slog.DiscardHandler)}
	r, err := NewRegistry(o, fakeFactory(nil))
	if err != nil {
		t.Fatal(err)
	}
	prov, err := r.Open(context.Background(), "fake", "production")
	if err != nil {
		t.Fatal(err)
	}
	c := prov.(*fakeProvider).acct.HTTP
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+fakeHost+"/v1/download", nil)
	req.Header.Set("Authorization", "Bearer set-by-the-adapter")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	reqs := inner.requests()
	if len(reqs) != 2 {
		t.Fatalf("saw %d requests, want the API call and the redirect", len(reqs))
	}
	if !strings.HasPrefix(reqs[0].Auth, "Bearer ") || reqs[0].Auth == "Bearer set-by-the-adapter" {
		t.Errorf("API request auth = %q, want the source's token", reqs[0].Auth)
	}
	if reqs[1].Auth != "" {
		t.Fatalf("the redirect to the object host carried %q", reqs[1].Auth)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// C3 before C5: a refused host never reaches the credential source.
func TestEgressRefusalPrecedesCredentials(t *testing.T) {
	h := newHarness(t, Options{})
	c := h.open().acct.HTTP
	for _, u := range []string{"https://evil.test/v1", "http://" + fakeHost + "/v1", "https://objects.fake.test/blob"} {
		if err := get(t, c, u); !errors.Is(err, ErrEgressDenied) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if h.creds.count() != 0 || len(h.world.requests()) != 0 {
		t.Fatalf("credential fetches %d, requests %d; want 0 and 0", h.creds.count(), len(h.world.requests()))
	}
}

func TestCredentialFailuresAreRedacted(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.open()
	leak := rand.Text()
	h.creds.err = errors.New("vault said: token=" + leak + " expired")
	_, err := p.Inventory(context.Background(), Filter{})
	if err == nil || strings.Contains(err.Error(), leak) {
		t.Fatalf("error = %v", err)
	}
	h.creds.err = nil
	h.creds.token = ""
	if _, err := p.Inventory(context.Background(), Filter{}); !errors.Is(err, errEmptyToken) {
		t.Fatalf("empty token: %v", err)
	}
	if n := len(h.world.requests()); n != 0 {
		t.Fatalf("%d requests went out without a credential", n)
	}
}

func TestProbeReportsWritesDeniedForAReadOnlyToken(t *testing.T) {
	h := newHarness(t, Options{})
	h.world.readOnly = true
	p := h.open()
	caps, err := p.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps.Capabilities() {
		if c.IsWrite() && caps.Get(c) != Denied {
			t.Errorf("%s = %s for a read-only token", c, caps.Get(c))
		}
	}
	if !caps.Allowed(CapInventory) {
		t.Error("a read-only token cannot read")
	}
	if _, err := p.PlanCreate(context.Background(), Resource{Kind: "server", Name: "web-1"}); !errors.Is(err, ErrDenied) {
		t.Errorf("PlanCreate with a read-only token: %v", err)
	}
}

func TestInventoryFilters(t *testing.T) {
	h := newHarness(t, Options{},
		Resource{Kind: "server", ID: "s1", Name: "web-1", Region: "fsn1", Labels: map[string]string{"env": "prod"}},
		Resource{Kind: "server", ID: "s2", Name: "web-2", Region: "nbg1"},
		Resource{Kind: "volume", ID: "v1", Name: "data", Region: "fsn1"},
	)
	p := h.open()
	cases := []struct {
		f    Filter
		want int
	}{
		{Filter{}, 3},
		{Filter{Kinds: []string{"server"}}, 2},
		{Filter{Region: "fsn1"}, 2},
		{Filter{Labels: map[string]string{"env": "prod"}}, 1},
		{Filter{Labels: map[string]string{"env": "dev"}}, 0},
		{Filter{Kinds: []string{"volume", "server"}, Region: "nbg1"}, 1},
	}
	for _, c := range cases {
		got, err := p.Inventory(context.Background(), c.f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("%+v: %d resources, want %d", c.f, len(got), c.want)
		}
	}
}

func TestOpenIsSafeConcurrently(t *testing.T) {
	r, err := NewRegistry(validOptions(), fakeFactory(nil))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			p, err := r.Open(context.Background(), "fake", "production")
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := p.Inventory(context.Background(), Filter{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
