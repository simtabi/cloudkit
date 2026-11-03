package cloudkit

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func server(id, name string) Resource {
	return Resource{Kind: "server", ID: id, Name: name, Region: "fsn1", Attrs: map[string]string{"size": "cx22"}}
}

func attachedVolume(id, to string) Resource {
	return Resource{Kind: "volume", ID: id, Name: id, Attrs: map[string]string{"attached_to": to}}
}

func TestNewPlanRefusesMalformedPlans(t *testing.T) {
	s := server("s1", "web-1")
	nameless := server("s1", "")
	cases := map[string]struct {
		provider, account string
		changes           []Change
		loses             []Resource
	}{
		"bare provider":        {"", "production", []Change{{Action: Create, After: &s}}, nil},
		"bad provider":         {"Fake", "production", []Change{{Action: Create, After: &s}}, nil},
		"no account":           {"fake", "", []Change{{Action: Create, After: &s}}, nil},
		"no changes":           {"fake", "production", nil, nil},
		"unknown action":       {"fake", "production", []Change{{Action: "replace", After: &s}}, nil},
		"create with before":   {"fake", "production", []Change{{Action: Create, Before: &s, After: &s}}, nil},
		"create without":       {"fake", "production", []Change{{Action: Create}}, nil},
		"update without after": {"fake", "production", []Change{{Action: Update, Before: &s}}, nil},
		"delete with after":    {"fake", "production", []Change{{Action: Delete, Before: &s, After: &s}}, []Resource{s}},
		"kindless resource":    {"fake", "production", []Change{{Action: Create, After: &Resource{Name: "x"}}}, nil},
		"before without ID":    {"fake", "production", []Change{{Action: Update, Before: &Resource{Kind: "server"}, After: &s}}, nil},
		// C7: a destroy names what it loses, and something to type back.
		"delete, no loses":            {"fake", "production", []Change{{Action: Delete, Before: &s}}, nil},
		"delete, target not in loses": {"fake", "production", []Change{{Action: Delete, Before: &s}}, []Resource{attachedVolume("v1", "s1")}},
		"delete, no name":             {"fake", "production", []Change{{Action: Delete, Before: &nameless}}, []Resource{nameless}},
	}
	for name, c := range cases {
		if _, err := NewPlan(c.provider, c.account, c.changes, c.loses, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// C6: the digest covers what was planned and the state it was planned
// against, so any difference in either gives a different digest, and the
// same inputs always give the same digest and ID.
func TestPlanDigestCoversEverythingApproved(t *testing.T) {
	s, v := server("s1", "web-1"), attachedVolume("v1", "s1")
	live := []Resource{s, v}
	base := func() (string, string, []Change, []Resource, []Resource) {
		target := s
		return "fake", "production", []Change{{Action: Delete, Before: &target}}, []Resource{s, v}, live
	}
	p0, err := NewPlan(base())
	if err != nil {
		t.Fatal(err)
	}
	again, _ := NewPlan(base())
	if again.Digest != p0.Digest || again.ID != p0.ID {
		t.Fatal("the same plan gave a different digest or ID")
	}
	if !strings.HasPrefix(p0.ID, "plan_") || time.Until(p0.Expires) <= 0 || time.Until(p0.Expires) > PlanTTL {
		t.Fatalf("ID %q, expires in %s", p0.ID, time.Until(p0.Expires))
	}
	variants := map[string]func(*string, *string, *[]Change, *[]Resource, *[]Resource){
		"provider": func(p, _ *string, _ *[]Change, _, _ *[]Resource) { *p = "fake-two" },
		"account":  func(_, a *string, _ *[]Change, _, _ *[]Resource) { *a = "staging" },
		"loses":    func(_, _ *string, _ *[]Change, l, _ *[]Resource) { *l = []Resource{s} },
		"live":     func(_, _ *string, _ *[]Change, _, lv *[]Resource) { *lv = []Resource{s} },
		"live attr": func(_, _ *string, _ *[]Change, _, lv *[]Resource) {
			x := s
			x.Attrs = map[string]string{"size": "cx32"}
			*lv = []Resource{x, v}
		},
		"change": func(_, _ *string, c *[]Change, _, _ *[]Resource) {
			other := s
			other.Name = "web-2"
			*c = []Change{{Action: Delete, Before: &other}}
		},
	}
	for name, mutate := range variants {
		p, a, c, l, lv := base()
		mutate(&p, &a, &c, &l, &lv)
		got, err := NewPlan(p, a, c, l, lv)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Digest == p0.Digest {
			t.Errorf("changing the %s did not change the digest", name)
		}
	}
}

func TestConfirmText(t *testing.T) {
	a, b := server("s1", "web-1"), server("s2", "web-2")
	p, err := NewPlan("fake", "production", []Change{{Action: Delete, Before: &a}, {Action: Delete, Before: &b}}, []Resource{a, b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsDestroy() || p.ConfirmText() != "web-1, web-2" {
		t.Fatalf("IsDestroy %v, ConfirmText %q", p.IsDestroy(), p.ConfirmText())
	}
	c, err := NewPlan("fake", "production", []Change{{Action: Create, After: &a}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.IsDestroy() || c.ConfirmText() != "" {
		t.Fatalf("create: IsDestroy %v, ConfirmText %q", c.IsDestroy(), c.ConfirmText())
	}
}

func TestApplyCreates(t *testing.T) {
	h := newHarness(t, Options{}, server("s1", "web-1"))
	ctx := context.Background()
	p, err := h.open().PlanCreate(ctx, Resource{Kind: "server", Name: "web-2"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.reg.Apply(ctx, p, Confirmation{})
	if err != nil {
		t.Fatal(err)
	}
	if res.PlanID != p.ID || len(res.Resources) != 1 || res.Resources[0].Name != "web-2" || res.Resources[0].ID == "" {
		t.Fatalf("result = %+v", res)
	}
	if h.world.writes() != 1 {
		t.Fatalf("%d writes, want 1", h.world.writes())
	}
	if !strings.Contains(h.logs.String(), p.ID) || strings.Contains(h.logs.String(), h.creds.token) {
		t.Fatalf("logs do not name the plan, or carry the token: %s", h.logs)
	}
}

// C6: a plan whose live state moved is refused, and nothing is written.
func TestApplyRefusesDrift(t *testing.T) {
	h := newHarness(t, Options{}, server("s1", "web-1"))
	ctx := context.Background()
	p, err := h.open().PlanCreate(ctx, Resource{Kind: "server", Name: "web-2"})
	if err != nil {
		t.Fatal(err)
	}
	moved := server("s1", "web-1")
	moved.Attrs["size"] = "cx32" // someone resized it in the console
	h.world.set(moved)
	_, err = h.reg.Apply(ctx, p, Confirmation{})
	var pe *PlanError
	if !errors.Is(err, ErrConflict) || !errors.As(err, &pe) || pe.PlanID != p.ID {
		t.Fatalf("got %v, want a PlanError for %s", err, p.ID)
	}
	if h.world.writes() != 0 {
		t.Fatalf("%d writes after a refused plan", h.world.writes())
	}
}

// C7: a destroy applies only with every deleted name typed back, and a
// refusal makes no request at all.
func TestApplyDestroyNeedsTheNameTypedBack(t *testing.T) {
	h := newHarness(t, Options{}, server("s1", "web-1"), attachedVolume("v1", "s1"), server("s2", "web-2"))
	ctx := context.Background()
	p, err := h.open().PlanDestroy(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Loses) != 2 {
		t.Fatalf("the plan loses %+v, want the server and its volume", p.Loses)
	}
	before := len(h.world.requests())
	for _, typed := range []string{"", "web-2", "WEB-1", "web-1 ", "s1"} {
		_, err := h.reg.Apply(ctx, p, Confirmation{Typed: typed})
		if !errors.Is(err, ErrConfirmationRequired) {
			t.Errorf("typed %q: got %v, want ErrConfirmationRequired", typed, err)
		}
	}
	if n := len(h.world.requests()) - before; n != 0 {
		t.Fatalf("a refused destroy made %d requests", n)
	}
	if _, err := h.reg.Apply(ctx, p, Confirmation{Typed: "web-1"}); err != nil {
		t.Fatal(err)
	}
	left, _ := h.open().Inventory(ctx, Filter{})
	if len(left) != 1 || left[0].ID != "s2" {
		t.Fatalf("left after destroy: %+v", left)
	}
}

// C6 and C7: a plan altered after it was built is refused, whichever part
// was altered, before anything is sent.
func TestApplyRefusesAlteredPlans(t *testing.T) {
	h := newHarness(t, Options{}, server("s1", "web-1"), attachedVolume("v1", "s1"))
	ctx := context.Background()
	fresh := func() Plan {
		prov, err := h.reg.Open(ctx, "fake", "production")
		if err != nil {
			t.Fatal(err)
		}
		p, err := prov.(*fakeProvider).PlanDestroy(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]func(*Plan){
		"loses trimmed": func(p *Plan) { p.Loses = p.Loses[:1] },
		"other target": func(p *Plan) {
			b := *p.Changes[0].Before
			b.ID, b.Name = "s9", "web-9"
			p.Changes = []Change{{Action: Delete, Before: &b}}
			p.Loses = []Resource{b}
		},
		"other account": func(p *Plan) { p.Account = "staging" },
		"ID relabelled": func(p *Plan) { p.ID = "plan_000000000000000000000000" },
		"digest zeroed": func(p *Plan) { p.Digest = [32]byte{} },
		"expired":       func(p *Plan) { p.Expires = time.Now().Add(-time.Second) },
		"never expires": func(p *Plan) { p.Expires = time.Time{} },
		"no changes":    func(p *Plan) { p.Changes = nil },
		"zero plan":     func(p *Plan) { *p = Plan{} },
	}
	for name, mutate := range cases {
		p := fresh()
		mutate(&p)
		before := h.world.writes()
		_, err := h.reg.Apply(ctx, p, Confirmation{Typed: p.ConfirmText()})
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s: got %v, want ErrConflict", name, err)
		}
		if h.world.writes() != before {
			t.Errorf("%s: the altered plan wrote", name)
		}
	}
}

func TestApplyNeedsAnApplier(t *testing.T) {
	d := fakeDescriptor()
	r, err := NewRegistry(validOptions(), Factory{Descriptor: d, New: func(context.Context, Account) (Provider, error) {
		return readOnlyProvider{d}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := server("s1", "web-1")
	p, err := NewPlan("fake", "production", []Change{{Action: Create, After: &s}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Apply(context.Background(), p, Confirmation{})
	var ue *UnsupportedError
	if !errors.Is(err, ErrUnsupported) || !errors.As(err, &ue) || ue.Interface != "Applier" {
		t.Fatalf("got %v, want an UnsupportedError for Applier", err)
	}
	if _, err := r.Apply(context.Background(), Plan{Provider: "nope"}, Confirmation{}); err == nil {
		t.Fatal("a plan for an unknown provider applied")
	}
}

// Errors from the provider leave Apply redacted, and still match.
type leakyProvider struct {
	*fakeProvider
	liveErr, execErr error
}

func (p leakyProvider) LiveState(ctx context.Context, pl Plan) ([]Resource, error) {
	if p.liveErr != nil {
		return nil, p.liveErr
	}
	return p.fakeProvider.LiveState(ctx, pl)
}

func (p leakyProvider) Execute(context.Context, Plan) (Result, error) { return Result{}, p.execErr }

func TestApplyRedactsProviderErrors(t *testing.T) {
	leak := rand.Text()
	cause := errors.New("POST https://api.fake.test/v1/resources?token=" + leak + ": Authorization: Bearer " + leak)
	for name, lp := range map[string]leakyProvider{
		"live state": {liveErr: cause},
		"execute":    {execErr: cause},
	} {
		h := newHarness(t, Options{}, server("s1", "web-1"))
		ctx := context.Background()
		r, err := NewRegistry(Options{Transport: Transport{HTTP: h.world, Dial: noDial}, Credentials: h.creds, Logger: slog.New(slog.NewJSONHandler(h.logs, nil))},
			Factory{Descriptor: fakeDescriptor(), New: func(_ context.Context, a Account) (Provider, error) {
				lp.fakeProvider = &fakeProvider{acct: a}
				return lp, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		p, err := h.open().PlanCreate(ctx, Resource{Kind: "server", Name: "web-2"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Apply(ctx, p, Confirmation{})
		if err == nil || !errors.Is(err, cause) {
			t.Fatalf("%s: got %v, want the cause wrapped", name, err)
		}
		if strings.Contains(err.Error(), leak) || strings.Contains(h.logs.String(), leak) {
			t.Fatalf("%s: the token leaked: %v / %s", name, err, h.logs)
		}
	}
}

// A token that lost its write scope after planning is refused by the
// provider, and the refusal still matches ErrDenied.
func TestApplyAfterTheTokenLostWrite(t *testing.T) {
	h := newHarness(t, Options{}, server("s1", "web-1"))
	ctx := context.Background()
	p, err := h.open().PlanCreate(ctx, Resource{Kind: "server", Name: "web-2"})
	if err != nil {
		t.Fatal(err)
	}
	h.world.readOnly = true
	if _, err := h.reg.Apply(ctx, p, Confirmation{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("got %v, want ErrDenied", err)
	}
}

func TestErrorMessagesNameWhatWasRefused(t *testing.T) {
	for err, want := range map[error]string{
		&PlanError{PlanID: "plan_x", Reason: "moved"}:             "plan plan_x: moved",
		&UnsupportedError{Provider: "fake", Interface: "Applier"}: "fake does not implement Applier",
		&RateLimitError{RetryAfter: 2 * time.Second}:              "retry after 2s",
		&RateLimitError{}: "cloudkit: rate limited",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not contain %q", err.Error(), want)
		}
	}
}
