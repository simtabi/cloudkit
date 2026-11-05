package cloudkittest_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/simtabi/cloudkit/src/cloudkittest"
)

// specWith is a one-path document whose 200 response has the given schema.
func specWith(schema string) string {
	return `{"openapi":"3.0.3","info":{"title":"t","version":"1"},
	  "servers":[{"url":"https://api.example.test/v2"}],
	  "paths":{"/things/{id}":{"get":{"responses":{"200":{"description":"d",
	    "content":{"application/json":{"schema":` + schema + `}}}}}}},
	  "components":{"schemas":{
	    "Node":{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"children":{"type":"array","items":{"$ref":"#/components/schemas/Node"}}}}}}}`
}

const thingURL = "https://api.example.test/v2/things/7"

func validate(t *testing.T, schema, body string) error {
	t.Helper()
	s, err := cloudkittest.ParseSpec([]byte(specWith(schema)))
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	h := http.Header{"Content-Type": {"application/json"}}
	return s.ValidateResponse(http.MethodGet, thingURL, 200, h, []byte(body))
}

// Each assertion of the subset, accepting a value and refusing one.
func TestSchemaSubset(t *testing.T) {
	cases := []struct {
		name, schema, good, bad string
	}{
		{"string", `{"type":"string"}`, `"x"`, `1`},
		{"integer", `{"type":"integer"}`, `3`, `3.5`},
		{"integer written with an exponent", `{"type":"integer"}`, `1e3`, `true`},
		{"number", `{"type":"number"}`, `3.5`, `"3.5"`},
		{"boolean", `{"type":"boolean"}`, `false`, `"false"`},
		{"array of types", `{"type":["string","integer"]}`, `4`, `[]`},
		{"null without nullable", `{"type":"string"}`, `"x"`, `null`},
		{"nullable", `{"type":"string","nullable":true}`, `null`, `5`},
		{"3.1 null type", `{"type":["string","null"]}`, `null`, `5`},
		{"required", `{"type":"object","required":["id"]}`, `{"id":1}`, `{"name":"a"}`},
		{"properties", `{"type":"object","properties":{"id":{"type":"string"}}}`, `{"id":"a","extra":1}`, `{"id":1}`},
		{"no additional properties", `{"type":"object","properties":{"id":{}},"additionalProperties":false}`, `{"id":1}`, `{"id":1,"x":2}`},
		{"additional properties schema", `{"type":"object","additionalProperties":{"type":"string"}}`, `{"a":"b"}`, `{"a":1}`},
		{"items", `{"type":"array","items":{"type":"integer"}}`, `[1,2]`, `[1,"2"]`},
		{"enum", `{"type":"string","enum":["on","off"]}`, `"on"`, `"maybe"`},
		{"numeric enum by value", `{"enum":[1,2]}`, `1.0`, `3`},
		{"ref", `{"$ref":"#/components/schemas/Node"}`, `{"name":"a"}`, `{"children":[]}`},
		{"recursive ref", `{"$ref":"#/components/schemas/Node"}`, `{"name":"a","children":[{"name":"b"}]}`, `{"name":"a","children":[{"name":2}]}`},
		{"annotations are not checked", `{"type":"string","format":"uuid","description":"d","example":"x"}`, `"not-a-uuid"`, `7`},
	}
	for _, c := range cases {
		if err := validate(t, c.schema, c.good); err != nil {
			t.Errorf("%s: %s refused: %v", c.name, c.good, err)
		}
		err := validate(t, c.schema, c.bad)
		if !errors.Is(err, cloudkittest.ErrSchemaViolation) {
			t.Errorf("%s: %s: got %v, want ErrSchemaViolation", c.name, c.bad, err)
		}
		var se *cloudkittest.SchemaError
		if err != nil && (!errors.As(err, &se) || !strings.HasPrefix(se.Path, "$")) {
			t.Errorf("%s: the violation does not name a JSON path: %v", c.name, err)
		}
	}
}

// An unknown keyword is refused when the document loads, wherever it sits,
// never ignored.
func TestUnknownSchemaKeywordsAreRefused(t *testing.T) {
	for _, schema := range []string{
		`{"type":"string","pattern":"^a"}`,
		`{"type":"integer","minimum":1}`,
		`{"oneOf":[{"type":"string"}]}`,
		`{"type":"object","properties":{"deep":{"type":"array","items":{"type":"string","maxLength":3}}}}`,
		`{"$ref":"#/components/schemas/Node","type":"object"}`,
		`{"$ref":"https://elsewhere.test/schema.json"}`,
	} {
		_, err := cloudkittest.ParseSpec([]byte(specWith(schema)))
		if !errors.Is(err, cloudkittest.ErrUnsupportedKeyword) {
			t.Errorf("%s: got %v, want ErrUnsupportedKeyword", schema, err)
		}
	}
	// A keyword in a schema no response reaches is not compiled, and so not refused.
	doc := strings.Replace(specWith(`{"type":"string"}`), `"Node":`, `"Unused":{"pattern":"x"},"Node":`, 1)
	if _, err := cloudkittest.ParseSpec([]byte(doc)); err != nil {
		t.Errorf("an unreferenced schema was compiled: %v", err)
	}
}

