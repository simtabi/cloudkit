package cloudkittest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/simtabi/cloudkit/src"
	"github.com/simtabi/cloudkit/src/cloudkittest"
)

// The test provider is the core tests' fake provider (src/fake_test.go) written
// again against the replayed API in testdata/fake.openapi.json: Probe reads
// GET /token, Inventory GET /resources, and Apply deletes through
// DELETE /resources/{id}. Each field of defects plants the defect one
// contract case exists to catch.

const fakeHost = "api.fake.test"

type defects struct {
	bypassTransport    bool // 1: talks to its own canned transport, not Account.HTTP
	acceptNilClient    bool // 2: builds itself over a nil or default client
	flattenErrors      bool // 3: drops the error chain of a failed request
	probeIgnoresToken  bool // 4: reports writes allowed whatever the token says
	staleLiveState     bool // 5: re-uses the state it planned against
	dropAttached       bool // 6: lists only the target in a destroy's Loses
	leakErrors         bool // 7: returns transport errors unredacted
	renderIgnoresLimit bool // 8: renders user data of any size
	offerPassword      bool // 9: offers password auth over SSH
}

func fakeDescriptor() cloudkit.Descriptor {
	return cloudkit.Descriptor{
		ID:            "fake",
		Title:         "Fake Cloud",
		DocsURL:       "https://docs.fake.test/api",
		APIHosts:      []string{fakeHost},
		RedirectHosts: []string{"objects.fake.test"},
		TokenModel:    cloudkit.ReadOrReadWrite,
		UserDataLimit: 4 << 10,
	}
}

type testProvider struct {
	acct    cloudkit.Account
	client  *http.Client
	defects defects
	cache   *planCache // shared by every provider one factory builds
}

// planCache is where the stale defect keeps the state it planned against.
// It lives in the factory, as an adapter-wide cache would, so it survives the
// fresh provider Registry.Apply opens.
type planCache struct{ live []cloudkit.Resource }

func fakeFactory(d defects) cloudkit.Factory {
	cache := &planCache{}
	return cloudkit.Factory{Descriptor: fakeDescriptor(), New: func(_ context.Context, a cloudkit.Account) (cloudkit.Provider, error) {
		if !d.acceptNilClient && (a.HTTP == nil || a.HTTP.Transport == nil) {
			return nil, errors.New("fake: the account has no guarded client")
		}
		p := &testProvider{acct: a, client: a.HTTP, defects: d, cache: cache}
		if d.bypassTransport {
			p.client = &http.Client{Transport: cannedRT{}}
		}
		return p, nil
	}}
}

// cannedRT answers like the provider without touching any transport the
// caller gave: what an SDK that builds its own client does.
type cannedRT struct{}

