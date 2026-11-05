package cloudkittest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/simtabi/cloudkit/src"
)

// The nine contract cases, in the order they run.
const (
	CaseTransport    = "transport-only"
	CaseNilTransport = "nil-transport"
	CaseEgress       = "egress"
	CaseReadOnly     = "read-only-token"
	CasePlanDrift    = "plan-drift"
	CaseDestroy      = "destroy"
	CaseRedaction    = "redaction"
	CaseCloudInit    = "cloud-init"
	CaseSSH          = "ssh"
)

// Cases returns the case names in the order [Check] runs them.
func Cases() []string {
	return []string{
		CaseTransport, CaseNilTransport, CaseEgress, CaseReadOnly, CasePlanDrift,
		CaseDestroy, CaseRedaction, CaseCloudInit, CaseSSH,
	}
}

// The cassettes a suite's recordings must hold. Each case replays a fresh
// [Recorder] over one of them and expects the adapter to use every
// interaction, exactly once.
const (
	// CassetteReadWrite holds what Probe and then Inventory send with a token
	// that may write.
	CassetteReadWrite = "read-write"
	// CassetteReadOnly holds what Probe sends with a read-only token, then
	// what each of the suite's Writes sends, in order, before it is refused.
	CassetteReadOnly = "read-only"
	// CassetteDrift holds what the Drift plan sends, then the live-state read
	// Apply makes, which must return a different state. Needed when the
	// suite has Writes.
	CassetteDrift = "drift"
	// CassetteDestroy holds what the Destroy plan sends, then the live-state
	// read and the delete calls of applying it with the names typed back.
	// Needed when the suite has Writes.
	CassetteDestroy = "destroy"
)

// PlanFunc plans one write on an opened provider. It is how the suite reaches
// an adapter's Plan* methods, whose shapes differ by capability.
type PlanFunc func(ctx context.Context, p cloudkit.Provider) (cloudkit.Plan, error)

// Write is one write an adapter offers.
type Write struct {
	Name       string              // e.g. "PlanCreate"
	Capability cloudkit.Capability // the capability the probe reports for it; must be a write
	Plan       PlanFunc
}

// Destroy describes the destroy the destroy case plans.
type Destroy struct {
	Plan PlanFunc
	// Loses names, by kind and ID, the resource the plan deletes and
	// everything attached to it in the destroy cassette (volumes, IPs).
	// The plan's Loses must list every one. At least two entries: the
	// target and one attachment, or the case proves nothing.
	Loses []cloudkit.Resource
}

// OneTimeTokenField is the only user-data field that may hold a secret: a
// single-use, short-lived enrolment token.
const OneTimeTokenField = "OneTimeToken"

// CloudInit is how the cloud-init case reaches the user-data renderer an
// adapter's creates use.
type CloudInit struct {
	// Render renders user data with the named fields set, for a provider that
	// accepts at most limit bytes. It must refuse a secret in any field but
	// OneTimeTokenField, and output above limit.
	Render func(ctx context.Context, fields map[string]string, limit int) ([]byte, error)
	// Fields names every field Render accepts; it must include
	// OneTimeTokenField.
	Fields []string
}

// The host-key policies the SSH case checks.
const (
	PolicyKnownHosts   = "known-hosts"
	PolicyPinned       = "pinned"
	PolicyFromProvider = "from-provider"
	PolicyAcceptNew    = "accept-new"
)

// SSH is how the SSH case reaches the adapter's SSH client.
type SSH struct {
	// Policies names every host-key policy the client offers. It must
	// include PolicyKnownHosts, PolicyPinned and PolicyAcceptNew.
	Policies []string
	// Handshake connects, under the named policy, to a host presenting a key
	// that policy has never seen, and returns the authentication methods the
	// client offered (as SSH names them: "publickey", "password",
	// "keyboard-interactive") and the handshake's error.
	Handshake func(ctx context.Context, policy string) (offered []string, err error)
}

// Suite is one adapter under contract.
type Suite struct {
	// Factory builds the adapter, exactly as a consumer registers it.
	Factory cloudkit.Factory
	// Recordings are the adapter's spec-checked recordings; they must be for
	// the factory's provider ID and hold the cassettes named above.
	Recordings *Recordings
	// Writes is every write the adapter plans. Any capability the read-write
	// probe reports Allowed must have one. Empty for a read-only adapter,
	// which skips the drift and destroy cases.
	Writes []Write
	// Drift plans the write the plan-drift case applies. Required with Writes.
	Drift PlanFunc
	// Destroy is the destroy the destroy case plans. Required with Writes.
	Destroy *Destroy
	// CloudInit is required when the descriptor's UserDataLimit is above 0.
	CloudInit *CloudInit
	// SSH is optional until the SSH path exists; without it the SSH case is
	// skipped, and says so.
	SSH *SSH
}

// Status is a case's outcome.
type Status int

// The three outcomes. Skipped always carries a reason.
const (
	Passed Status = iota + 1
	Failed
	Skipped
)

