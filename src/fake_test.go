package cloudkit

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake provider used by the core tests. It reaches its "API" only through
// Account.HTTP, so every call it makes crosses the egress guard, the retry
// loop and the authenticator before it reaches world, which plays the
// caller's round tripper and the provider's servers at once.

const fakeHost = "api.fake.test"

func fakeDescriptor() Descriptor {
	return Descriptor{
		ID:            "fake",
		Title:         "Fake Cloud",
		DocsURL:       "https://docs.fake.test/api",
		APIHosts:      []string{fakeHost},
		RedirectHosts: []string{"objects.fake.test"},
		TokenModel:    ReadOrReadWrite,
		UserDataLimit: 32 << 10,
	}
}

// world is the provider's state and the caller's transport.
type world struct {
	mu        sync.Mutex
	resources map[string]Resource
	next      int
	readOnly  bool                // the token may only read
	fail      []int               // status codes to answer before serving normally
	reqs      []seenReq           // every request that reached the transport
	hook      func(*http.Request) // runs before serving, outside the lock
}

type seenReq struct {
	Method, URL, Auth, UserAgent, Body string
}

func newWorld(rs ...Resource) *world {
	w := &world{resources: map[string]Resource{}}
	for _, r := range rs {
		w.resources[r.ID] = r
	}
	return w
}

func (w *world) writes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, r := range w.reqs {
		if r.Method != http.MethodGet {
			n++
		}
	}
	return n
}

func (w *world) requests() []seenReq {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.reqs)
}

func (w *world) RoundTrip(req *http.Request) (*http.Response, error) {
	if w.hook != nil {
		w.hook(req)
	}
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reqs = append(w.reqs, seenReq{req.Method, req.URL.String(), req.Header.Get("Authorization"), req.Header.Get("User-Agent"), string(body)})
	if len(w.fail) > 0 {
		code := w.fail[0]
		w.fail = w.fail[1:]
		resp := reply(req, code, nil)
		if code == http.StatusTooManyRequests {
			resp.Header.Set("Retry-After", "2")
		}
		return resp, nil
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
		return reply(req, http.StatusUnauthorized, nil), nil
	}
	id := strings.TrimPrefix(req.URL.Path, "/v1/resources/")
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/v1/token":
		return reply(req, http.StatusOK, map[string]bool{"write": !w.readOnly}), nil
	case req.Method == http.MethodGet && req.URL.Path == "/v1/resources":
		return reply(req, http.StatusOK, w.list()), nil
	case w.readOnly:
		return reply(req, http.StatusForbidden, nil), nil
	case req.Method == http.MethodPost && req.URL.Path == "/v1/resources":
		var r Resource
		if err := json.Unmarshal(body, &r); err != nil {
			return reply(req, http.StatusBadRequest, nil), nil
		}
		w.next++
		r.ID = fmt.Sprintf("r%d", w.next)
		w.resources[r.ID] = r
		return reply(req, http.StatusCreated, r), nil
	case req.Method == http.MethodPut && id != req.URL.Path:
		var r Resource
		if err := json.Unmarshal(body, &r); err != nil {
			return reply(req, http.StatusBadRequest, nil), nil
		}
		w.resources[id] = r
		return reply(req, http.StatusOK, r), nil
	case req.Method == http.MethodDelete && id != req.URL.Path:
		if _, ok := w.resources[id]; !ok {
			return reply(req, http.StatusNotFound, nil), nil
		}
		delete(w.resources, id)
		for k, r := range w.resources { // attached things go with it
			if r.Attrs["attached_to"] == id {
				delete(w.resources, k)
			}
		}
		return reply(req, http.StatusNoContent, nil), nil
	}
	return reply(req, http.StatusNotFound, nil), nil
}

func (w *world) list() []Resource {
	out := make([]Resource, 0, len(w.resources))
	for _, r := range w.resources {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Resource) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// set changes the world behind the plan's back.
func (w *world) set(r Resource) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.resources[r.ID] = r
}

func reply(req *http.Request, code int, v any) *http.Response {
	var b []byte
	if v != nil {
		b, _ = json.Marshal(v)
	}
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(b)), Request: req}
}

// fakeProvider implements Provider and Applier over the world's API.
type fakeProvider struct {
	acct Account
}

func (p *fakeProvider) Descriptor() Descriptor { return fakeDescriptor() }

func (p *fakeProvider) Probe(ctx context.Context) (CapSet, error) {
	var tok struct{ Write bool }
	if err := p.call(ctx, http.MethodGet, "/v1/token", nil, &tok); err != nil {
		return nil, err
	}
	write := Denied
	if tok.Write {
		write = Allowed
	}
	return CapSet{
		CapInventory:     Allowed,
		CapServerCreate:  write,
		CapServerDestroy: write,
		CapVolumeWrite:   write,
	}, nil
}

func (p *fakeProvider) Inventory(ctx context.Context, f Filter) ([]Resource, error) {
	var all []Resource
	if err := p.call(ctx, http.MethodGet, "/v1/resources", nil, &all); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(r Resource) bool { return !f.Matches(r) }), nil
}