func (cannedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	body := `[]`
	if strings.HasSuffix(req.URL.Path, "/token") {
		body = `{"write":true}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (p *testProvider) Descriptor() cloudkit.Descriptor { return fakeDescriptor() }

func (p *testProvider) Probe(ctx context.Context) (cloudkit.CapSet, error) {
	var tok struct{ Write bool }
	if err := p.call(ctx, http.MethodGet, "/v1/token", &tok); err != nil {
		return nil, err
	}
	write := cloudkit.Denied
	if tok.Write || p.defects.probeIgnoresToken {
		write = cloudkit.Allowed
	}
	return cloudkit.CapSet{
		cloudkit.CapInventory:     cloudkit.Allowed,
		cloudkit.CapServerCreate:  write,
		cloudkit.CapServerDestroy: write,
	}, nil
}

func (p *testProvider) Inventory(ctx context.Context, f cloudkit.Filter) ([]cloudkit.Resource, error) {
	var all []cloudkit.Resource
	if err := p.call(ctx, http.MethodGet, "/v1/resources", &all); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(r cloudkit.Resource) bool { return !f.Matches(r) }), nil
}

func (p *testProvider) writable(ctx context.Context) error {
	caps, err := p.Probe(ctx)
	if err != nil {
		return err
	}
	if !caps.Allowed(cloudkit.CapServerCreate) {
		return fmt.Errorf("%w: the token is read only", cloudkit.ErrDenied)
	}
	return nil
}

func (p *testProvider) PlanCreate(ctx context.Context, r cloudkit.Resource) (cloudkit.Plan, error) {
	if err := p.writable(ctx); err != nil {
		return cloudkit.Plan{}, err
	}
	live, err := p.Inventory(ctx, cloudkit.Filter{})
	if err != nil {
		return cloudkit.Plan{}, err
	}
	p.cache.live = live
	return cloudkit.NewPlan(p.acct.Provider, p.acct.Name, []cloudkit.Change{{Action: cloudkit.Create, After: &r}}, nil, live)
}

func (p *testProvider) PlanDestroy(ctx context.Context, id string) (cloudkit.Plan, error) {
	if err := p.writable(ctx); err != nil {
		return cloudkit.Plan{}, err
	}
	live, err := p.Inventory(ctx, cloudkit.Filter{})
	if err != nil {
		return cloudkit.Plan{}, err
	}
	p.cache.live = live
	i := slices.IndexFunc(live, func(r cloudkit.Resource) bool { return r.ID == id })
	if i < 0 {
		return cloudkit.Plan{}, fmt.Errorf("%w: %s", cloudkit.ErrNotFound, id)
	}
	target := live[i]
	loses := []cloudkit.Resource{target}
	if !p.defects.dropAttached {
		for _, r := range live {
			if r.Attrs["attached_to"] == id {
				loses = append(loses, r)
			}
		}
	}
	return cloudkit.NewPlan(p.acct.Provider, p.acct.Name, []cloudkit.Change{{Action: cloudkit.Delete, Before: &target}}, loses, live)
}

func (p *testProvider) LiveState(ctx context.Context, _ cloudkit.Plan) ([]cloudkit.Resource, error) {
	if p.defects.staleLiveState && p.cache.live != nil {
		return p.cache.live, nil
	}
	return p.Inventory(ctx, cloudkit.Filter{})
}

func (p *testProvider) Execute(ctx context.Context, pl cloudkit.Plan) (cloudkit.Result, error) {
	var res cloudkit.Result
	for _, c := range pl.Changes {
		switch c.Action {
		case cloudkit.Create:
			var r cloudkit.Resource
			if err := p.send(ctx, http.MethodPost, "/v1/resources", c.After, &r); err != nil {
				return cloudkit.Result{}, err
			}
			res.Resources = append(res.Resources, r)
		case cloudkit.Delete:
			var out struct{ Action struct{ Status string } }
			if err := p.send(ctx, http.MethodDelete, "/v1/resources/"+c.Before.ID, nil, &out); err != nil {
				return cloudkit.Result{}, err
			}
			if out.Action.Status != "success" {
				return cloudkit.Result{}, fmt.Errorf("fake: delete of %s ended %q", c.Before.ID, out.Action.Status)
			}
		default:
			return cloudkit.Result{}, fmt.Errorf("%w: %s", cloudkit.ErrUnsupported, c.Action)
		}
	}
	return res, nil
}

func (p *testProvider) call(ctx context.Context, method, path string, out any) error {
	return p.send(ctx, method, path, nil, out)
}

func (p *testProvider) send(ctx context.Context, method, path string, in, out any) error {
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
	resp, err := p.client.Do(req)
	if err != nil {
		switch {
		case p.defects.leakErrors:
			return fmt.Errorf("fake: %s %s: %w", method, path, err)
		case p.defects.flattenErrors:
			return errors.New(cloudkit.Redact(err.Error()))
		}
		return cloudkit.RedactError(fmt.Errorf("fake: %s %s: %w", method, path, err))
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	p.acct.Logger.DebugContext(ctx, "fake: call", "method", method, "path", path, "status", resp.StatusCode)
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s %s", cloudkit.ErrDenied, method, path)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s", cloudkit.ErrNotFound, method, path)
	case resp.StatusCode >= 300:
		return fmt.Errorf("fake: %s %s: %d %s", method, path, resp.StatusCode, cloudkit.RedactBody(b))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// A stand-in for the cloud-init renderer, which does not exist yet: enough to show that the cloud-init
// case catches a renderer that lets a secret or an oversized file through.
var reSecretValue = regexp.MustCompile(`(?i)PRIVATE KEY-----|\bbearer\s+\S{8,}|\b(password|passwd|secret)\s*[=:]`)

func standInRender(ignoreLimit bool) func(context.Context, map[string]string, int) ([]byte, error) {
	return func(_ context.Context, fields map[string]string, limit int) ([]byte, error) {
		names := make([]string, 0, len(fields))
		for k := range fields {
			names = append(names, k)
		}
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("#cloud-config\n")
		for _, k := range names {
			if k != cloudkittest.OneTimeTokenField && reSecretValue.MatchString(fields[k]) {
				return nil, fmt.Errorf("user data field %s looks like a secret", k)
			}
			fmt.Fprintf(&b, "# %s: %q\n", k, fields[k])
		}
		if !ignoreLimit && b.Len() > limit {
			return nil, fmt.Errorf("user data is %d bytes, above the provider's %d", b.Len(), limit)
		}
		return []byte(b.String()), nil
	}
}

// A stand-in for the SSH client, for the same reason.
func standInHandshake(offerPassword bool) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, policy string) ([]string, error) {
		offered := []string{"publickey"}
		if offerPassword {
			offered = append(offered, "password")
		}
		if policy != cloudkittest.PolicyAcceptNew {
			return offered, fmt.Errorf("ssh: host key not known under %s", policy)
		}
		return offered, nil
	}
}

func loadFixtures(t *testing.T) (*cloudkittest.Spec, *cloudkittest.Recordings) {
	t.Helper()
	fsys := os.DirFS("testdata")
	spec, err := cloudkittest.LoadSpec(fsys, "fake.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := cloudkittest.LoadRecordings(fsys, "fake.recordings.json", spec)
	if err != nil {
		t.Fatal(err)
	}
	return spec, recs
}

// suite is the contract suite for the test provider with the given defects.
func suite(t *testing.T, d defects) cloudkittest.Suite {
	t.Helper()
	_, recs := loadFixtures(t)
	asFake := func(p cloudkit.Provider) *testProvider { return p.(*testProvider) }
	create := func(ctx context.Context, p cloudkit.Provider) (cloudkit.Plan, error) {
		return asFake(p).PlanCreate(ctx, cloudkit.Resource{Kind: "server", Name: "web-2", Region: "fsn1"})
	}
	destroy := func(ctx context.Context, p cloudkit.Provider) (cloudkit.Plan, error) {
		return asFake(p).PlanDestroy(ctx, "s1")
	}
	return cloudkittest.Suite{
		Factory:    fakeFactory(d),
		Recordings: recs,
		Writes: []cloudkittest.Write{
			{Name: "PlanCreate", Capability: cloudkit.CapServerCreate, Plan: create},
			{Name: "PlanDestroy", Capability: cloudkit.CapServerDestroy, Plan: destroy},
		},
		Drift: create,
		Destroy: &cloudkittest.Destroy{Plan: destroy, Loses: []cloudkit.Resource{
			{Kind: "server", ID: "s1"}, {Kind: "volume", ID: "v1"}, {Kind: "ipv4", ID: "ip1"},
		}},
		CloudInit: &cloudkittest.CloudInit{
			Render: standInRender(d.renderIgnoresLimit),
			Fields: []string{"Hostname", "SSHAuthorizedKey", "Steps", cloudkittest.OneTimeTokenField},
		},
		SSH: &cloudkittest.SSH{
			Policies:  []string{cloudkittest.PolicyKnownHosts, cloudkittest.PolicyPinned, cloudkittest.PolicyFromProvider, cloudkittest.PolicyAcceptNew},
			Handshake: standInHandshake(d.offerPassword),
		},
	}
}
