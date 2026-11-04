package cloudkittest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"
)

// ErrSchemaViolation means a response body does not match the schema the
// provider's OpenAPI document gives for it.
var ErrSchemaViolation = errors.New("cloudkittest: response does not match the provider's schema")

// ErrUnsupportedKeyword means a schema uses a keyword this validator does not
// implement. It is refused, never ignored, so a schema is either checked in
// full or not loaded at all.
var ErrUnsupportedKeyword = errors.New("cloudkittest: unsupported schema keyword")

// The keywords the validator knows; the package documentation says what each
// one checks. Anything else is refused with ErrUnsupportedKeyword when the
// document loads.
var (
	assertionKeywords  = []string{"type", "nullable", "required", "properties", "additionalProperties", "items", "enum", "$ref"}
	annotationKeywords = []string{"description", "title", "example", "default", "format", "deprecated", "readOnly", "writeOnly"}
	schemaTypes        = []string{"object", "array", "string", "number", "integer", "boolean"}
)

// schema is one compiled schema. A $ref schema has target set and nothing
// else.
type schema struct {
	ref        string
	target     *schema
	types      []string
	nullable   bool
	required   []string
	properties map[string]*schema
	// additional is the schema for properties not in properties; nil with
	// noAdditional false means any.
	additional   *schema
	noAdditional bool
	items        *schema
	enum         []any
	hasEnum      bool
}

// compiler turns decoded schema objects into compiled ones, resolving $ref
// against the whole document and compiling each target once, so a schema
// that refers to itself terminates.
type compiler struct {
	doc  map[string]any
	refs map[string]*schema
}

func (c *compiler) compile(v any, at string) (*schema, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cloudkittest: %s: a schema must be an object", at)
	}
	for k := range m {
		if !slices.Contains(assertionKeywords, k) && !slices.Contains(annotationKeywords, k) {
			return nil, fmt.Errorf("%w %q at %s; extend the validator rather than ignore it", ErrUnsupportedKeyword, k, at)
		}
	}
	if ref, ok := m["$ref"]; ok {
		for k := range m {
			if k != "$ref" && k != "description" {
				return nil, fmt.Errorf("%w %q beside $ref at %s", ErrUnsupportedKeyword, k, at)
			}
		}
		s, ok := ref.(string)
		if !ok {
			return nil, fmt.Errorf("cloudkittest: %s: $ref must be a string", at)
		}
		return c.resolve(s, at)
	}
	s := &schema{}
	if t, ok := m["type"]; ok {
		switch tv := t.(type) {
		case string:
			s.types = []string{tv}
		case []any:
			for _, e := range tv {
				name, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("cloudkittest: %s: type must be a string or strings", at)
				}
				s.types = append(s.types, name)
			}
		default:
			return nil, fmt.Errorf("cloudkittest: %s: type must be a string or strings", at)
		}
		for _, name := range s.types {
			if name == "null" { // OpenAPI 3.1 spelling of nullable
				s.nullable = true
				continue
			}
			if !slices.Contains(schemaTypes, name) {
				return nil, fmt.Errorf("cloudkittest: %s: unknown type %q", at, name)
			}
		}
		s.types = slices.DeleteFunc(s.types, func(n string) bool { return n == "null" })
	}
	if n, ok := m["nullable"]; ok {
		b, ok := n.(bool)
		if !ok {
			return nil, fmt.Errorf("cloudkittest: %s: nullable must be a boolean", at)
		}
		s.nullable = s.nullable || b
	}
	if r, ok := m["required"]; ok {
		list, ok := r.([]any)
		if !ok {
			return nil, fmt.Errorf("cloudkittest: %s: required must be an array of names", at)
		}
		for _, e := range list {
			name, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("cloudkittest: %s: required must be an array of names", at)
			}
			s.required = append(s.required, name)
		}
	}
	if p, ok := m["properties"]; ok {
		pm, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cloudkittest: %s: properties must be an object", at)
		}
		s.properties = make(map[string]*schema, len(pm))
		for name, ps := range pm {
			cs, err := c.compile(ps, at+"/properties/"+name)
			if err != nil {
				return nil, err
			}
			s.properties[name] = cs
		}
	}
	if a, ok := m["additionalProperties"]; ok {
		switch av := a.(type) {
		case bool:
			s.noAdditional = !av
		default:
			cs, err := c.compile(av, at+"/additionalProperties")
			if err != nil {
				return nil, err
			}
			s.additional = cs
		}
	}
	if it, ok := m["items"]; ok {
		cs, err := c.compile(it, at+"/items")
		if err != nil {
			return nil, err
		}
		s.items = cs
	}
	if e, ok := m["enum"]; ok {
		list, ok := e.([]any)
		if !ok || len(list) == 0 {
			return nil, fmt.Errorf("cloudkittest: %s: enum must be a non-empty array", at)
		}
		s.enum, s.hasEnum = list, true
	}
	return s, nil
}