// PlanCreate and PlanDestroy are what a ServerLifecycle adapter would offer.
func (p *fakeProvider) PlanCreate(ctx context.Context, r Resource) (Plan, error) {
	if err := p.writable(ctx); err != nil {
		return Plan{}, err
	}
	live, err := p.Inventory(ctx, Filter{})
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(p.acct.Provider, p.acct.Name, []Change{{Action: Create, After: &r}}, nil, live)
}

func (p *fakeProvider) PlanDestroy(ctx context.Context, id string) (Plan, error) {
	if err := p.writable(ctx); err != nil {
		return Plan{}, err
	}
	live, err := p.Inventory(ctx, Filter{})
	if err != nil {
		return Plan{}, err
	}
	i := slices.IndexFunc(live, func(r Resource) bool { return r.ID == id })
	if i < 0 {
		return Plan{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	target := live[i]
	loses := []Resource{target}
	for _, r := range live {
		if r.Attrs["attached_to"] == id {
			loses = append(loses, r)
		}
	}
	return NewPlan(p.acct.Provider, p.acct.Name, []Change{{Action: Delete, Before: &target}}, loses, live)
}

func (p *fakeProvider) writable(ctx context.Context) error {
	caps, err := p.Probe(ctx)
	if err != nil {
		return err
	}
	if caps.Get(CapServerCreate) != Allowed {
		return fmt.Errorf("%w: the token is read only", ErrDenied)
	}
	return nil
}

func (p *fakeProvider) LiveState(ctx context.Context, _ Plan) ([]Resource, error) {
	return p.Inventory(ctx, Filter{})
}

func (p *fakeProvider) Execute(ctx context.Context, pl Plan) (Result, error) {
	var res Result
	for _, c := range pl.Changes {
		switch c.Action {
		case Create:
			var r Resource
			if err := p.call(ctx, http.MethodPost, "/v1/resources", c.After, &r); err != nil {
				return Result{}, err
			}
			res.Resources = append(res.Resources, r)
		case Update:
			if err := p.call(ctx, http.MethodPut, "/v1/resources/"+c.Before.ID, c.After, nil); err != nil {
				return Result{}, err
			}
			res.Resources = append(res.Resources, *c.After)
		case Delete:
			if err := p.call(ctx, http.MethodDelete, "/v1/resources/"+c.Before.ID, nil, nil); err != nil {
				return Result{}, err
			}
		}
	}
	return res, nil
}

func (p *fakeProvider) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+fakeHost+path, body)
	if err != nil {
		return err
	}
	resp, err := p.acct.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s %s", ErrDenied, method, path)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s", ErrNotFound, method, path)
	case resp.StatusCode >= 300:
		return fmt.Errorf("fake: %s %s: %d %s", method, path, resp.StatusCode, RedactBody(b))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// fakeFactory builds fake providers; opened counts the accounts it saw.
func fakeFactory(opened *[]Account) Factory {
	return Factory{Descriptor: fakeDescriptor(), New: func(_ context.Context, a Account) (Provider, error) {
		if opened != nil {
			*opened = append(*opened, a)
		}
		return &fakeProvider{acct: a}, nil
	}}
}

// readOnlyProvider implements Provider and nothing else.
type readOnlyProvider struct{ d Descriptor }

func (p readOnlyProvider) Descriptor() Descriptor { return p.d }
func (p readOnlyProvider) Probe(context.Context) (CapSet, error) {
	return CapSet{CapInventory: Allowed}, nil
}
func (p readOnlyProvider) Inventory(context.Context, Filter) ([]Resource, error) {
	return nil, nil
}

// creds is a CredentialSource whose token changes when rotate is called.
type creds struct {
	mu    sync.Mutex
	token string
	calls int
	err   error
}

func newCreds() *creds { return &creds{token: rand.Text()} }

func (c *creds) Token(_ context.Context, _ string) (Secret, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return Secret{}, c.err
	}
	return NewSecret([]byte(c.token)), nil
}

func (c *creds) rotate() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = rand.Text()
	return c.token
}

func (c *creds) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func noDial(context.Context, string, string) (net.Conn, error) { return nil, errors.New("no SSH here") }

// harness is one registry over the fake provider.
type harness struct {
	t      *testing.T
	world  *world
	creds  *creds
	reg    *Registry
	logs   *bytes.Buffer
	slept  []time.Duration
	opened []Account
}

func newHarness(t *testing.T, o Options, rs ...Resource) *harness {
	t.Helper()
	h := &harness{t: t, world: newWorld(rs...), creds: newCreds(), logs: &bytes.Buffer{}}
	if o.Transport.HTTP == nil {
		o.Transport = Transport{HTTP: h.world, Dial: noDial}
	}
	if o.Credentials == nil {
		o.Credentials = h.creds
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewJSONHandler(h.logs, nil))
	}
	reg, err := NewRegistry(o, fakeFactory(&h.opened))
	if err != nil {
		t.Fatal(err)
	}
	reg.sleep = func(ctx context.Context, d time.Duration) error {
		h.slept = append(h.slept, d)
		return ctx.Err()
	}
	h.reg = reg
	return h
}

func (h *harness) open() *fakeProvider {
	h.t.Helper()
	p, err := h.reg.Open(context.Background(), "fake", "production")
	if err != nil {
		h.t.Fatal(err)
	}
	return p.(*fakeProvider)
}