func TestParseSpecRefusesBadDocuments(t *testing.T) {
	for name, doc := range map[string]string{
		"not JSON":          `{`,
		"swagger 2":         `{"swagger":"2.0","paths":{}}`,
		"no servers":        strings.Replace(specWith(`{}`), `"servers":[{"url":"https://api.example.test/v2"}],`, ``, 1),
		"http server":       strings.Replace(specWith(`{}`), `https://api.example.test`, `http://api.example.test`, 1),
		"dangling ref":      specWith(`{"$ref":"#/components/schemas/Missing"}`),
		"non-JSON media":    strings.Replace(specWith(`{}`), `application/json`, `text/plain`, 1),
		"media, no schema":  strings.Replace(specWith(`{}`), `"schema":{}`, `"example":1`, 1),
		"bad status key":    strings.Replace(specWith(`{}`), `"200":`, `"ok":`, 1),
		"trailing data":     specWith(`{}`) + `{}`,
		"unknown type name": specWith(`{"type":"date"}`),
	} {
		if _, err := cloudkittest.ParseSpec([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A request the document does not describe is not a schema pass.
func TestValidateResponseNeedsTheRequestInTheSpec(t *testing.T) {
	s, err := cloudkittest.ParseSpec([]byte(specWith(`{"type":"string"}`)))
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{"Content-Type": {"application/json"}}
	for name, tc := range map[string]struct {
		method, url string
		status      int
	}{
		"other host":     {http.MethodGet, "https://api.other.test/v2/things/7", 200},
		"outside base":   {http.MethodGet, "https://api.example.test/v1/things/7", 200},
		"unknown path":   {http.MethodGet, "https://api.example.test/v2/others/7", 200},
		"missing param":  {http.MethodGet, "https://api.example.test/v2/things/", 200},
		"unknown method": {http.MethodPost, thingURL, 200},
		"unknown status": {http.MethodGet, thingURL, 500},
	} {
		if err := s.ValidateResponse(tc.method, tc.url, tc.status, h, []byte(`"x"`)); !errors.Is(err, cloudkittest.ErrNotInSpec) {
			t.Errorf("%s: got %v, want ErrNotInSpec", name, err)
		}
	}
	if err := s.ValidateResponse(http.MethodGet, thingURL, 200, h, nil); !errors.Is(err, cloudkittest.ErrSchemaViolation) {
		t.Errorf("an empty body where the spec gives one: got %v", err)
	}
	if err := s.ValidateResponse(http.MethodGet, thingURL, 200, http.Header{"Content-Type": {"text/html"}}, []byte(`"x"`)); !errors.Is(err, cloudkittest.ErrNotInSpec) {
		t.Errorf("a content type the spec does not give: got %v", err)
	}
}

// A literal path segment wins over a template, and a range or default
// response answers a status with no exact entry.
func TestOperationAndResponseLookup(t *testing.T) {
	doc := `{"openapi":"3.0.3","servers":[{"url":"https://api.example.test"}],"paths":{
	  "/things/{id}":{"get":{"responses":{"200":{"description":"d","content":{"application/json":{"schema":{"type":"integer"}}}}}}},
	  "/things/search":{"get":{"responses":{"2XX":{"description":"d","content":{"application/json":{"schema":{"type":"string"}}}},
	    "default":{"description":"e","content":{"application/json":{"schema":{"type":"boolean"}}}}}}}}}`
	s, err := cloudkittest.ParseSpec([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	if err := s.ValidateResponse(http.MethodGet, "https://api.example.test/things/search", 202, h, []byte(`"found"`)); err != nil {
		t.Errorf("2XX for the literal path: %v", err)
	}
	if err := s.ValidateResponse(http.MethodGet, "https://api.example.test/things/search", 503, h, []byte(`true`)); err != nil {
		t.Errorf("default: %v", err)
	}
	if err := s.ValidateResponse(http.MethodGet, "https://api.example.test/things/9", 200, h, []byte(`9`)); err != nil {
		t.Errorf("templated path: %v", err)
	}
}

// Synthetic responses come from the spec's examples, are marked, and are
// refused when the example breaks its own schema.
func TestSynthesize(t *testing.T) {
	spec, _ := loadFixtures(t)
	del := "https://api.fake.test/v1/resources/s1"
	r, err := spec.Synthesize(http.MethodDelete, del, 200, "deleted")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Synthetic || r.Status != 200 || !strings.Contains(string(r.Body), `"success"`) || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("deleted: %+v %s", r, r.Body)
	}
	if _, err := spec.Synthesize(http.MethodDelete, del, 200, ""); err == nil {
		t.Error("two named examples and none chosen: accepted")
	}
	if _, err := spec.Synthesize(http.MethodDelete, del, 200, "missing"); err == nil {
		t.Error("an unknown example name: accepted")
	}
	if r, err := spec.Synthesize(http.MethodPost, "https://api.fake.test/v1/resources", 201, ""); err != nil || !r.Synthetic {
		t.Errorf("the single example field: %v", err)
	}
	if _, err := spec.Synthesize(http.MethodGet, "https://api.fake.test/v1/elsewhere", 200, ""); !errors.Is(err, cloudkittest.ErrNotInSpec) {
		t.Errorf("an undescribed path: %v", err)
	}
	bad := strings.Replace(specWith(`{"type":"integer"}`), `"schema":{"type":"integer"}`, `"schema":{"type":"integer"},"example":"seven"`, 1)
	s, err := cloudkittest.ParseSpec([]byte(bad))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Synthesize(http.MethodGet, thingURL, 200, ""); !errors.Is(err, cloudkittest.ErrSchemaViolation) {
		t.Errorf("an example that breaks its schema: got %v, want ErrSchemaViolation", err)
	}
}
