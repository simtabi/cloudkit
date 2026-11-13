// Package policy holds tests that read the whole repository rather than one
// package: the module's network and global-state rules, and the release
// workflow guard. They are tests, not a linter, so `make ci` cannot skip them.
package policy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// minGoFiles is a floor on how many Go files the scan reads. A walk that
// stops matching would otherwise report a clean module. Raise it as the
// module grows; never lower it to make a failure go away.
const minGoFiles = 28

// forbidden are the symbols that would let a request or a session bypass the
// caller's transport, egress controls or host-key checks, or that would give
// the module global state. Each names the rule it enforces.
var forbidden = []struct {
	pattern *regexp.Regexp
	rule    string
}{
	{regexp.MustCompile(`\bhttp\.DefaultClient\b`), "C1: use the caller's Transport"},
	{regexp.MustCompile(`\bhttp\.DefaultTransport\b`), "C1: use the caller's Transport"},
	{regexp.MustCompile(`\bhttp\.(Get|Head|Post|PostForm)\(`), "C1: these use http.DefaultClient"},
	{regexp.MustCompile(`\bnet\.(Dial|DialTimeout)\(`), "C1: dial through Transport.Dial"},
	{regexp.MustCompile(`\bProxyFromEnvironment\b`), "C1: no proxy from the environment"},
	{regexp.MustCompile(`\bInsecureSkipVerify\b`), "C4: TLS is always verified"},
	{regexp.MustCompile(`\bInsecureIgnoreHostKey\b`), "C8: SSH host keys are always verified"},
	{regexp.MustCompile(`\bPasswordCallback\b|\bssh\.Password\(`), "C8: no SSH password auth"},
	{regexp.MustCompile(`(?m)^func init\(\)`), "C9: no init registration"},
	{regexp.MustCompile(`(?m)^var\s+\w+\s*(=\s*)?[&*]?(http\.Client|Registry|NewRegistry\(|NewHTTPClient\()`), "C9: no package-level clients or registries"},
	{regexp.MustCompile(`\bslog\.(Default|SetDefault|Info|Warn|Error|Debug)\(|\blog\.(Print|Fatal|Panic)`), "C10: log through the injected *slog.Logger"},
}

// exempt lists the only places a forbidden symbol may appear, each with its
// reason and the exact number of occurrences measured. A count that drifts in
// either direction fails, so an entry cannot quietly cover a new use.
var exempt = map[string]map[string]int{
	// Validate and validateHTTP compare against it to refuse it.
	"transport.go": {`\bhttp\.DefaultTransport\b`: 2},
	// NewHTTPClient's tests hand it in to prove it is refused.
	"cloudkit_test.go": {`\bhttp\.DefaultTransport\b`: 2},
	// NewRegistry's tests hand it in to prove it is refused (C2).
	"core_test.go": {`\bhttp\.DefaultTransport\b`: 1},
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	return root
}