func (s Status) String() string {
	switch s {
	case Passed:
		return "passed"
	case Failed:
		return "failed"
	case Skipped:
		return "skipped"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// CaseResult is one case's outcome.
type CaseResult struct {
	Name     string
	Status   Status
	Failures []string // why it failed; the run's token never appears in them
	Reason   string   // why it was skipped
}

// Report is the outcome of a [Check]. Err is set when the suite itself is
// unusable, and then no case ran.
type Report struct {
	Err   error
	Cases []CaseResult
}

// Failed reports whether the suite was unusable or any case failed.
func (r Report) Failed() bool {
	if r.Err != nil {
		return true
	}
	for _, c := range r.Cases {
		if c.Status == Failed {
			return true
		}
	}
	return false
}

// Case returns the result of the named case.
func (r Report) Case(name string) (CaseResult, bool) {
	for _, c := range r.Cases {
		if c.Name == name {
			return c, true
		}
	}
	return CaseResult{}, false
}

// Run runs every contract case against the suite's adapter, one subtest per
// case, and fails t on any failure. A skipped case is reported with
// t.Skip and its reason.
func Run(t *testing.T, s Suite) {
	t.Helper()
	rep := Check(t.Context(), s)
	if rep.Err != nil {
		t.Fatalf("cloudkittest: %v", rep.Err)
	}
	for _, c := range rep.Cases {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Status {
			case Skipped:
				t.Skip(c.Reason)
			case Failed:
				for _, f := range c.Failures {
					t.Error(f)
				}
			}
		})
	}
}

// Check runs every contract case and returns the outcome without a
// *testing.T, so a loader (a descriptor that must pass the suite before it
// loads) or a test of the suite itself can read it.
//
// Each case builds its own [cloudkit.Registry] over a fresh transport, a
// token generated for this run, no retries, and a logger that keeps every
// record for the redaction case. Nothing reaches the network: requests are
// answered by a [Recorder] or by a transport that injects a fault, and
// dialing is refused.
func Check(ctx context.Context, s Suite) Report {
	if err := s.validate(); err != nil {
		return Report{Err: err}
	}
	r := &runner{ctx: ctx, s: s, token: rand.Text()}
	cases := []struct {
		name string
		run  func(*caseT)
	}{
		{CaseTransport, r.transportOnly},
		{CaseNilTransport, r.nilTransport},
		{CaseEgress, r.egress},
		{CaseReadOnly, r.readOnly},
		{CasePlanDrift, r.planDrift},
		{CaseDestroy, r.destroy},
		{CaseRedaction, r.redaction},
		{CaseCloudInit, r.cloudInit},
		{CaseSSH, r.ssh},
	}
	rep := Report{}
	for _, c := range cases {
		ct := &caseT{token: r.token}
		func() {
			defer func() {
				if p := recover(); p != nil {
					ct.failf("panicked: %v", p)
				}
			}()
			c.run(ct)
		}()
		rep.Cases = append(rep.Cases, ct.result(c.name))
	}
	return rep
}

func (s Suite) validate() error {
	if err := s.Factory.Descriptor.Validate(); err != nil {
		return fmt.Errorf("suite: %w", err)
	}
	if s.Factory.New == nil {
		return errors.New("suite: the factory has no New function")
	}
	if s.Recordings == nil {
		return errors.New("suite: no recordings; build them with ParseRecordings")
	}
	if got, want := s.Recordings.Provider(), s.Factory.Descriptor.ID; got != want {
		return fmt.Errorf("suite: the recordings are for %q, the factory is %q", got, want)
	}
	need := []string{CassetteReadWrite, CassetteReadOnly}
	if len(s.Writes) > 0 {
		need = append(need, CassetteDrift, CassetteDestroy)
		if s.Drift == nil {
			return errors.New("suite: Writes are set but Drift is nil")
		}
		if s.Destroy == nil || s.Destroy.Plan == nil {
			return errors.New("suite: Writes are set but Destroy has no Plan")
		}
		if len(s.Destroy.Loses) < 2 {
			return errors.New("suite: Destroy.Loses must name the target and at least one attached resource")
		}
	}
	for _, c := range need {
		if s.Recordings.Len(c) == 0 {
			return fmt.Errorf("suite: the recordings have no %q cassette", c)
		}
	}
	for _, w := range s.Writes {
		if w.Name == "" || w.Plan == nil {
			return errors.New("suite: every Write needs a Name and a Plan")
		}
		if !w.Capability.IsWrite() {
			return fmt.Errorf("suite: Write %s names %q, which is not a write capability", w.Name, w.Capability)
		}
	}
	if c := s.CloudInit; c != nil && c.Render == nil {
		return errors.New("suite: CloudInit has no Render")
	}
	if c := s.SSH; c != nil && c.Handshake == nil {
		return errors.New("suite: SSH has no Handshake")
	}
	return nil
}

// caseT collects one case's failures, with the run's token masked out of
// every message, so a report never prints the token even when the adapter
// leaked it.
type caseT struct {
	token    string
	failures []string
	skip     string
}

func (c *caseT) failf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.failures = append(c.failures, strings.ReplaceAll(msg, c.token, "[TOKEN]"))
}

func (c *caseT) skipf(format string, args ...any) { c.skip = fmt.Sprintf(format, args...) }

func (c *caseT) result(name string) CaseResult {
	switch {
	case len(c.failures) > 0:
		return CaseResult{Name: name, Status: Failed, Failures: c.failures}
	case c.skip != "":
		return CaseResult{Name: name, Status: Skipped, Reason: c.skip}
	}
	return CaseResult{Name: name, Status: Passed}
}
