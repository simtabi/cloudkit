package cloudkittest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/simtabi/cloudkit/src"
)

// contractAccount is the account name every case opens.
const contractAccount = "contract"

// The hosts the egress case uses. .invalid never resolves (RFC 2606), so even
// a request that escaped every guard could not reach anything.
const (
	outsideHost  = "outside.cloudkit.invalid"
	redirectHost = "redirect.cloudkit.invalid"
)

var errTransportDown = errors.New("cloudkittest: the injected transport is down")

type runner struct {
	ctx   context.Context
	s     Suite
	token string
}

// tokenSource hands out the run's token for every account.
type tokenSource struct{ v string }

func (t tokenSource) Token(context.Context, string) (cloudkit.Secret, error) {
	return cloudkit.NewSecret([]byte(t.v)), nil
}

func refuseDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("cloudkittest: the contract suite never dials")
}

// lockedBuffer collects log records written from any goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is one registry a case opens the adapter through.
type env struct {
	reg  *cloudkit.Registry
	logs *lockedBuffer

	mu       sync.Mutex
	accounts []cloudkit.Account
}

func (e *env) lastAccount() (cloudkit.Account, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.accounts) == 0 {
		return cloudkit.Account{}, false
	}
	return e.accounts[len(e.accounts)-1], true
}

// open builds a registry over rt and opens the adapter. It records the
// Account the factory received, so a case can use the very client the
// adapter was given.
func (r *runner) open(c *caseT, rt http.RoundTripper) (*env, cloudkit.Provider, bool) {
	e := &env{logs: &lockedBuffer{}}
	f := r.s.Factory
	inner := f.New
	f.New = func(ctx context.Context, a cloudkit.Account) (cloudkit.Provider, error) {
		e.mu.Lock()
		e.accounts = append(e.accounts, a)
		e.mu.Unlock()
		return inner(ctx, a)
	}
	reg, err := cloudkit.NewRegistry(cloudkit.Options{
		Transport:   cloudkit.Transport{HTTP: rt, Dial: refuseDial},
		Credentials: tokenSource{r.token},
		Logger:      slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Retry:       cloudkit.RetryPolicy{MaxAttempts: 1}, // a cassette records each call once
		UserAgent:   "cloudkittest",
	}, f)
	if err != nil {
		c.failf("NewRegistry refused the suite's factory: %v", err)
		return nil, nil, false
	}
	e.reg = reg
	p, err := reg.Open(r.ctx, f.Descriptor.ID, contractAccount)
	if err != nil {
		c.failf("Open failed: %v", err)
		return nil, nil, false
	}
	return e, p, true
}

func (r *runner) recorder(c *caseT, cassette string) (*Recorder, bool) {
	rec, err := NewRecorder(r.s.Recordings, cassette)
	if err != nil {
		c.failf("%v", err)
		return nil, false
	}
	return rec, true
}

// cassetteDone fails the case for every request the cassette did not hold
// and every interaction the adapter never asked for.
func cassetteDone(c *caseT, cassette string, rec *Recorder) {
	for _, u := range rec.Unexpected() {
		c.failf("cassette %s does not hold %s %s", cassette, u.Method, u.URL)
	}
	for _, u := range rec.Unused() {
		c.failf("the adapter never sent the recorded %s %s (cassette %s)", u.Method, u.URL, cassette)
	}
}

// faultRT answers every request itself and keeps each one, with its raw URL
// and headers, for the case to inspect.
type faultRT struct {
	answer func(*http.Request) (*http.Response, error)

	mu   sync.Mutex
	seen []*http.Request
}

func (f *faultRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	f.mu.Lock()
	f.seen = append(f.seen, req)
	f.mu.Unlock()
	return f.answer(req)
}

func (f *faultRT) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

// spyRT passes requests on and keeps their raw URLs.
type spyRT struct {
	next http.RoundTripper

	mu   sync.Mutex
	urls []string
}

func (s *spyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.urls = append(s.urls, req.URL.String())
	s.mu.Unlock()
	return s.next.RoundTrip(req)
}