func TestNoForbiddenSymbols(t *testing.T) {
	root := moduleRoot(t)
	self, err := filepath.Abs("policy_test.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || path == self {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // a file of this module, found by the walk
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for _, f := range forbidden {
			n := len(f.pattern.FindAllIndex(b, -1))
			want := exempt[filepath.ToSlash(rel)][f.pattern.String()]
			if n != want {
				t.Errorf("%s: %d use(s) of %s, %d allowed (%s)", rel, n, f.pattern, want, f.rule)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < minGoFiles {
		t.Fatalf("scanned %d Go files, below the floor of %d: the walk is not reading the module", scanned, minGoFiles)
	}
}

// Every exemption must still name a file that exists, or it is stale.
func TestExemptionsAreLive(t *testing.T) {
	root := moduleRoot(t)
	for file := range exempt {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(file))); err != nil {
			t.Errorf("exemption for %s names a missing file", file)
		}
	}
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(moduleRoot(t)), ".github", "workflows", name)) //nolint:gosec // a workflow of this repository
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// onBlock returns the trigger names under release.yml's top-level `on:` and
// the text of each, read by indentation: the file is plain block YAML and the
// module has no YAML dependency to parse it with.
func onBlock(t *testing.T, wf string) map[string]string {
	t.Helper()
	triggers := map[string]string{}
	in, current := false, ""
	top := regexp.MustCompile(`^[A-Za-z_]+:`)
	key := regexp.MustCompile(`^  ([A-Za-z_]+):`)
	for _, line := range strings.Split(wf, "\n") {
		switch {
		case line == "on:":
			in = true
		case in && top.MatchString(line):
			in = false
		case in:
			if m := key.FindStringSubmatch(line); m != nil {
				current = m[1]
				triggers[current] = ""
			} else if current != "" {
				triggers[current] += line + "\n"
			}
		}
	}
	if len(triggers) == 0 {
		t.Fatal("release.yml has no block-style `on:` with triggers under it")
	}
	return triggers
}

// A release runs on a pushed version tag, or on request for a tag already
// pushed (owner, 2026-11-13), and on nothing else: never a branch push, a
// schedule, a pull request or a release event. cloudkit is the library
// exception to the shared simtabi/.github go-release.yml, so its own job
// resolves, checks and checks out the tag it releases.
func TestReleaseRunsOnAVersionTagOrOnRequest(t *testing.T) {
	wf := readWorkflow(t, "release.yml")
	triggers := onBlock(t, wf)
	names := make([]string, 0, len(triggers))
	for name := range triggers {
		names = append(names, name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "push,workflow_dispatch" {
		t.Fatalf("release.yml triggers on %s; want exactly push (version tags) and workflow_dispatch", got)
	}

	push := triggers["push"]
	if regexp.MustCompile(`(?m)^\s*(branches|branches-ignore|tags-ignore|paths|paths-ignore)\s*:`).MatchString(push) {
		t.Error("release.yml's push trigger filters branches or paths; it must fire on version tags only")
	}
	filters := regexp.MustCompile(`(?m)^\s*-\s*'([^']+)'`).FindAllStringSubmatch(push, -1)
	if !strings.Contains(push, "tags:") || len(filters) == 0 {
		t.Fatal("release.yml's push trigger has no tag filters")
	}
	for _, f := range filters {
		if !strings.HasPrefix(f[1], "v[0-9]+.") {
			t.Errorf("release.yml tag filter %q is not a version tag", f[1])
		}
	}

	dispatch := triggers["workflow_dispatch"]
	if !regexp.MustCompile(`(?m)^\s+tag:\s*$`).MatchString(dispatch) ||
		!regexp.MustCompile(`(?m)^\s+required:\s*true\s*$`).MatchString(dispatch) {
		t.Error("release.yml's workflow_dispatch lacks a required tag input")
	}

	for _, want := range []string{
		"${{ inputs.tag || github.ref_name }}",           // the tag, from either trigger
		"refs/tags/${{ inputs.tag || github.ref_name }}", // the release is built from the tag, not a branch
		`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`,    // a version, or a pre-release
		"git/ref/tags/${TAG}",                            // that exists on GitHub
		"cancel-in-progress: false",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml lacks %q", want)
		}
	}
	if strings.Contains(wf, "release-go") {
		t.Error("release.yml still carries the retired confirm=release-go gate")
	}
	// A library has no binaries; nothing in the release may build or publish
	// one. Comments are skipped: the header says why the shared one is not used.
	code := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(wf, "")
	for _, never := range []string{"goreleaser", "go-release.yml", "upload-artifact", "docker/", "homebrew", "scoop"} {
		if strings.Contains(strings.ToLower(code), never) {
			t.Errorf("release.yml mentions %q; cloudkit releases are git tags and a release body only", never)
		}
	}
}

// CI runs on pull requests, by hand and weekly, never on a branch push.
func TestCIRunsOnPullRequestsNotPushes(t *testing.T) {
	wf := readWorkflow(t, "ci.yml")
	for _, want := range []string{"pull_request:", "workflow_dispatch:", "schedule:"} {
		if !strings.Contains(wf, want) {
			t.Errorf("ci.yml lacks %q", want)
		}
	}
	if regexp.MustCompile(`(?m)^[ \t]*push\s*:`).MatchString(wf) {
		t.Error("ci.yml triggers on push; CI runs on pull_request plus a weekly schedule")
	}
	if n := strings.Count(wf, "cron:"); n != 1 {
		t.Errorf("ci.yml has %d crons, want exactly one weekly", n)
	}
	// Jobs skip drafts, so a PR marked ready must start a run, or its required
	// checks report "skipped" and the ruleset lets it merge untested.
	if strings.Contains(wf, "pull_request.draft") &&
		!regexp.MustCompile(`(?m)^[ \t]*types:\s*\[[^\]]*\bready_for_review\b`).MatchString(wf) {
		t.Error("ci.yml skips drafts but its pull_request trigger lacks ready_for_review")
	}
}

// Third-party actions are pinned to a commit SHA with a version comment;
// first-party ones (actions/, github/, docker/) float on a major tag.
func TestActionPins(t *testing.T) {
	re := regexp.MustCompile(`uses:\s*([A-Za-z0-9._-]+/[A-Za-z0-9._/-]+)@([^\s#]+)(\s*#\s*(\S+))?`)
	firstParty := regexp.MustCompile(`^(actions|github|docker)/`)
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	release := regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)
	major := regexp.MustCompile(`^v\d+$`)
	seen := 0
	for _, name := range []string{"ci.yml", "release.yml"} {
		for _, m := range re.FindAllStringSubmatch(readWorkflow(t, name), -1) {
			seen++
			action, ref, comment := m[1], m[2], m[4]
			if firstParty.MatchString(action) {
				if !major.MatchString(ref) {
					t.Errorf("%s: first-party %s@%s must float on a major tag", name, action, ref)
				}
				continue
			}
			if !sha.MatchString(ref) || !release.MatchString(comment) {
				t.Errorf("%s: third-party %s@%s must be a SHA with a # x.y.z comment", name, action, ref)
			}
		}
	}
	if seen < 3 {
		t.Fatalf("read %d action references, below the floor of 3", seen)
	}
}

