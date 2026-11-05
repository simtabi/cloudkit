package cloudkittest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"slices"

	"github.com/simtabi/cloudkit/src"
)

// ErrCredentialInRecording means a recording carries an Authorization header,
// a cookie, an API-key header, or a value [cloudkit.Redact] would change (a
// bearer token, a credential query parameter, URL userinfo). Recordings are
// committed to a public repository, so such a file is refused whole.
var ErrCredentialInRecording = errors.New("cloudkittest: recording carries a credential")

// credentialHeaders are refused in a recording, in a request or a response,
// whatever their value.
var credentialHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
	"X-Auth-Token", "X-Api-Key", "Api-Key",
}

var reCassette = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// Recordings are one provider's recorded responses, as named cassettes, each
// an ordered list of request and response pairs. Build them with
// [ParseRecordings], which checks every response, recorded or synthetic,
// against the provider's [Spec]; there is no other way to make one, so every
// response a [Recorder] replays has been checked.
type Recordings struct {
	provider  string
	cassettes map[string][]interaction
}

type interaction struct {
	method  string
	url     *url.URL
	body    any // decoded request body; nil with hasBody false means none
	hasBody bool
	resp    Response
}

// The file format, in JSON:
//
//	{
//	  "provider": "fake",
//	  "cassettes": {
//	    "read-write": [
//	      {"request": {"method": "GET", "url": "https://api.fake.test/v1/token"},
//	       "response": {"status": 200, "headers": {"Content-Type": ["application/json"]}, "body": {"write": true}}},
//	      {"request": {"method": "DELETE", "url": "https://api.fake.test/v1/resources/s1"},
//	       "synthetic": {"status": 200, "example": "deleted"}}
//	    ]
//	  }
//	}
type fileRecordings struct {
	Provider  string                       `json:"provider"`
	Cassettes map[string][]fileInteraction `json:"cassettes"`
}

type fileInteraction struct {
	Request   fileRequest    `json:"request"`
	Response  *fileResponse  `json:"response,omitempty"`
	Synthetic *fileSynthetic `json:"synthetic,omitempty"`
}