func reply(req *http.Request, code int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", code, http.StatusText(code)),
		StatusCode:    code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// call is one thing a case asks the adapter to do.
type call struct {
	name string
	run  func(ctx context.Context) error
}

// everyCall is every call the suite knows how to make: Probe, Inventory and
// each plan.
func (r *runner) everyCall(p cloudkit.Provider) []call {
	calls := []call{
		{"Probe", func(ctx context.Context) error { _, err := p.Probe(ctx); return err }},
		{"Inventory", func(ctx context.Context) error { _, err := p.Inventory(ctx, cloudkit.Filter{}); return err }},
	}
	plan := func(name string, f PlanFunc) call {
		return call{name, func(ctx context.Context) error { _, err := f(ctx, p); return err }}
	}
	for _, w := range r.s.Writes {
		calls = append(calls, plan(w.Name, w.Plan))
	}
	if r.s.Drift != nil {
		calls = append(calls, plan("Drift", r.s.Drift))
	}
	if r.s.Destroy != nil && r.s.Destroy.Plan != nil {
		calls = append(calls, plan("Destroy", r.s.Destroy.Plan))
	}
	return calls
}

// guarded runs f, turning a panic into its printed value.
func guarded(f func() error) (recovered string, err error) {
	defer func() {
		if p := recover(); p != nil {
			recovered = fmt.Sprint(p)
		}
	}()
	return "", f()
}

func isWriteMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// 1. Transport only: every request goes through the injected transport, and
// a failing transport fails every call.
func (r *runner) transportOnly(c *caseT) {
	if rec, ok := r.recorder(c, CassetteReadWrite); ok {
		if _, p, ok := r.open(c, rec); ok {
			n := len(rec.Requests())
			caps, err := p.Probe(r.ctx)
			switch {
			case err != nil:
				c.failf("Probe over the read-write cassette: %v", err)
			case len(rec.Requests()) == n:
				c.failf("Probe sent nothing through the injected transport")
			}
			for _, cp := range caps.Capabilities() {
				if cp.IsWrite() && caps.Get(cp) == cloudkit.Allowed &&
					!slices.ContainsFunc(r.s.Writes, func(w Write) bool { return w.Capability == cp }) {
					c.failf("Probe allows %s, and the suite has no Write for it", cp)
				}
			}
			n = len(rec.Requests())
			if _, err := p.Inventory(r.ctx, cloudkit.Filter{}); err != nil {
				c.failf("Inventory over the read-write cassette: %v", err)
			} else if len(rec.Requests()) == n {
				c.failf("Inventory sent nothing through the injected transport")
			}
			cassetteDone(c, CassetteReadWrite, rec)
		}
	}
	down := &faultRT{answer: func(*http.Request) (*http.Response, error) { return nil, errTransportDown }}
	_, p, ok := r.open(c, down)
	if !ok {
		return
	}
	for _, cl := range r.everyCall(p) {
		rec, err := guarded(func() error { return cl.run(r.ctx) })
		switch {
		case rec != "":
			c.failf("%s panicked with a failing transport: %s", cl.name, rec)
		case err == nil:
			c.failf("%s succeeded although the injected transport fails every request", cl.name)
		}
	}
	if len(down.requests()) == 0 {
		c.failf("no request reached the injected transport")
	}
}

// 2. Nil transport: NewRegistry refuses it, and the factory refuses an
// account with no guarded client rather than falling back to the default one.
func (r *runner) nilTransport(c *caseT) {
	creds := tokenSource{r.token}
	discard := slog.New(slog.DiscardHandler)
	transports := []struct {
		name string
		t    cloudkit.Transport
	}{
		{"a nil HTTP round tripper", cloudkit.Transport{Dial: refuseDial}},
		{"a nil Dial", cloudkit.Transport{HTTP: &faultRT{answer: func(*http.Request) (*http.Response, error) { return nil, errTransportDown }}}},
	}
	for _, tc := range transports {
		_, err := cloudkit.NewRegistry(cloudkit.Options{Transport: tc.t, Credentials: creds, Logger: discard}, r.s.Factory)
		if !errors.Is(err, cloudkit.ErrInvalidTransport) {
			c.failf("NewRegistry with %s: got %v, want ErrInvalidTransport", tc.name, err)
		}
	}
	clients := []struct {
		name string
		c    *http.Client
	}{
		{"a nil client", nil},
		{"a client with no Transport, which would use the default one", &http.Client{}},
	}
	for _, tc := range clients {
		var prov cloudkit.Provider
		rec, err := guarded(func() error {
			var err error
			prov, err = r.s.Factory.New(r.ctx, cloudkit.Account{
				Provider: r.s.Factory.Descriptor.ID, Name: contractAccount, HTTP: tc.c, Logger: discard,
			})
			return err
		})
		switch {
		case rec != "":
			c.failf("Factory.New panicked on %s; it must return an error: %s", tc.name, rec)
		case err == nil || prov != nil:
			c.failf("Factory.New accepted %s; it must refuse it, not fall back (C2)", tc.name)
		}
	}
}

// 3. Egress: a request to a host outside APIHosts is refused before the
// transport sees it, and a redirect to an unlisted host is refused.
func (r *runner) egress(c *caseT) {
	d := r.s.Factory.Descriptor
	redirector := &faultRT{answer: func(req *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Location", "https://"+redirectHost+"/elsewhere")
		return reply(req, http.StatusFound, h, ""), nil
	}}
	e, p, ok := r.open(c, redirector)
	if !ok {
		return
	}
	got := p.Descriptor()
	if !sameHosts(got.APIHosts, d.APIHosts) || !sameHosts(got.RedirectHosts, d.RedirectHosts) {
		c.failf("the provider describes hosts %v/%v, its factory %v/%v", got.APIHosts, got.RedirectHosts, d.APIHosts, d.RedirectHosts)
	}
	if acct, ok := e.lastAccount(); ok && acct.HTTP != nil {
		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, "https://"+outsideHost+"/", nil)
		if err != nil {
			c.failf("%v", err)
			return
		}
		resp, err := acct.HTTP.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, cloudkit.ErrEgressDenied) {
			c.failf("the adapter's client sent a request to %s: got %v, want ErrEgressDenied", outsideHost, err)
		}
	} else {
		c.failf("the factory was never handed an account with a client")
	}
	for _, cl := range r.everyCall(p)[:2] { // Probe and Inventory
		rec, err := guarded(func() error { return cl.run(r.ctx) })
		switch {
		case rec != "":
			c.failf("%s panicked on a redirect: %s", cl.name, rec)
		case !errors.Is(err, cloudkit.ErrEgressDenied):
			c.failf("%s, redirected to an unlisted host: got %v, want an error matching ErrEgressDenied", cl.name, err)
		}
	}
	for _, req := range redirector.requests() {
		if h := strings.ToLower(req.URL.Hostname()); !slices.Contains(lower(d.APIHosts), h) {
			c.failf("a request to %s reached the transport", h)
		}
	}
}