// shippedPackages are the packages whose syntax the AST guards read: the
// directory under the module root, the package name, and a floor on how many
// non-test files they must find.
var shippedPackages = []struct {
	dir, name string
	minFiles  int
}{
	{".", "cloudkit", 11},
	{"cloudkittest", "cloudkittest", 7},
}

// parsePackage parses the non-test files of package cloudkit, at the module
// root, and fails below a floor so a parse that finds nothing cannot pass.
func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	p := shippedPackages[0]
	return parseDir(t, p.dir, p.name, p.minFiles)
}

// parseDir parses the non-test files of one package and fails below a floor.
func parseDir(t *testing.T, dir, name string, minSourceFiles int) []*ast.File {
	t.Helper()
	root := moduleRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, m, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name.Name == name {
			files = append(files, f)
		}
	}
	if len(files) < minSourceFiles {
		t.Fatalf("parsed %d source files of package %s, below the floor of %d", len(files), name, minSourceFiles)
	}
	return files
}

// C10: every method of an exported interface, every method of *Registry,
// every exported func type and every exported func-typed field takes a
// context.Context first, so the caller owns cancellation. It reads every
// shipped package, cloudkittest's hooks included. The exemptions are pure accessors that reach
// nothing, each with its reason.
var contextExempt = map[string]string{
	"Provider.Descriptor":  "returns a constant description; no I/O",
	"Registry.Descriptors": "returns the factories' descriptions; no I/O",
}

