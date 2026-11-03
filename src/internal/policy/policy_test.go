// Package policy holds tests that read the whole repository rather than one
// package: the module's network and global-state rules, and the release
// workflow guard. They are tests, not a linter, so `make ci` cannot skip them.
package policy

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// minGoFiles is a floor on how many Go files the scan reads. A walk that
// stops matching would otherwise report a clean module. Raise it as the
// module grows; never lower it to make a failure go away.
const minGoFiles = 7

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

// Go releases are git only until the owner says otherwise, so release.yml
// must never start on its own: no automatic trigger of any kind, and a
// dispatch that refuses to run without confirm=release-go on a v* tag.
func TestReleaseRunsOnlyOnAConfirmedDispatch(t *testing.T) {
	wf := readWorkflow(t, "release.yml")
	if regexp.MustCompile(`(?m)^[ \t]*(push|schedule|pull_request|pull_request_target|release|workflow_run)\s*:`).MatchString(wf) {
		t.Error("release.yml has an automatic trigger; it must run on workflow_dispatch only")
	}
	for _, want := range []string{"workflow_dispatch:", "confirm:", `"${CONFIRM}" != "release-go"`, "refs/tags/v[0-9]*"} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml lacks %q: a release needs an explicit, confirmed dispatch on a tag", want)
		}
	}
	// A library has no binaries; nothing in the release may build or publish one.
	for _, never := range []string{"goreleaser", "upload-artifact", "docker/", "homebrew", "scoop"} {
		if strings.Contains(strings.ToLower(wf), never) {
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
