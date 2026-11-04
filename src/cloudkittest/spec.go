package cloudkittest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ErrNotInSpec means a request or response the provider's OpenAPI document
// does not describe: an unknown server, path, method or status code.
var ErrNotInSpec = errors.New("cloudkittest: not described by the provider's OpenAPI document")

// Spec is a provider's OpenAPI 3 document in JSON, reduced to what the
// contract kit checks: its servers, its paths and methods, and each
// response's JSON schema and examples. Build one with [ParseSpec].
//
// Every schema reachable from a response is compiled when the document loads,
// in the JSON Schema subset the package documentation lists: type, nullable,
// required, properties, additionalProperties, items, enum and local $ref,
// plus annotations that are not checked. A keyword outside that subset fails
// ParseSpec with [ErrUnsupportedKeyword], so no part of a response is
// silently left unchecked. Request bodies and parameters are not read. Only
// JSON media types are supported, and a response object may not be a $ref.
type Spec struct {
	servers []*url.URL
	paths   []specPath
}

type specPath struct {
	template string
	segments []string
	ops      map[string]*specOperation // by upper-case method
}

type specOperation struct {
	responses map[string]*specResponse // "200", "2XX", "default"
}

type specResponse struct {
	content map[string]*specMedia // empty: the response has no body
}

type specMedia struct {
	schema     *schema
	example    any
	hasExample bool
	examples   map[string]any
}

var methods = []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}

// ParseSpec reads an OpenAPI 3 document in JSON.
func ParseSpec(b []byte) (*Spec, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return nil, fmt.Errorf("cloudkittest: spec is not JSON: %w", err)
	}
	doc, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("cloudkittest: spec is not a JSON object")
	}
	if ver, _ := doc["openapi"].(string); !strings.HasPrefix(ver, "3.") {
		return nil, fmt.Errorf("cloudkittest: spec: openapi %q is not 3.x", ver)
	}
	s := &Spec{}
	servers, _ := doc["servers"].([]any)
	for i, e := range servers {
		m, _ := e.(map[string]any)
		raw, _ := m["url"].(string)
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("cloudkittest: spec: servers[%d] %q must be an absolute https URL", i, raw)
		}
		u.Path = strings.TrimSuffix(u.Path, "/")
		s.servers = append(s.servers, u)
	}
	if len(s.servers) == 0 {
		return nil, errors.New("cloudkittest: spec names no servers")
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		return nil, errors.New("cloudkittest: spec has no paths")
	}
	c := &compiler{doc: doc, refs: map[string]*schema{}}
	for _, tmpl := range sortedKeys(paths) {
		if !strings.HasPrefix(tmpl, "/") {
			return nil, fmt.Errorf("cloudkittest: spec: path %q must start with /", tmpl)
		}
		item, ok := paths[tmpl].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cloudkittest: spec: path %q is not an object", tmpl)
		}
		p := specPath{template: tmpl, segments: split(tmpl), ops: map[string]*specOperation{}}
		for _, m := range methods {
			raw, ok := item[m]
			if !ok {
				continue
			}
			op, err := c.operation(raw, "paths."+tmpl+"."+m)
			if err != nil {
				return nil, err
			}
			p.ops[strings.ToUpper(m)] = op
		}
		s.paths = append(s.paths, p)
	}
	return s, nil
}

// LoadSpec reads and parses an OpenAPI document from fsys.
func LoadSpec(fsys fs.FS, name string) (*Spec, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	s, err := ParseSpec(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return s, nil
}

func (c *compiler) operation(raw any, at string) (*specOperation, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cloudkittest: spec: %s is not an object", at)
	}
	rs, ok := m["responses"].(map[string]any)
	if !ok || len(rs) == 0 {
		return nil, fmt.Errorf("cloudkittest: spec: %s has no responses", at)
	}
	op := &specOperation{responses: map[string]*specResponse{}}
	for _, code := range sortedKeys(rs) {
		if !validStatusKey(code) {
			return nil, fmt.Errorf("cloudkittest: spec: %s: response key %q is not a status code, a range like 2XX, or default", at, code)
		}
		r, err := c.response(rs[code], at+".responses."+code)
		if err != nil {
			return nil, err
		}
		op.responses[strings.ToUpper(code)] = r
	}
	return op, nil
}

