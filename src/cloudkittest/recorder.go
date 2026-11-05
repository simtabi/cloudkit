package cloudkittest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/simtabi/cloudkit/src"
)

// ErrUnexpectedRequest means a [Recorder] received a request its cassette
// does not hold, or holds fewer times than it was sent.
var ErrUnexpectedRequest = errors.New("cloudkittest: unexpected request")

// UnexpectedRequestError names a request a [Recorder] refused. It matches
// [ErrUnexpectedRequest]. The URL has been through [cloudkit.Redact].
type UnexpectedRequestError struct {
	Cassette    string
	Method, URL string
}

func (e *UnexpectedRequestError) Error() string {
	return fmt.Sprintf("cloudkittest: unexpected request in cassette %s: %s %s", e.Cassette, e.Method, e.URL)
}

// Unwrap lets errors.Is match [ErrUnexpectedRequest].
func (e *UnexpectedRequestError) Unwrap() error { return ErrUnexpectedRequest }

// Request is a request a [Recorder] saw, with its URL redacted.
type Request struct {
	Method, URL string
	Unexpected  bool // the cassette did not hold it, so it was refused
}

// Recorder is an http.RoundTripper that replays one cassette. A request is
// answered by the first interaction not yet used whose method, URL (scheme,
// host, path, and query in any order) and JSON body match; each interaction
// answers once, so a cassette records how many times a call is made, and in
// what order identical calls see different state. Any other request gets an
// [*UnexpectedRequestError] and is never sent anywhere. Headers are not
// matched, so the Authorization header cloudkit adds is ignored and never
// stored. It is safe for concurrent use.
//
// Recording new responses from a live provider is the job of a scripted
// recording run, not of this type.
type Recorder struct {
	cassette string
	list     []interaction

	mu   sync.Mutex
	used []bool
	seen []Request
}

// NewRecorder returns a recorder over one cassette of recs.
func NewRecorder(recs *Recordings, cassette string) (*Recorder, error) {
	if recs == nil {
		return nil, errors.New("cloudkittest: NewRecorder: recordings are nil")
	}
	list, ok := recs.cassettes[cassette]
	if !ok {
		return nil, fmt.Errorf("cloudkittest: recordings for %s have no cassette %q", recs.provider, cassette)
	}
	return &Recorder{cassette: cassette, list: list, used: make([]bool, len(list))}, nil
}

// RoundTrip implements http.RoundTripper.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	shown := cloudkit.Redact(req.URL.String())
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, in := range r.list {
		if r.used[i] || !in.matches(req, body) {
			continue
		}
		r.used[i] = true
		r.seen = append(r.seen, Request{Method: req.Method, URL: shown})
		return in.response(req), nil
	}
	r.seen = append(r.seen, Request{Method: req.Method, URL: shown, Unexpected: true})
	return nil, &UnexpectedRequestError{Cassette: r.cassette, Method: req.Method, URL: shown}
}

// Requests returns every request the recorder saw, in order, the refused
// ones included.
func (r *Recorder) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.seen...)
}

// Unexpected returns the requests the cassette did not hold.
func (r *Recorder) Unexpected() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Request
	for _, s := range r.seen {
		if s.Unexpected {
			out = append(out, s)
		}
	}
	return out
}

// Unused returns the cassette's interactions no request has used yet. A
// passing contract case uses every one, so a leftover means the adapter
// skipped a call it was recorded making.
func (r *Recorder) Unused() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Request
	for i, in := range r.list {
		if !r.used[i] {
			out = append(out, Request{Method: in.method, URL: in.url.String()})
		}
	}
	return out
}

// Err returns every unexpected request as one error, or nil.
func (r *Recorder) Err() error {
	var errs []error
	for _, u := range r.Unexpected() {
		errs = append(errs, &UnexpectedRequestError{Cassette: r.cassette, Method: u.Method, URL: u.URL})
	}
	return errors.Join(errs...)
}

func (in interaction) matches(req *http.Request, body []byte) bool {
	if req.Method != in.method || !sameURL(req.URL, in.url) {
		return false
	}
	if len(body) == 0 {
		return !in.hasBody
	}
	if !in.hasBody {
		return false
	}
	v, err := decodeJSON(body)
	return err == nil && equalJSON(v, in.body)
}

func sameURL(a, b *url.URL) bool {
	if a.Scheme != b.Scheme || !strings.EqualFold(a.Host, b.Host) || a.EscapedPath() != b.EscapedPath() {
		return false
	}
	qa, qb := a.Query(), b.Query()
	if len(qa) != len(qb) {
		return false
	}
	for k, va := range qa {
		vb, ok := qb[k]
		if !ok || strings.Join(va, "\x00") != strings.Join(vb, "\x00") {
			return false
		}
	}
	return true
}

func (in interaction) response(req *http.Request) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", in.resp.Status, http.StatusText(in.resp.Status)),
		StatusCode:    in.resp.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        in.resp.Header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(in.resp.Body)),
		ContentLength: int64(len(in.resp.Body)),
		Request:       req,
	}
}