func TestContextFirst(t *testing.T) {
	checked := 0
	check := func(name string, ft *ast.FuncType) {
		checked++
		if _, ok := contextExempt[name]; ok {
			return
		}
		ps := ft.Params.List
		if len(ps) == 0 || !isContext(ps[0].Type) {
			t.Errorf("%s does not take context.Context first (C10)", name)
		}
	}
	var files []*ast.File
	for _, p := range shippedPackages {
		files = append(files, parseDir(t, p.dir, p.name, p.minFiles)...)
	}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil && d.Name.IsExported() && recvName(d.Recv) == "Registry" {
					check("Registry."+d.Name.Name, d.Type)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || !ts.Name.IsExported() {
						continue
					}
					switch tt := ts.Type.(type) {
					case *ast.FuncType:
						check(ts.Name.Name, tt)
					case *ast.InterfaceType:
						for _, m := range tt.Methods.List {
							if ft, ok := m.Type.(*ast.FuncType); ok {
								check(ts.Name.Name+"."+m.Names[0].Name, ft)
							}
						}
					case *ast.StructType:
						for _, fld := range tt.Fields.List {
							ft, ok := fld.Type.(*ast.FuncType)
							if !ok {
								continue
							}
							for _, n := range fld.Names {
								if n.IsExported() {
									check(ts.Name.Name+"."+n.Name, ft)
								}
							}
						}
					}
				}
			}
		}
	}
	// Measured 2026-11-04: Registry.{Open,Apply,Descriptors}, Provider's 3,
	// Applier's 2, CredentialSource.Token, Factory.New and Transport.Dial;
	// cloudkittest's PlanFunc, CloudInit.Render and SSH.Handshake.
	if checked < 14 {
		t.Fatalf("checked %d signatures, below the floor of 14", checked)
	}
	for name := range contextExempt {
		if !strings.Contains(name, ".") {
			t.Errorf("exemption %q is not Type.Method", name)
		}
	}
}

func isContext(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "context" && sel.Sel.Name == "Context"
}