func (c *compiler) response(raw any, at string) (*specResponse, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cloudkittest: spec: %s is not an object", at)
	}
	if _, ok := m["$ref"]; ok {
		return nil, fmt.Errorf("%w: $ref on a response object at %s; inline the response", ErrUnsupportedKeyword, at)
	}
	r := &specResponse{content: map[string]*specMedia{}}
	content, _ := m["content"].(map[string]any)
	for _, mt := range sortedKeys(content) {
		if !isJSONMedia(mt) {
			return nil, fmt.Errorf("cloudkittest: spec: %s: media type %q is not JSON; only JSON bodies are checked", at, mt)
		}
		mm, ok := content[mt].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cloudkittest: spec: %s.content.%s is not an object", at, mt)
		}
		raw, ok := mm["schema"]
		if !ok {
			return nil, fmt.Errorf("cloudkittest: spec: %s.content.%s has no schema, so its body cannot be checked", at, mt)
		}
		sc, err := c.compile(raw, at+".content."+mt+".schema")
		if err != nil {
			return nil, err
		}
		media := &specMedia{schema: sc}
		media.example, media.hasExample = mm["example"]
		if ex, ok := mm["examples"].(map[string]any); ok {
			media.examples = map[string]any{}
			for name, e := range ex {
				em, _ := e.(map[string]any)
				val, ok := em["value"]
				if !ok {
					return nil, fmt.Errorf("%w: example %q at %s has no inline value ($ref and externalValue are not supported)", ErrUnsupportedKeyword, name, at)
				}
				media.examples[name] = val
			}
		}
		r.content[mediaKey(mt)] = media
	}
	return r, nil
}

func validStatusKey(k string) bool {
	if k == "default" {
		return true
	}
	if len(k) != 3 || k[0] < '1' || k[0] > '5' {
		return false
	}
	if strings.EqualFold(k[1:], "XX") {
		return true
	}
	_, err := strconv.Atoi(k)
	return err == nil
}

func isJSONMedia(mt string) bool {
	base, _, err := mime.ParseMediaType(mt)
	return err == nil && (base == "application/json" || strings.HasSuffix(base, "+json"))
}

func mediaKey(mt string) string {
	base, _, err := mime.ParseMediaType(mt)
	if err != nil {
		return mt
	}
	return base
}

func split(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// operation finds the operation a request goes to: a server whose scheme,
// host and base path prefix the URL, then the path template that matches the
// rest with the most literal segments.
func (s *Spec) operation(method, rawURL string) (*specOperation, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("cloudkittest: %q is not a URL: %w", rawURL, err)
	}
	var rest string
	found := false
	for _, srv := range s.servers {
		if u.Scheme != srv.Scheme || !strings.EqualFold(u.Host, srv.Host) {
			continue
		}
		p := strings.TrimSuffix(u.Path, "/")
		if p == srv.Path || strings.HasPrefix(p, srv.Path+"/") {
			rest, found = strings.TrimPrefix(p, srv.Path), true
			break
		}
	}
	if !found {
		return nil, "", fmt.Errorf("%w: %s %s matches none of the spec's servers", ErrNotInSpec, method, u.Redacted())
	}
	segs := split(rest)
	best, bestLiterals := -1, -1
	for i, p := range s.paths {
		if lit, ok := matchSegments(p.segments, segs); ok && lit > bestLiterals {
			best, bestLiterals = i, lit
		}
	}
	if best < 0 {
		return nil, "", fmt.Errorf("%w: no path matches %s %s", ErrNotInSpec, method, rest)
	}
	p := s.paths[best]
	op, ok := p.ops[strings.ToUpper(method)]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s has no %s operation", ErrNotInSpec, p.template, strings.ToUpper(method))
	}
	return op, strings.ToUpper(method) + " " + p.template, nil
}

func matchSegments(tmpl, segs []string) (literals int, ok bool) {
	if len(tmpl) != len(segs) {
		return 0, false
	}
	for i, t := range tmpl {
		if strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}") {
			if segs[i] == "" {
				return 0, false
			}
			continue
		}
		if t != segs[i] {
			return 0, false
		}
		literals++
	}
	return literals, true
}

