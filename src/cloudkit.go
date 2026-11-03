package cloudkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"time"
)

// CredentialSource returns the token for one provider account. It is called
// per request, so rotation needs no restart, and the module never keeps the
// value (C5).
type CredentialSource interface {
	Token(ctx context.Context, account string) (Secret, error)
}

// Options configures a [Registry]. Transport, Credentials and Logger are
// required; Retry and UserAgent may be left zero.
type Options struct {
	Transport   Transport        // required (C1, C2)
	Credentials CredentialSource // required (C5)
	Logger      *slog.Logger     // required; slog.New(slog.DiscardHandler) to silence (C10)
	Retry       RetryPolicy      // zero value: 3 tries, jittered backoff, honours Retry-After
	UserAgent   string           // e.g. "dbpantry/1.4.0" or "mesobia/0.3.0"; empty leaves the header alone
}

// Validate reports whether o can build a [Registry].
func (o Options) Validate() error {
	if err := o.Transport.Validate(); err != nil {
		return err
	}
	if o.Credentials == nil {
		return errors.New("cloudkit: options: Credentials is nil")
	}
	if o.Logger == nil {
		return errors.New("cloudkit: options: Logger is nil; pass slog.New(slog.DiscardHandler) to silence it")
	}
	return o.Retry.Validate()
}

// TokenModel says how a provider scopes its API tokens.
type TokenModel int

// The token models. The zero value is not one of them, so a descriptor that
// forgets to say is refused.
const (
	Scoped          TokenModel = iota + 1 // scoped tokens per endpoint or permission
	ReadOrReadWrite                       // one token per project, read or read and write
	PerProduct                            // per product: none, read only, read and write
	Unscoped                              // one key reaches everything
)

func (m TokenModel) String() string {
	switch m {
	case Scoped:
		return "scoped"
	case ReadOrReadWrite:
		return "read-or-read-write"
	case PerProduct:
		return "per-product"
	case Unscoped:
		return "unscoped"
	}
	return fmt.Sprintf("TokenModel(%d)", int(m))
}

// Module names the SDK an adapter is built on, for the consumers' inventory
// screens.
type Module struct {
	Path    string // e.g. "github.com/hetznercloud/hcloud-go/v2"
	Version string // e.g. "v2.53.0"
	License string // SPDX identifier, e.g. "MIT"
}

// Descriptor is what an adapter says about itself.
type Descriptor struct {
	ID, Title, DocsURL string
	APIHosts           []string   // C3; e.g. "api.hetzner.cloud"
	RedirectHosts      []string   // C4; redirect targets the adapter expects, if any
	TokenModel         TokenModel // Scoped, ReadOrReadWrite, PerProduct, Unscoped
	UserDataLimit      int        // bytes the provider accepts; 0 means no user data
	SDK                Module     // path, version and licence, for the inventory screen
}

// reID is the shape of a provider ID: lower-case words joined by hyphens.
var reID = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

const maxIDLen = 64

func validID(id string) bool { return len(id) <= maxIDLen && reID.MatchString(id) }

// Validate reports whether d is complete: a lower-case, hyphenated ID, a
// title, an HTTPS docs URL if any, a valid egress policy and a token model.
func (d Descriptor) Validate() error {
	if !validID(d.ID) {
		return fmt.Errorf("cloudkit: descriptor ID %q must be lower-case words joined by hyphens, at most %d bytes", d.ID, maxIDLen)
	}
	if d.Title == "" {
		return fmt.Errorf("cloudkit: descriptor %s has no title", d.ID)
	}
	if d.DocsURL != "" {
		if u, err := url.Parse(d.DocsURL); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("cloudkit: descriptor %s: DocsURL must be an https URL", d.ID)
		}
	}
	if err := d.Egress().Validate(); err != nil {
		return fmt.Errorf("cloudkit: descriptor %s: %w", d.ID, err)
	}
	if d.TokenModel < Scoped || d.TokenModel > Unscoped {
		return fmt.Errorf("cloudkit: descriptor %s has no token model", d.ID)
	}
	if d.UserDataLimit < 0 {
		return fmt.Errorf("cloudkit: descriptor %s: UserDataLimit is negative", d.ID)
	}
	return nil
}

// Egress returns the egress policy the descriptor declares.
func (d Descriptor) Egress() EgressPolicy {
	return EgressPolicy{APIHosts: slices.Clone(d.APIHosts), RedirectHosts: slices.Clone(d.RedirectHosts)}
}

// Resource is one thing a provider holds: a server, volume, IP, firewall,
// snapshot, bucket or DNS record.
type Resource struct {
	Kind   string            `json:"kind"`             // e.g. "server", "volume", "ipv4"
	ID     string            `json:"id,omitempty"`     // the provider's ID; empty before a create
	Name   string            `json:"name,omitempty"`   // the name a person types back to destroy it
	Region string            `json:"region,omitempty"` // the provider's region or location code
	Labels map[string]string `json:"labels,omitempty"` // provider labels or tags
	Attrs  map[string]string `json:"attrs,omitempty"`  // facts a plan depends on: size, image, status
}

// Filter narrows an inventory. A zero Filter matches everything.
type Filter struct {
	Kinds  []string          // match any of these kinds; empty matches all
	Region string            // empty matches all
	Labels map[string]string // every label must be present with this value
}