type fileRequest struct {
	Method string          `json:"method"`
	URL    string          `json:"url"`
	Header http.Header     `json:"headers,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type fileResponse struct {
	Status int             `json:"status"`
	Header http.Header     `json:"headers,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

type fileSynthetic struct {
	Status  int    `json:"status"`
	Example string `json:"example,omitempty"`
}

// ParseRecordings reads a recordings file and checks it against spec. It
// refuses unknown fields, a credential anywhere ([ErrCredentialInRecording]),
// an interaction with both or neither of a response and a synthetic entry,
// and any response the spec does not describe ([ErrNotInSpec]) or whose body
// breaks its schema ([ErrSchemaViolation]). A synthetic entry is generated
// with [Spec.Synthesize] and marked Synthetic.
func ParseRecordings(b []byte, spec *Spec) (*Recordings, error) {
	if spec == nil {
		return nil, errors.New("cloudkittest: recordings need the provider's spec; every response is checked against it")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f fileRecordings
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("cloudkittest: recordings: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("cloudkittest: recordings: trailing data after the JSON object")
	}
	if !reCassette.MatchString(f.Provider) {
		return nil, fmt.Errorf("cloudkittest: recordings name no valid provider (%q)", f.Provider)
	}
	if len(f.Cassettes) == 0 {
		return nil, errors.New("cloudkittest: recordings hold no cassettes")
	}
	recs := &Recordings{provider: f.Provider, cassettes: map[string][]interaction{}}
	for name, list := range f.Cassettes {
		if !reCassette.MatchString(name) {
			return nil, fmt.Errorf("cloudkittest: cassette name %q must be lower-case words joined by hyphens", name)
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("cloudkittest: cassette %s is empty", name)
		}
		for i, fi := range list {
			in, err := fi.resolve(spec)
			if err != nil {
				return nil, fmt.Errorf("cloudkittest: cassette %s, interaction %d: %w", name, i, err)
			}
			recs.cassettes[name] = append(recs.cassettes[name], in)
		}
	}
	return recs, nil
}

// LoadRecordings reads and parses a recordings file from fsys.
func LoadRecordings(fsys fs.FS, name string, spec *Spec) (*Recordings, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	r, err := ParseRecordings(b, spec)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return r, nil
}

// Provider returns the provider ID the recordings were made for.
func (r *Recordings) Provider() string { return r.provider }

// Cassettes returns the cassette names, sorted.
func (r *Recordings) Cassettes() []string {
	out := make([]string, 0, len(r.cassettes))
	for k := range r.cassettes {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Len returns the number of interactions in a cassette, 0 if it is absent.
func (r *Recordings) Len(cassette string) int { return len(r.cassettes[cassette]) }

func (fi fileInteraction) resolve(spec *Spec) (interaction, error) {
	req := fi.Request
	if err := noCredentials("request", req.URL, req.Header, req.Body); err != nil {
		return interaction{}, err
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return interaction{}, fmt.Errorf("request method %q is not an upper-case HTTP method", req.Method)
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return interaction{}, fmt.Errorf("request URL %q must be an absolute https URL", req.URL)
	}
	in := interaction{method: req.Method, url: u}
	if len(req.Body) > 0 {
		v, err := decodeJSON(req.Body)
		if err != nil {
			return interaction{}, fmt.Errorf("request body is not JSON: %w", err)
		}
		in.body, in.hasBody = v, true
	}
	switch {
	case fi.Response != nil && fi.Synthetic != nil:
		return interaction{}, errors.New("an interaction has a response or a synthetic entry, not both")
	case fi.Response != nil:
		fr := fi.Response
		if err := noCredentials("response", "", fr.Header, fr.Body); err != nil {
			return interaction{}, err
		}
		h := fr.Header.Clone()
		if h == nil {
			h = http.Header{}
		}
		body := []byte(fr.Body)
		if len(body) > 0 && h.Get("Content-Type") == "" {
			h.Set("Content-Type", "application/json")
		}
		if err := spec.ValidateResponse(req.Method, req.URL, fr.Status, h, body); err != nil {
			return interaction{}, err
		}
		in.resp = Response{Status: fr.Status, Header: h, Body: body}
	case fi.Synthetic != nil:
		resp, err := spec.Synthesize(req.Method, req.URL, fi.Synthetic.Status, fi.Synthetic.Example)
		if err != nil {
			return interaction{}, err
		}
		if err := noCredentials("synthetic response", "", resp.Header, resp.Body); err != nil {
			return interaction{}, err
		}
		in.resp = resp
	default:
		return interaction{}, errors.New("an interaction needs a response or a synthetic entry")
	}
	return in, nil
}

// noCredentials refuses a credential header by name, and any URL, header
// value or body that cloudkit.Redact would change.
func noCredentials(where, rawURL string, h http.Header, body []byte) error {
	for name, vals := range h {
		if slices.Contains(credentialHeaders, http.CanonicalHeaderKey(name)) {
			return fmt.Errorf("%w: the %s has a %s header", ErrCredentialInRecording, where, http.CanonicalHeaderKey(name))
		}
		for _, v := range vals {
			if cloudkit.Redact(v) != v {
				return fmt.Errorf("%w: the %s's %s header", ErrCredentialInRecording, where, http.CanonicalHeaderKey(name))
			}
		}
	}
	if rawURL != "" && cloudkit.Redact(rawURL) != rawURL {
		return fmt.Errorf("%w: the %s URL", ErrCredentialInRecording, where)
	}
	if s := string(body); cloudkit.Redact(s) != s {
		return fmt.Errorf("%w: the %s body", ErrCredentialInRecording, where)
	}
	return nil
}
