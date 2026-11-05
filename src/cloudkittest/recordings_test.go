package cloudkittest_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/simtabi/cloudkit/src/cloudkittest"
)

func fakeSpec(t *testing.T) *cloudkittest.Spec {
	t.Helper()
	spec, _ := loadFixtures(t)
	return spec
}

// one is a recordings file with one cassette holding one interaction.
func one(interaction string) []byte {
	return []byte(`{"provider":"fake","cassettes":{"read-write":[` + interaction + `]}}`)
}

const tokenOK = `{"request":{"method":"GET","url":"https://api.fake.test/v1/token"},
  "response":{"status":200,"headers":{"Content-Type":["application/json"]},"body":{"write":true}}}`

func TestTheFixturesLoad(t *testing.T) {
	_, recs := loadFixtures(t)
	if got := strings.Join(recs.Cassettes(), ","); got != "destroy,drift,read-only,read-write" {
		t.Errorf("cassettes %s", got)
	}
	if recs.Provider() != "fake" || recs.Len(cloudkittest.CassetteDestroy) != 4 || recs.Len("absent") != 0 {
		t.Errorf("provider %q, destroy %d", recs.Provider(), recs.Len(cloudkittest.CassetteDestroy))
	}
}

// A fixture whose response breaks the provider's schema does not load.
func TestAFixtureThatBreaksTheSchemaFails(t *testing.T) {
	_, err := cloudkittest.LoadRecordings(os.DirFS("testdata"), "broken-schema.recordings.json", fakeSpec(t))
	if !errors.Is(err, cloudkittest.ErrSchemaViolation) {
		t.Fatalf("got %v, want ErrSchemaViolation", err)
	}
	if !strings.Contains(err.Error(), "$.write") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

// A recording never carries an Authorization header or a token: a request
// or response header by name, or a credential anywhere Redact finds one, and
// the file is refused whole.
func TestRecordingsRefuseCredentials(t *testing.T) {
	tok := rand.Text()
	cases := map[string]string{
		"request Authorization": `{"request":{"method":"GET","url":"https://api.fake.test/v1/token","headers":{"Authorization":["Bearer ` + tok + `"]}},
		  "response":{"status":200,"body":{"write":true}}}`,
		"lower-case authorization": `{"request":{"method":"GET","url":"https://api.fake.test/v1/token","headers":{"authorization":["x"]}},
		  "response":{"status":200,"body":{"write":true}}}`,
		"response Set-Cookie": `{"request":{"method":"GET","url":"https://api.fake.test/v1/token"},
		  "response":{"status":200,"headers":{"Set-Cookie":["s=1"]},"body":{"write":true}}}`,
		"X-Api-Key": `{"request":{"method":"GET","url":"https://api.fake.test/v1/token","headers":{"X-Api-Key":["k"]}},
		  "response":{"status":200,"body":{"write":true}}}`,
		"token in a query string": `{"request":{"method":"GET","url":"https://api.fake.test/v1/token?access_token=` + tok + `"},
		  "response":{"status":200,"body":{"write":true}}}`,
		"bearer token in a body": `{"request":{"method":"GET","url":"https://api.fake.test/v1/token"},
		  "response":{"status":401,"body":{"message":"Bearer ` + tok + ` is not valid"}}}`,
		"token in a request body": `{"request":{"method":"POST","url":"https://api.fake.test/v1/resources","body":{"note":"Authorization: Bearer ` + tok + `"}},
		  "response":{"status":201,"body":{"kind":"server","id":"s2"}}}`,
		"userinfo in the URL": `{"request":{"method":"GET","url":"https://admin:` + tok + `@api.fake.test/v1/token"},
		  "response":{"status":200,"body":{"write":true}}}`,
	}
	spec := fakeSpec(t)
	for name, in := range cases {
		_, err := cloudkittest.ParseRecordings(one(in), spec)
		if !errors.Is(err, cloudkittest.ErrCredentialInRecording) {
			t.Errorf("%s: got %v, want ErrCredentialInRecording", name, err)
		}
		if err != nil && strings.Contains(err.Error(), tok) {
			t.Errorf("%s: the refusal repeats the token", name)
		}
	}
}

func TestParseRecordingsRefusesMalformedFiles(t *testing.T) {
	spec := fakeSpec(t)
	cases := map[string][]byte{
		"no spec":          nil,
		"unknown field":    []byte(`{"provider":"fake","cassettes":{"read-write":[` + tokenOK + `]},"extra":1}`),
		"no provider":      []byte(`{"cassettes":{"read-write":[` + tokenOK + `]}}`),
		"no cassettes":     []byte(`{"provider":"fake","cassettes":{}}`),
		"empty cassette":   []byte(`{"provider":"fake","cassettes":{"read-write":[]}}`),
		"bad cassette":     []byte(`{"provider":"fake","cassettes":{"Read Write":[` + tokenOK + `]}}`),
		"trailing data":    []byte(`{"provider":"fake","cassettes":{"read-write":[` + tokenOK + `]}} {}`),
		"lower-case verb":  one(`{"request":{"method":"get","url":"https://api.fake.test/v1/token"},"response":{"status":200,"body":{"write":true}}}`),
		"plain http":       one(`{"request":{"method":"GET","url":"http://api.fake.test/v1/token"},"response":{"status":200,"body":{"write":true}}}`),
		"both":             one(`{"request":{"method":"GET","url":"https://api.fake.test/v1/token"},"response":{"status":200,"body":{"write":true}},"synthetic":{"status":200}}`),
		"neither":          one(`{"request":{"method":"GET","url":"https://api.fake.test/v1/token"}}`),
		"undescribed path": one(`{"request":{"method":"GET","url":"https://api.fake.test/v1/secrets"},"response":{"status":200,"body":{}}}`),
		"undescribed code": one(`{"request":{"method":"DELETE","url":"https://api.fake.test/v1/resources/s1"},"response":{"status":204}}`),
		"bad synthetic":    one(`{"request":{"method":"DELETE","url":"https://api.fake.test/v1/resources/s1"},"synthetic":{"status":200,"example":"nope"}}`),
	}
	for name, b := range cases {
		s := spec
		if name == "no spec" {
			s, b = nil, one(tokenOK)
		}
		if _, err := cloudkittest.ParseRecordings(b, s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := cloudkittest.ParseRecordings(one(tokenOK), spec); err != nil {
		t.Errorf("a valid file refused: %v", err)
	}
}

// get sends one GET through rt and returns the body it was answered with.
func get(t *testing.T, rt http.RoundTripper, url string) (string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// The recorder fails any request its cassette does not hold, answers each
// interaction once and in order, and reports what was left unused.
func TestRecorderFailsUnexpectedRequests(t *testing.T) {
	_, recs := loadFixtures(t)
	rec, err := cloudkittest.NewRecorder(recs, cloudkittest.CassetteDrift)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, rec, "https://api.fake.test/v1/elsewhere"); !errors.Is(err, cloudkittest.ErrUnexpectedRequest) {
		t.Fatalf("an unrecorded request: got %v, want ErrUnexpectedRequest", err)
	}
	var bodies []string
	for range 2 {
		b, err := get(t, rec, "https://api.fake.test/v1/resources")
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, b)
	}
	if !strings.Contains(bodies[0], `"small"`) || !strings.Contains(bodies[1], `"large"`) {
		t.Errorf("identical requests were not answered in recorded order: %v", bodies)
	}
	if _, err := get(t, rec, "https://api.fake.test/v1/resources"); !errors.Is(err, cloudkittest.ErrUnexpectedRequest) {
		t.Errorf("a third identical request: got %v, want ErrUnexpectedRequest (each interaction answers once)", err)
	}
	if n := len(rec.Unexpected()); n != 2 {
		t.Errorf("%d unexpected requests recorded, want 2", n)
	}
	if !errors.Is(rec.Err(), cloudkittest.ErrUnexpectedRequest) {
		t.Errorf("Err: %v", rec.Err())
	}
	if u := rec.Unused(); len(u) != 1 || !strings.HasSuffix(u[0].URL, "/v1/token") {
		t.Errorf("unused %v, want the token interaction", u)
	}
	if n := len(rec.Requests()); n != 4 {
		t.Errorf("%d requests seen, want 4", n)
	}
	if _, err := cloudkittest.NewRecorder(recs, "absent"); err == nil {
		t.Error("an absent cassette: accepted")
	}
}

// Matching ignores query order and headers (the Authorization cloudkit adds
// is never compared, so never needs storing), and compares JSON bodies by
// value.
func TestRecorderMatching(t *testing.T) {
	spec := fakeSpec(t)
	recs, err := cloudkittest.ParseRecordings([]byte(`{"provider":"fake","cassettes":{"read-write":[
	  {"request":{"method":"GET","url":"https://api.fake.test/v1/resources?a=1&b=2"},"response":{"status":200,"body":[]}},
	  {"request":{"method":"POST","url":"https://api.fake.test/v1/resources","body":{"kind":"server","name":"web"}},
	   "synthetic":{"status":201}}]}}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := cloudkittest.NewRecorder(recs, cloudkittest.CassetteReadWrite)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://API.fake.test/v1/resources?b=2&a=1", nil)
	req.Header.Set("Authorization", "Bearer "+rand.Text())
	resp, err := rec.RoundTrip(req)
	if err != nil {
		t.Fatalf("query in another order: %v", err)
	}
	_ = resp.Body.Close()
	post := func(body string) error {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.fake.test/v1/resources", strings.NewReader(body))
		resp, err := rec.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := post(`{"name":"other","kind":"server"}`); !errors.Is(err, cloudkittest.ErrUnexpectedRequest) {
		t.Errorf("another body matched: %v", err)
	}
	if err := post(`{"name": "web", "kind": "server"}`); err != nil {
		t.Errorf("the same body, re-spaced and reordered: %v", err)
	}
	for _, q := range rec.Requests() {
		if strings.Contains(q.URL, "Bearer") {
			t.Errorf("a seen request carries a credential: %v", q)
		}
	}
}