// Matches reports whether r passes the filter.
func (f Filter) Matches(r Resource) bool {
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, r.Kind) {
		return false
	}
	if f.Region != "" && f.Region != r.Region {
		return false
	}
	for k, v := range f.Labels {
		if got, ok := r.Labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// Provider is the only interface every adapter implements. The capability
// interfaces are optional, found by type assertion.
type Provider interface {
	Descriptor() Descriptor
	Probe(ctx context.Context) (CapSet, error) // with the real token
	Inventory(ctx context.Context, f Filter) ([]Resource, error)
}

// Account is what an adapter gets for one provider account when the registry
// opens it. HTTP is the only client the adapter may use: it is limited to the
// descriptor's hosts (C3, C4), adds the account's token to each request from
// the CredentialSource (C5), and retries by the registry's RetryPolicy.
type Account struct {
	Provider string       // the descriptor ID
	Name     string       // the account name the caller passed to Open
	HTTP     *http.Client // guarded and authenticated; never replace its Transport
	Logger   *slog.Logger // the registry's logger, with provider and account attributes
}

// Factory builds a provider for one account. The caller passes factories to
// [NewRegistry]; nothing registers itself (C9).
type Factory struct {
	Descriptor Descriptor
	New        func(ctx context.Context, a Account) (Provider, error)
}

// Registry opens providers and applies plans. The caller builds it with
// [NewRegistry]; there is no package-level registry (C9). It is safe for
// concurrent use and holds no client between calls, so two accounts never
// share one.
type Registry struct {
	opts      Options
	factories map[string]Factory
	order     []string
	now       func() time.Time                                 // replaced in tests
	sleep     func(ctx context.Context, d time.Duration) error // replaced in tests
}

// NewRegistry builds a registry over the given factories. It refuses invalid
// options (a nil or default transport among them, C2), a factory whose
// descriptor is invalid or whose New is nil, and two factories with one ID.
func NewRegistry(o Options, factories ...Factory) (*Registry, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	r := &Registry{opts: o, factories: make(map[string]Factory, len(factories)), now: time.Now, sleep: sleepCtx}
	for _, f := range factories {
		if err := f.Descriptor.Validate(); err != nil {
			return nil, err
		}
		if f.New == nil {
			return nil, fmt.Errorf("cloudkit: factory %s has no New function", f.Descriptor.ID)
		}
		if _, dup := r.factories[f.Descriptor.ID]; dup {
			return nil, fmt.Errorf("cloudkit: two factories have the ID %q", f.Descriptor.ID)
		}
		f.Descriptor = cloneDescriptor(f.Descriptor)
		r.factories[f.Descriptor.ID] = f
		r.order = append(r.order, f.Descriptor.ID)
	}
	return r, nil
}

// Descriptors returns the descriptor of every factory, in the order given to
// [NewRegistry].
func (r *Registry) Descriptors() []Descriptor {
	out := make([]Descriptor, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, cloneDescriptor(r.factories[id].Descriptor))
	}
	return out
}

// Open builds a provider for one account, with a fresh guarded and
// authenticated client. It returns [ErrNotFound] for an unknown provider.
func (r *Registry) Open(ctx context.Context, provider, account string) (Provider, error) {
	f, ok := r.factories[provider]
	if !ok {
		return nil, fmt.Errorf("%w: no provider %q in the registry", ErrNotFound, provider)
	}
	if account == "" {
		return nil, fmt.Errorf("cloudkit: open %s: account is empty", provider)
	}
	client, err := r.client(f.Descriptor, account)
	if err != nil {
		return nil, err
	}
	p, err := f.New(ctx, Account{
		Provider: provider,
		Name:     account,
		HTTP:     client,
		Logger:   r.opts.Logger.With("provider", provider, "account", account),
	})
	if err != nil {
		return nil, redactErr(fmt.Errorf("cloudkit: open %s: %w", provider, err))
	}
	if p == nil {
		return nil, fmt.Errorf("cloudkit: open %s: factory returned no provider", provider)
	}
	if got := p.Descriptor().ID; got != provider {
		return nil, fmt.Errorf("cloudkit: open %s: provider describes itself as %q", provider, got)
	}
	return p, nil
}

// client builds the http.Client an adapter uses: the egress guard outermost,
// so a refused host never reaches the retry loop or the credential source,
// then retries, then the token, then the caller's round tripper.
func (r *Registry) client(d Descriptor, account string) (*http.Client, error) {
	auth := &authenticator{
		next:      r.opts.Transport.HTTP,
		creds:     r.opts.Credentials,
		account:   account,
		hosts:     normalise(d.APIHosts),
		userAgent: r.opts.UserAgent,
	}
	retry := &retrier{next: auth, policy: r.opts.Retry, sleep: r.sleep}
	return NewHTTPClient(Transport{HTTP: retry, Dial: r.opts.Transport.Dial}, d.Egress())
}

func cloneDescriptor(d Descriptor) Descriptor {
	d.APIHosts = slices.Clone(d.APIHosts)
	d.RedirectHosts = slices.Clone(d.RedirectHosts)
	return d
}
