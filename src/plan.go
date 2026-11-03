package cloudkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PlanTTL is how long a plan stays applicable after [NewPlan] builds it.
const PlanTTL = 15 * time.Minute

// digestVersion is hashed first, so a change to what the digest covers can
// never make an old plan match a new state.
const digestVersion = "cloudkit/plan/v1"

// ChangeAction is what a [Change] does.
type ChangeAction string

// The three kinds of change.
const (
	Create ChangeAction = "create" // After only
	Update ChangeAction = "update" // Before and After
	Delete ChangeAction = "delete" // Before only
)

// Change is one create, update or delete, with the resource before and after.
type Change struct {
	Action ChangeAction `json:"action"`
	Before *Resource    `json:"before,omitempty"` // nil for a create
	After  *Resource    `json:"after,omitempty"`  // nil for a delete
}

func (c Change) validate() error {
	switch c.Action {
	case Create:
		if c.Before != nil || c.After == nil {
			return fmt.Errorf("cloudkit: a create has an After and no Before")
		}
	case Update:
		if c.Before == nil || c.After == nil {
			return fmt.Errorf("cloudkit: an update has a Before and an After")
		}
	case Delete:
		if c.Before == nil || c.After != nil {
			return fmt.Errorf("cloudkit: a delete has a Before and no After")
		}
	default:
		return fmt.Errorf("cloudkit: unknown change action %q", c.Action)
	}
	for _, r := range []*Resource{c.Before, c.After} {
		if r != nil && r.Kind == "" {
			return fmt.Errorf("cloudkit: a %s names a resource with no kind", c.Action)
		}
	}
	if c.Before != nil && c.Before.ID == "" {
		return fmt.Errorf("cloudkit: a %s of a %s names no resource ID", c.Action, c.Before.Kind)
	}
	return nil
}

// Money is an amount in a currency's minor unit (cents for USD and EUR).
type Money struct {
	Minor    int64  // e.g. 499 for 4.99
	Currency string // ISO 4217, e.g. "EUR"
}

// Plan is a write that has not happened yet: the changes, what a destroy
// loses, and a digest over both and the live state they were planned against.
// Build one with [NewPlan]; run it with [Registry.Apply].
type Plan struct {
	ID       string     // stable; derived from the digest. The API and approvals refer to it
	Provider string     // descriptor ID
	Account  string     // the account the plan was made for; Apply opens the same one
	Changes  []Change   // create, update, delete, each with before and after
	Loses    []Resource // for destroy: disks, volumes, IPs (C7)
	Cost     *Money     // monthly delta, when the API reports it; not covered by the digest
	Digest   [32]byte   // over Provider, Account, Changes, Loses and the live state
	Expires  time.Time
}

// NewPlan builds a plan. live is the state the changes were planned against,
// read by the adapter; [Registry.Apply] reads it again through the adapter's
// [Applier.LiveState] and refuses the plan if it moved (C6).
//
// A plan that deletes anything must list in loses every resource it deletes,
// plus whatever goes with them, such as attached volumes and IPs (C7).
func NewPlan(provider, account string, changes []Change, loses, live []Resource) (Plan, error) {
	p := Plan{Provider: provider, Account: account, Changes: changes, Loses: loses}
	if err := p.validate(); err != nil {
		return Plan{}, err
	}
	d, err := digest(p, live)
	if err != nil {
		return Plan{}, err
	}
	p.Digest = d
	p.ID = planID(d)
	p.Expires = time.Now().Add(PlanTTL)
	return p, nil
}

func (p Plan) validate() error {
	if !validID(p.Provider) {
		return fmt.Errorf("cloudkit: plan names an invalid provider %q", p.Provider)
	}
	if p.Account == "" {
		return fmt.Errorf("cloudkit: plan for %s names no account", p.Provider)
	}
	if len(p.Changes) == 0 {
		return fmt.Errorf("cloudkit: plan for %s has no changes", p.Provider)
	}
	for _, c := range p.Changes {
		if err := c.validate(); err != nil {
			return err
		}
		if c.Action != Delete {
			continue
		}
		if !listed(p.Loses, *c.Before) {
			return fmt.Errorf("cloudkit: plan deletes %s %s but does not list it in Loses", c.Before.Kind, c.Before.ID)
		}
		if c.Before.Name == "" {
			return fmt.Errorf("cloudkit: plan deletes %s %s, which has no name to type back", c.Before.Kind, c.Before.ID)
		}
	}
	return nil
}

func listed(loses []Resource, r Resource) bool {
	for _, l := range loses {
		if l.Kind == r.Kind && l.ID == r.ID {
			return true
		}
	}
	return false
}

// IsDestroy reports whether the plan deletes anything.
func (p Plan) IsDestroy() bool {
	for _, c := range p.Changes {
		if c.Action == Delete {
			return true
		}
	}
	return false
}