func lower(hs []string) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = strings.TrimSuffix(strings.ToLower(h), ".")
	}
	return out
}

func sameHosts(a, b []string) bool {
	x, y := lower(a), lower(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// 4. Read-only token: Probe reports every write as Denied, and every write
// plan returns ErrDenied without sending a write.
func (r *runner) readOnly(c *caseT) {
	rec, ok := r.recorder(c, CassetteReadOnly)
	if !ok {
		return
	}
	_, p, ok := r.open(c, rec)
	if !ok {
		return
	}
	caps, err := p.Probe(r.ctx)
	if err != nil {
		c.failf("Probe with a read-only token: %v", err)
		return
	}
	for _, cp := range caps.Capabilities() {
		if cp.IsWrite() && caps.Get(cp) != cloudkit.Denied {
			c.failf("Probe reports %s as %s with a read-only token, want denied", cp, caps.Get(cp))
		}
	}
	for _, w := range r.s.Writes {
		if _, said := caps[w.Capability]; !said {
			c.failf("Probe with a read-only token does not report %s (needed by %s), want denied", w.Capability, w.Name)
		}
	}
	for _, w := range r.s.Writes {
		rec, err := guarded(func() error { _, err := w.Plan(r.ctx, p); return err })
		switch {
		case rec != "":
			c.failf("%s panicked with a read-only token: %s", w.Name, rec)
		case !errors.Is(err, cloudkit.ErrDenied):
			c.failf("%s with a read-only token: got %v, want ErrDenied", w.Name, err)
		}
	}
	for _, q := range rec.Requests() {
		if isWriteMethod(q.Method) {
			c.failf("a read-only token sent %s %s", q.Method, q.URL)
		}
	}
	cassetteDone(c, CassetteReadOnly, rec)
}

// 5. Plan drift: Apply of a plan whose live state changed returns ErrConflict
// and makes no write.
func (r *runner) planDrift(c *caseT) {
	if len(r.s.Writes) == 0 {
		c.skipf("the adapter plans no writes")
		return
	}
	rec, ok := r.recorder(c, CassetteDrift)
	if !ok {
		return
	}
	e, p, ok := r.open(c, rec)
	if !ok {
		return
	}
	plan, err := r.s.Drift(r.ctx, p)
	if err != nil {
		c.failf("Drift plan: %v", err)
		return
	}
	_, err = e.reg.Apply(r.ctx, plan, cloudkit.Confirmation{Typed: plan.ConfirmText()})
	if !errors.Is(err, cloudkit.ErrConflict) {
		c.failf("Apply after the live state moved: got %v, want ErrConflict", err)
	}
	for _, q := range rec.Requests() {
		if isWriteMethod(q.Method) {
			c.failf("Apply sent %s %s although the live state moved", q.Method, q.URL)
		}
	}
	cassetteDone(c, CassetteDrift, rec)
}

// 6. Destroy: without the typed name Apply refuses; the plan lists every
// attached volume and IP; with the name typed back it goes through, so the
// refusals were the name and nothing else.
func (r *runner) destroy(c *caseT) {
	if len(r.s.Writes) == 0 {
		c.skipf("the adapter plans no writes")
		return
	}
	rec, ok := r.recorder(c, CassetteDestroy)
	if !ok {
		return
	}
	e, p, ok := r.open(c, rec)
	if !ok {
		return
	}
	plan, err := r.s.Destroy.Plan(r.ctx, p)
	if err != nil {
		c.failf("Destroy plan: %v", err)
		return
	}
	if !plan.IsDestroy() {
		c.failf("the Destroy plan deletes nothing")
		return
	}
	for _, want := range r.s.Destroy.Loses {
		if !slices.ContainsFunc(plan.Loses, func(l cloudkit.Resource) bool { return l.Kind == want.Kind && l.ID == want.ID }) {
			c.failf("the destroy plan does not list %s %s in Loses", want.Kind, want.ID)
		}
	}
	n := len(rec.Requests())
	for _, typed := range []string{"", "not " + plan.ConfirmText()} {
		if _, err := e.reg.Apply(r.ctx, plan, cloudkit.Confirmation{Typed: typed}); !errors.Is(err, cloudkit.ErrConfirmationRequired) {
			c.failf("Apply with %q typed: got %v, want ErrConfirmationRequired", typed, err)
		}
	}
	if len(rec.Requests()) != n {
		c.failf("a refused destroy sent %d request(s)", len(rec.Requests())-n)
	}
	if _, err := e.reg.Apply(r.ctx, plan, cloudkit.Confirmation{Typed: plan.ConfirmText()}); err != nil {
		c.failf("Apply with the names typed back failed, so the refusals prove nothing: %v", err)
	}
	cassetteDone(c, CassetteDestroy, rec)
}

// 7. Redaction: no token, Authorization header or credential query string
// appears in any error, log record or panic, including when a proxy or the
// provider echoes the request back.
func (r *runner) redaction(c *caseT) {
	tok := r.token
	echo := &faultRT{answer: func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("cloudkittest: proxy refused %s?access_token=%s: Authorization: %s", req.URL.Path, tok, req.Header.Get("Authorization"))
	}}
	reflect := &faultRT{answer: func(req *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		body := fmt.Sprintf(`{"message":"refused Authorization: Bearer %s","request":"%s?access_token=%s"}`, tok, req.URL.Path, tok)
		return reply(req, http.StatusUnauthorized, h, body), nil
	}}
	var outputs []string // "where: text"
	var urls []string
	var logs []*lockedBuffer
	for _, f := range []struct {
		name string
		rt   *faultRT
	}{{"a proxy that echoes the request", echo}, {"a provider that echoes the token", reflect}} {
		e, p, ok := r.open(c, f.rt)
		if !ok {
			return
		}
		logs = append(logs, e.logs)
		for _, cl := range r.everyCall(p) {
			rec, err := guarded(func() error { return cl.run(r.ctx) })
			if rec != "" {
				c.failf("%s panicked behind %s", cl.name, f.name)
				outputs = append(outputs, cl.name+" panic: "+rec)
			}
			if err != nil {
				outputs = append(outputs, cl.name+" error behind "+f.name+": "+err.Error())
			}
		}
		for _, q := range f.rt.requests() {
			urls = append(urls, q.URL.String())
		}
	}
	if rec, ok := r.recorder(c, CassetteReadWrite); ok {
		spy := &spyRT{next: rec}
		if e, p, ok := r.open(c, spy); ok {
			logs = append(logs, e.logs)
			for _, cl := range r.everyCall(p)[:2] {
				if err := cl.run(r.ctx); err != nil {
					outputs = append(outputs, cl.name+" error: "+err.Error())
				}
			}
			spy.mu.Lock()
			urls = append(urls, spy.urls...)
			spy.mu.Unlock()
			cassetteDone(c, CassetteReadWrite, rec)
		}
	}
	for _, o := range outputs {
		if strings.Contains(o, tok) {
			where, _, _ := strings.Cut(o, ":")
			c.failf("the token appears in %s", where)
		}
	}
	for _, l := range logs {
		if strings.Contains(l.String(), tok) {
			c.failf("a log record carries the token")
		}
	}
	for _, u := range urls {
		if strings.Contains(u, tok) {
			c.failf("the token was sent in a URL: %s", u)
		}
	}
}

// 8. cloud-init: a secret in any field but OneTimeToken fails the render, and
// output above UserDataLimit fails.
func (r *runner) cloudInit(c *caseT) {
	limit := r.s.Factory.Descriptor.UserDataLimit
	ci := r.s.CloudInit
	if ci == nil {
		if limit == 0 {
			c.skipf("the provider accepts no user data (UserDataLimit 0)")
			return
		}
		c.failf("the descriptor accepts %d bytes of user data, and the suite has no CloudInit to check", limit)
		return
	}
	if !slices.Contains(ci.Fields, OneTimeTokenField) {
		c.failf("CloudInit.Fields must include %s", OneTimeTokenField)
		return
	}
	render := func(fields map[string]string) (string, error) {
		var out []byte
		rec, err := guarded(func() error {
			var err error
			out, err = ci.Render(r.ctx, fields, limit)
			return err
		})
		if rec != "" {
			return "", fmt.Errorf("panicked: %s", rec)
		}
		return string(out), err
	}
	base := map[string]string{}
	for _, f := range ci.Fields {
		base[f] = "cloudkit-contract"
	}
	out, err := render(maps.Clone(base))
	if limit == 0 {
		if err == nil {
			c.failf("Render produced user data for a provider that accepts none")
		}
		return
	}
	if err != nil {
		c.failf("Render refused plain user data: %v", err)
		return
	}
	if len(out) > limit {
		c.failf("plain user data rendered to %d bytes, above the provider's limit of %d", len(out), limit)
	}
	others := slices.DeleteFunc(slices.Sorted(maps.Keys(base)), func(f string) bool { return f == OneTimeTokenField })
	for _, f := range others {
		for _, sh := range secretShapes() {
			m := maps.Clone(base)
			m[f] = sh.value
			if _, err := render(m); err == nil {
				c.failf("%s in %s was rendered; only %s may hold a secret", sh.name, f, OneTimeTokenField)
			}
		}
	}
	m := maps.Clone(base)
	m[OneTimeTokenField] = rand.Text()
	if _, err := render(m); err != nil {
		c.failf("a one-time token in %s was refused: %v", OneTimeTokenField, err)
	}
	if len(others) > 0 {
		m := maps.Clone(base)
		m[others[0]] = strings.Repeat("a", limit+1)
		if _, err := render(m); err == nil {
			c.failf("user data above the provider's limit of %d bytes was rendered", limit)
		}
	} else {
		c.failf("CloudInit.Fields has no field but %s, so the size limit cannot be checked", OneTimeTokenField)
	}
}

// secretShapes are values any secret lint must refuse, generated per run so
// no credential-shaped literal sits in the source.
func secretShapes() []struct{ name, value string } {
	pem := "PRIVATE KEY-----"
	return []struct{ name, value string }{
		{"a private key", "-----BEGIN OPENSSH " + pem + "\n" + rand.Text() + "\n-----END OPENSSH " + pem},
		{"a bearer token", "Bearer " + rand.Text()},
		{"a password", "password=" + rand.Text()},
	}
}

// 9. SSH: an unknown host key fails under every policy except AcceptNew, and
// password auth is never offered.
func (r *runner) ssh(c *caseT) {
	s := r.s.SSH
	if s == nil {
		c.skipf("Suite.SSH is nil: no SSH client to check")
		return
	}
	for _, want := range []string{PolicyKnownHosts, PolicyPinned, PolicyAcceptNew} {
		if !slices.Contains(s.Policies, want) {
			c.failf("SSH.Policies lacks %s", want)
		}
	}
	for _, pol := range s.Policies {
		var offered []string
		rec, err := guarded(func() error {
			var err error
			offered, err = s.Handshake(r.ctx, pol)
			return err
		})
		switch {
		case rec != "":
			c.failf("Handshake under %s panicked: %s", pol, rec)
			continue
		case pol == PolicyAcceptNew && err != nil:
			c.failf("under %s, a new host key was refused: %v", pol, err)
		case pol != PolicyAcceptNew && err == nil:
			c.failf("under %s, a host key never seen before was accepted", pol)
		}
		for _, m := range offered {
			if m == "password" || m == "keyboard-interactive" {
				c.failf("under %s, the client offered %s authentication", pol, m)
			}
		}
	}
}