// resolve compiles the schema a local $ref points at, once.
func (c *compiler) resolve(ref, at string) (*schema, error) {
	if s, ok := c.refs[ref]; ok {
		return s, nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("%w: $ref %q at %s: only references within the document (#/...) are supported", ErrUnsupportedKeyword, ref, at)
	}
	v, err := pointer(c.doc, ref)
	if err != nil {
		return nil, fmt.Errorf("cloudkittest: %s: %w", at, err)
	}
	s := &schema{ref: ref}
	c.refs[ref] = s // before compiling, so a cycle finds it
	t, err := c.compile(v, ref)
	if err != nil {
		return nil, err
	}
	s.target = t
	return s, nil
}

// pointer follows a "#/a/b" JSON pointer (RFC 6901) through a decoded document.
func pointer(doc map[string]any, ref string) (any, error) {
	var cur any = doc
	for _, tok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$ref %q does not resolve", ref)
		}
		if cur, ok = m[tok]; !ok {
			return nil, fmt.Errorf("$ref %q does not resolve", ref)
		}
	}
	return cur, nil
}

// validate checks v, decoded with json.Decoder.UseNumber, against s. It
// returns the first mismatch, naming its JSON path.
func (s *schema) validate(v any, at string) error {
	for s.target != nil {
		s = s.target
	}
	if v == nil {
		if s.nullable {
			return nil
		}
		return violation(at, "is null, and the schema is not nullable")
	}
	if len(s.types) > 0 && !slices.ContainsFunc(s.types, func(t string) bool { return hasType(v, t) }) {
		return violation(at, fmt.Sprintf("is %s, want %s", typeOf(v), strings.Join(s.types, " or ")))
	}
	if s.hasEnum && !slices.ContainsFunc(s.enum, func(e any) bool { return equalJSON(e, v) }) {
		return violation(at, fmt.Sprintf("is %s, not one of the schema's enum values", short(v)))
	}
	switch tv := v.(type) {
	case map[string]any:
		for _, name := range s.required {
			if _, ok := tv[name]; !ok {
				return violation(at, fmt.Sprintf("lacks the required property %q", name))
			}
		}
		for _, name := range sortedKeys(tv) {
			ps, listed := s.properties[name]
			switch {
			case listed:
			case s.noAdditional:
				return violation(at, fmt.Sprintf("has the property %q, which the schema does not allow", name))
			case s.additional != nil:
				ps = s.additional
			default:
				continue
			}
			if err := ps.validate(tv[name], at+"."+name); err != nil {
				return err
			}
		}
	case []any:
		if s.items != nil {
			for i, e := range tv {
				if err := s.items.validate(e, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// SchemaError says where a response broke its schema. It matches
// [ErrSchemaViolation].
type SchemaError struct {
	Path   string // a JSON path into the body, "$" for the root
	Reason string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("cloudkittest: schema violation at %s: %s", e.Path, e.Reason)
}

// Unwrap lets errors.Is match [ErrSchemaViolation].
func (e *SchemaError) Unwrap() error { return ErrSchemaViolation }

func violation(at, reason string) error { return &SchemaError{Path: at, Reason: reason} }

func hasType(v any, t string) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, _, err := big.ParseFloat(n.String(), 10, 256, big.ToNearestEven)
		return err == nil && f.IsInt()
	}
	return false
}

func typeOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	}
	return fmt.Sprintf("%T", v)
}

// equalJSON compares two decoded JSON values; numbers compare by value.
func equalJSON(a, b any) bool {
	switch av := a.(type) {
	case json.Number:
		bv, ok := b.(json.Number)
		if !ok {
			return false
		}
		x, _, err1 := big.ParseFloat(av.String(), 10, 256, big.ToNearestEven)
		y, _, err2 := big.ParseFloat(bv.String(), 10, 256, big.ToNearestEven)
		return err1 == nil && err2 == nil && x.Cmp(y) == 0
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, e := range av {
			if f, ok := bv[k]; !ok || !equalJSON(e, f) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !equalJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// short renders a value for a message, bounded.
func short(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return typeOf(v)
	}
	if len(b) > 64 {
		return string(b[:64]) + "…"
	}
	return string(b)
}

// decodeJSON decodes b keeping numbers exact, and refuses trailing data.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var rest any
	if err := dec.Decode(&rest); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}