func recvName(fl *ast.FieldList) string {
	e := fl.List[0].Type
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// shipped is every exported name package cloudkit has carried. Removing one
// breaks a caller this repository cannot see, so it is deprecated in place
// instead; this test fails if one disappears. Append, never delete.
var shipped = []string{
	// Phase 0, c1a6a98.
	"Transport", "Transport.Validate", "EgressPolicy", "EgressPolicy.Validate", "NewHTTPClient",
	"Secret", "NewSecret", "Secret.Reveal", "Secret.IsZero", "Secret.String", "Secret.GoString",
	"Secret.Format", "Secret.LogValue", "Secret.MarshalJSON", "Secret.MarshalText",
	"ErrInvalidTransport", "ErrDenied", "ErrNotFound", "ErrConflict", "ErrRateLimited",
	"ErrEgressDenied", "ErrUnsupported", "EgressError", "EgressError.Error", "EgressError.Unwrap",
	"Redact", "RedactBody", "MaxBodyBytes",
	// Phase 1, core.
	"CredentialSource", "Options", "Options.Validate", "TokenModel", "TokenModel.String",
	"Scoped", "ReadOrReadWrite", "PerProduct", "Unscoped", "Module", "Descriptor",
	"Descriptor.Validate", "Descriptor.Egress", "Resource", "Filter", "Filter.Matches", "Provider",
	"Account", "Factory", "Registry", "NewRegistry", "Registry.Descriptors", "Registry.Open",
	"Registry.Apply", "Capability", "Capability.IsWrite", "Access", "Access.String", "Unknown",
	"Allowed", "Denied", "CapSet", "CapSet.Get", "CapSet.Allowed", "CapSet.Set",
	"CapSet.Capabilities", "CapInventory", "CapServerCreate", "CapServerResize", "CapServerDestroy",
	"CapServerPower", "CapServerUpdate", "CapFirewallRead", "CapFirewallWrite", "CapNetworkRead",
	"CapNetworkWrite", "CapVolumeRead", "CapVolumeWrite", "CapSnapshotRead", "CapSnapshotWrite",
	"CapObjectStorageRead", "CapObjectStorageWrite", "CapDNSRead", "CapDNSWrite", "CapHostKeysRead",
	"CapCostRead", "CapManagedDatabasesRead", "CapManagedDatabasesConnect", "PlanTTL",
	"ChangeAction", "Create", "Update", "Delete", "Change", "Money", "Plan", "NewPlan",
	"Plan.IsDestroy", "Plan.ConfirmText", "Confirmation", "Result", "Applier",
	"ErrConfirmationRequired", "PlanError", "PlanError.Error", "PlanError.Unwrap",
	"UnsupportedError", "UnsupportedError.Error", "UnsupportedError.Unwrap", "RetryPolicy",
	"RetryPolicy.Validate", "RetryPolicy.Backoff", "RateLimitError", "RateLimitError.Error",
	"RateLimitError.Unwrap", "DefaultMaxAttempts", "DefaultBaseDelay", "DefaultMaxDelay",
	// Phase 2, contract kit.
	"RedactError",
}

// shippedKit is every exported name package cloudkittest has carried, under
// the same rule as shipped. Append, never delete.
var shippedKit = []string{
	// Phase 2, contract kit.
	"Run", "Check", "Cases", "Suite", "Write", "PlanFunc", "Destroy", "CloudInit", "SSH",
	"OneTimeTokenField", "PolicyKnownHosts", "PolicyPinned", "PolicyFromProvider", "PolicyAcceptNew",
	"CaseTransport", "CaseNilTransport", "CaseEgress", "CaseReadOnly", "CasePlanDrift", "CaseDestroy",
	"CaseRedaction", "CaseCloudInit", "CaseSSH", "CassetteReadWrite", "CassetteReadOnly",
	"CassetteDrift", "CassetteDestroy", "Status", "Status.String", "Passed", "Failed", "Skipped",
	"CaseResult", "Report", "Report.Failed", "Report.Case",
	"Spec", "ParseSpec", "LoadSpec", "Spec.ValidateResponse", "Spec.Synthesize", "Response",
	"Recordings", "ParseRecordings", "LoadRecordings", "Recordings.Provider", "Recordings.Cassettes",
	"Recordings.Len", "Recorder", "NewRecorder", "Recorder.RoundTrip", "Recorder.Requests",
	"Recorder.Unexpected", "Recorder.Unused", "Recorder.Err", "Request", "UnexpectedRequestError",
	"UnexpectedRequestError.Error", "UnexpectedRequestError.Unwrap", "SchemaError", "SchemaError.Error",
	"SchemaError.Unwrap", "ErrSchemaViolation", "ErrUnsupportedKeyword", "ErrNotInSpec",
	"ErrCredentialInRecording", "ErrUnexpectedRequest",
}

func TestShippedSurfaceIsPreserved(t *testing.T) {
	checkSurface(t, parsePackage(t), shipped)
	kit := shippedPackages[1]
	checkSurface(t, parseDir(t, kit.dir, kit.name, kit.minFiles), shippedKit)
}

func checkSurface(t *testing.T, files []*ast.File, shipped []string) {
	t.Helper()
	have := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				name := d.Name.Name
				if d.Recv != nil {
					name = recvName(d.Recv) + "." + name
				}
				have[name] = true
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						have[sp.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							have[n.Name] = true
						}
					}
				}
			}
		}
	}
	for _, name := range shipped {
		if !have[name] {
			t.Errorf("exported %s has gone; deprecate it in place instead of removing it", name)
		}
	}
	if len(have) < len(shipped) {
		t.Fatalf("found %d declarations, fewer than the %d shipped names", len(have), len(shipped))
	}
}
