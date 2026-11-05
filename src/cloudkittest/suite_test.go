package cloudkittest_test

import (
	"strings"
	"testing"

	"github.com/simtabi/cloudkit/src/cloudkittest"
)

// The healthy test provider passes all nine cases, through Run itself.
func TestTheTestProviderPassesTheSuite(t *testing.T) {
	cloudkittest.Run(t, suite(t, defects{}))
}

// Check reports all nine cases, passed, and none skipped: a skip would hide a
// case the healthy provider is meant to exercise.
func TestCheckReportsEveryCase(t *testing.T) {
	rep := cloudkittest.Check(t.Context(), suite(t, defects{}))
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	if len(rep.Cases) != 9 || len(cloudkittest.Cases()) != 9 {
		t.Fatalf("got %d cases (Cases lists %d), want nine", len(rep.Cases), len(cloudkittest.Cases()))
	}
	for i, c := range rep.Cases {
		if c.Name != cloudkittest.Cases()[i] {
			t.Errorf("case %d is %s, want %s", i, c.Name, cloudkittest.Cases()[i])
		}
		if c.Status != cloudkittest.Passed {
			t.Errorf("%s: %s %v %s", c.Name, c.Status, c.Failures, c.Reason)
		}
	}
}

// One deliberately broken provider per case: each must fail the case written
// to catch it. A case that cannot fail is not known to work.
func TestEachCaseCatchesItsDefect(t *testing.T) {
	broken := []struct {
		name string
		d    defects
		want string // the failure must say this
	}{
		{cloudkittest.CaseTransport, defects{bypassTransport: true}, "sent nothing through the injected transport"},
		{cloudkittest.CaseNilTransport, defects{acceptNilClient: true}, "Factory.New accepted a nil client"},
		{cloudkittest.CaseEgress, defects{flattenErrors: true}, "want an error matching ErrEgressDenied"},
		{cloudkittest.CaseReadOnly, defects{probeIgnoresToken: true}, "with a read-only token, want denied"},
		{cloudkittest.CasePlanDrift, defects{staleLiveState: true}, "want ErrConflict"},
		{cloudkittest.CaseDestroy, defects{dropAttached: true}, "does not list volume v1 in Loses"},
		{cloudkittest.CaseRedaction, defects{leakErrors: true}, "the token appears in"},
		{cloudkittest.CaseCloudInit, defects{renderIgnoresLimit: true}, "above the provider's limit"},
		{cloudkittest.CaseSSH, defects{offerPassword: true}, "offered password authentication"},
	}
	if len(broken) != len(cloudkittest.Cases()) {
		t.Fatalf("%d broken variants for %d cases", len(broken), len(cloudkittest.Cases()))
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			rep := cloudkittest.Check(t.Context(), suite(t, b.d))
			if rep.Err != nil {
				t.Fatal(rep.Err)
			}
			c, ok := rep.Case(b.name)
			if !ok {
				t.Fatalf("no result for %s", b.name)
			}
			if c.Status != cloudkittest.Failed {
				t.Fatalf("%s %s with its defect planted; want failed", b.name, c.Status)
			}
			if !strings.Contains(strings.Join(c.Failures, "\n"), b.want) {
				t.Errorf("%s failed, but not for its defect:\n%s", b.name, strings.Join(c.Failures, "\n"))
			}
			if !rep.Failed() {
				t.Error("Report.Failed is false with a failed case")
			}
		})
	}
}

// The redaction case's own failure messages never print the token, even when
// the adapter leaked it.
func TestFailuresNeverPrintTheToken(t *testing.T) {
	rep := cloudkittest.Check(t.Context(), suite(t, defects{leakErrors: true}))
	c, _ := rep.Case(cloudkittest.CaseRedaction)
	for _, f := range c.Failures {
		if strings.Contains(f, "Bearer ") && !strings.Contains(f, "[TOKEN]") && !strings.Contains(f, "[REDACTED]") {
			t.Errorf("a failure may carry the token: %q", f)
		}
	}
	if len(c.Failures) == 0 {
		t.Fatal("the leaking provider passed the redaction case")
	}
}

// An unusable suite is reported as such, and no case runs.
func TestCheckRefusesAnUnusableSuite(t *testing.T) {
	mutations := map[string]func(*cloudkittest.Suite){
		"no recordings":       func(s *cloudkittest.Suite) { s.Recordings = nil },
		"no New":              func(s *cloudkittest.Suite) { s.Factory.New = nil },
		"wrong provider":      func(s *cloudkittest.Suite) { s.Factory.Descriptor.ID = "other" },
		"writes but no drift": func(s *cloudkittest.Suite) { s.Drift = nil },
		"writes, no destroy":  func(s *cloudkittest.Suite) { s.Destroy = nil },
		"vacuous loses":       func(s *cloudkittest.Suite) { s.Destroy.Loses = s.Destroy.Loses[:1] },
		"read as write":       func(s *cloudkittest.Suite) { s.Writes[0].Capability = "inventory.read" },
		"cloud-init, no fn":   func(s *cloudkittest.Suite) { s.CloudInit.Render = nil },
		"ssh, no fn":          func(s *cloudkittest.Suite) { s.SSH.Handshake = nil },
	}
	for name, mutate := range mutations {
		s := suite(t, defects{})
		mutate(&s)
		rep := cloudkittest.Check(t.Context(), s)
		if rep.Err == nil || len(rep.Cases) != 0 || !rep.Failed() {
			t.Errorf("%s: err %v, %d cases ran", name, rep.Err, len(rep.Cases))
		}
	}
}

// Without an SSH client the SSH case is skipped and says why; without a
// renderer for a provider that takes user data, the cloud-init case fails.
func TestOptionalCases(t *testing.T) {
	s := suite(t, defects{})
	s.SSH = nil
	rep := cloudkittest.Check(t.Context(), s)
	if c, _ := rep.Case(cloudkittest.CaseSSH); c.Status != cloudkittest.Skipped || c.Reason == "" {
		t.Errorf("no SSH hook: %s %q, want skipped with a reason", c.Status, c.Reason)
	}
	s = suite(t, defects{})
	s.CloudInit = nil
	rep = cloudkittest.Check(t.Context(), s)
	if c, _ := rep.Case(cloudkittest.CaseCloudInit); c.Status != cloudkittest.Failed {
		t.Errorf("no renderer, UserDataLimit > 0: %s, want failed", c.Status)
	}
}