// ConfirmText is what a person must type back before a destroy plan applies:
// the name of every resource it deletes, in the order of its changes, joined
// by ", ". It is empty for a plan that deletes nothing.
func (p Plan) ConfirmText() string {
	var names []string
	for _, c := range p.Changes {
		if c.Action == Delete && c.Before != nil {
			names = append(names, c.Before.Name)
		}
	}
	return strings.Join(names, ", ")
}

// digest covers everything the person approving the plan was shown, except
// Cost and Expires, and the live state it was planned against. encoding/json
// writes struct fields in order and map keys sorted, so the bytes are
// canonical.
func digest(p Plan, live []Resource) ([32]byte, error) {
	b, err := json.Marshal(struct {
		Version  string     `json:"v"`
		Provider string     `json:"provider"`
		Account  string     `json:"account"`
		Changes  []Change   `json:"changes"`
		Loses    []Resource `json:"loses"`
		Live     []Resource `json:"live"`
	}{digestVersion, p.Provider, p.Account, p.Changes, p.Loses, live})
	if err != nil {
		return [32]byte{}, fmt.Errorf("cloudkit: digest: %w", err)
	}
	return sha256.Sum256(b), nil
}

func planID(d [32]byte) string { return "plan_" + hex.EncodeToString(d[:12]) }

// Confirmation is what the consumer collected from the person approving a
// plan. Approval levels and step-up stay in the consumer; cloudkit only checks
// the typed name.
type Confirmation struct {
	Typed string // must equal Plan.ConfirmText for a destroy plan; ignored otherwise
}

// Result is what an applied plan produced. Apply returns only once the
// provider reports the work done, not merely accepted.
type Result struct {
	PlanID    string
	Resources []Resource // the resources as they are after the changes
}

// Applier is implemented by a provider that can apply plans. LiveState reads
// the current state of everything the plan touches, in the same shape the
// adapter passed to [NewPlan]. Execute makes the changes and waits for the
// provider's actions to finish.
type Applier interface {
	LiveState(ctx context.Context, p Plan) ([]Resource, error)
	Execute(ctx context.Context, p Plan) (Result, error)
}

// Apply runs a plan. Before anything changes it refuses, in this order: a
// plan that is malformed, was altered after it was built or has expired
// ([ErrConflict]); a destroy whose ConfirmText was not typed back
// ([ErrConfirmationRequired]); a provider that cannot apply plans
// ([ErrUnsupported]); and a plan whose live state moved since it was made
// ([ErrConflict], C6). None of those refusals reaches Execute.
func (r *Registry) Apply(ctx context.Context, p Plan, confirm Confirmation) (Result, error) {
	if err := p.validate(); err != nil {
		return Result{}, &PlanError{PlanID: p.ID, Reason: err.Error()}
	}
	if p.Digest == ([32]byte{}) || p.ID != planID(p.Digest) {
		return Result{}, &PlanError{PlanID: p.ID, Reason: "the plan was not built by NewPlan, or its ID was changed"}
	}
	if p.Expires.IsZero() || !r.now().Before(p.Expires) {
		return Result{}, &PlanError{PlanID: p.ID, Reason: "the plan has expired; plan again"}
	}
	if p.IsDestroy() && confirm.Typed != p.ConfirmText() {
		return Result{}, fmt.Errorf("%w: plan %s deletes %q; type the name back to apply it", ErrConfirmationRequired, p.ID, p.ConfirmText())
	}
	prov, err := r.Open(ctx, p.Provider, p.Account)
	if err != nil {
		return Result{}, err
	}
	ap, ok := prov.(Applier)
	if !ok {
		return Result{}, &UnsupportedError{Provider: p.Provider, Interface: "Applier"}
	}
	live, err := ap.LiveState(ctx, p)
	if err != nil {
		return Result{}, redactErr(fmt.Errorf("cloudkit: plan %s: read live state: %w", p.ID, err))
	}
	now, err := digest(p, live)
	if err != nil {
		return Result{}, err
	}
	if now != p.Digest {
		return Result{}, &PlanError{PlanID: p.ID, Reason: "the live state moved since the plan was made; plan again"}
	}
	logger := r.opts.Logger.With("provider", p.Provider, "account", p.Account, "plan", p.ID)
	logger.InfoContext(ctx, "cloudkit: applying plan", "changes", len(p.Changes), "destroy", p.IsDestroy())
	res, err := ap.Execute(ctx, p)
	if err != nil {
		logger.WarnContext(ctx, "cloudkit: plan failed", "error", Redact(err.Error()))
		return Result{}, redactErr(fmt.Errorf("cloudkit: plan %s: %w", p.ID, err))
	}
	res.PlanID = p.ID
	logger.InfoContext(ctx, "cloudkit: plan applied")
	return res, nil
}