// response picks the response for a status: the exact code, then its range
// (2XX), then default.
func (op *specOperation) response(status int) (*specResponse, bool) {
	code := strconv.Itoa(status)
	for _, k := range []string{code, code[:1] + "XX", "DEFAULT"} {
		if r, ok := op.responses[k]; ok {
			return r, true
		}
	}
	return nil, false
}

// ValidateResponse checks a response against the document: the request's
// server, path and method must be described, the status must have a response,
// and the body must match that response's schema, or be empty when the
// response has no content. A mismatch matches [ErrSchemaViolation]; an
// undescribed request or status matches [ErrNotInSpec].
func (s *Spec) ValidateResponse(method, rawURL string, status int, header http.Header, body []byte) error {
	op, name, err := s.operation(method, rawURL)
	if err != nil {
		return err
	}
	r, ok := op.response(status)
	if !ok {
		return fmt.Errorf("%w: %s has no response for status %d", ErrNotInSpec, name, status)
	}
	if len(r.content) == 0 {
		if len(body) > 0 {
			return fmt.Errorf("%s %d: %w", name, status, violation("$", "the spec gives this response no body, and it has one"))
		}
		return nil
	}
	if len(body) == 0 {
		return fmt.Errorf("%s %d: %w", name, status, violation("$", "the spec gives this response a body, and it has none"))
	}
	ct := header.Get("Content-Type")
	media, ok := r.content[mediaKey(ct)]
	if !ok {
		return fmt.Errorf("%w: %s %d has no content for Content-Type %q", ErrNotInSpec, name, status, ct)
	}
	v, err := decodeJSON(body)
	if err != nil {
		return fmt.Errorf("%s %d: %w", name, status, violation("$", "is not JSON: "+err.Error()))
	}
	if err := media.schema.validate(v, "$"); err != nil {
		return fmt.Errorf("%s %d: %w", name, status, err)
	}
	return nil
}

// Response is a provider response the contract kit replays. Synthetic is true
// when it was generated from the document's examples rather than recorded
// from the provider.
type Response struct {
	Status    int
	Header    http.Header
	Body      []byte
	Synthetic bool
}

// Synthesize builds a response from the document's examples, for a write path
// with no recording yet. example names an entry of the media
// type's examples; empty means its single example (the example field, or the
// only entry of examples). A response with no content needs no example. The
// result is marked Synthetic and has passed [Spec.ValidateResponse], so an
// example that breaks its own schema is refused.
func (s *Spec) Synthesize(method, rawURL string, status int, example string) (Response, error) {
	op, name, err := s.operation(method, rawURL)
	if err != nil {
		return Response{}, err
	}
	r, ok := op.response(status)
	if !ok {
		return Response{}, fmt.Errorf("%w: %s has no response for status %d", ErrNotInSpec, name, status)
	}
	resp := Response{Status: status, Header: http.Header{}, Synthetic: true}
	if len(r.content) == 0 {
		if example != "" {
			return Response{}, fmt.Errorf("cloudkittest: %s %d has no content, so no example %q", name, status, example)
		}
		return resp, nil
	}
	mt := "application/json"
	if _, ok := r.content[mt]; !ok {
		keys := make([]string, 0, len(r.content))
		for k := range r.content {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		mt = keys[0]
	}
	media := r.content[mt]
	var val any
	switch {
	case example != "":
		v, ok := media.examples[example]
		if !ok {
			return Response{}, fmt.Errorf("cloudkittest: %s %d has no example %q", name, status, example)
		}
		val = v
	case media.hasExample:
		val = media.example
	case len(media.examples) == 1:
		for _, v := range media.examples {
			val = v
		}
	default:
		return Response{}, fmt.Errorf("cloudkittest: %s %d: name one of its examples", name, status)
	}
	b, err := json.Marshal(val)
	if err != nil {
		return Response{}, err
	}
	resp.Header.Set("Content-Type", mt)
	resp.Body = b
	if err := s.ValidateResponse(method, rawURL, status, resp.Header, resp.Body); err != nil {
		return Response{}, fmt.Errorf("cloudkittest: the spec's own example breaks its schema: %w", err)
	}
	return resp, nil
}
