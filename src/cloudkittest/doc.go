// Package cloudkittest is the contract kit every cloudkit adapter passes: nine
// contract cases, run against any [cloudkit.Provider] over spec-checked
// recordings, with nothing reaching the network.
//
// # Running the suite
//
// [Run] runs every case as a subtest; [Check] runs them and returns a
// [Report] without a *testing.T, for a loader that must refuse an adapter
// (or a REST descriptor) whose recordings fail. A [Suite] names the
// adapter's [cloudkit.Factory], its [Recordings], and how to reach its writes
// ([Write], [PlanFunc], [Destroy]), its user-data renderer ([CloudInit]) and
// its SSH client ([SSH]).
//
//	spec, err := cloudkittest.LoadSpec(os.DirFS("testdata"), "openapi.json")
//	recs, err := cloudkittest.LoadRecordings(os.DirFS("testdata"), "recordings.json", spec)
//	cloudkittest.Run(t, cloudkittest.Suite{Factory: hetzner.Factory(), Recordings: recs, ...})
//
// # Recordings and replay
//
// [Recorder] is an http.RoundTripper that replays one cassette of
// [Recordings] and refuses any request the cassette does not hold. Each
// interaction answers once and in order, so a cassette records how many times
// a call is made and what state each identical call sees.
//
// [ParseRecordings] is the only way to build Recordings, and it checks every
// response against the provider's OpenAPI document ([Spec]): an undescribed
// path or status fails with [ErrNotInSpec], and a body that breaks its schema
// with [ErrSchemaViolation], so a stale or hand-edited fixture cannot load.
// A write path with no recording yet uses a synthetic response, generated
// from the document's examples by [Spec.Synthesize] and marked Synthetic. A
// recording that carries an Authorization header, a cookie, an API-key header
// or any value [cloudkit.Redact] would change is refused whole with
// [ErrCredentialInRecording].
//
// # The schema subset
//
// The validator implements the part of JSON Schema, as OpenAPI 3.0 spells it,
// that provider responses use. Assertions, all checked:
//
//   - type: "object", "array", "string", "number", "integer" or "boolean",
//     as a string or an array of them ("null" in an array means nullable).
//     "integer" is a number with no fractional part.
//   - nullable: true admits null. Without it, null fails whatever the type.
//   - required: property names an object must have.
//   - properties: a schema per property name.
//   - additionalProperties: true, false, or a schema for every property not
//     in properties. Absent, any extra property is allowed.
//   - items: one schema every array element must match.
//   - enum: the value must equal one of the listed JSON values; numbers
//     compare by value.
//   - $ref: a JSON pointer within the same document ("#/..."), with no
//     sibling keyword but description.
//
// Annotations, accepted and not checked: description, title, example,
// default, format, deprecated, readOnly, writeOnly.
//
// Any other keyword (allOf, oneOf, anyOf, not, pattern, minimum, maxLength,
// and the rest) fails [ParseSpec] with [ErrUnsupportedKeyword], naming the
// keyword and where it sits. It is never ignored: a schema is either checked
// in full or not loaded, and the validator grows when a provider's document
// needs it to.
//
// The package uses the standard library only and, like cloudkit itself, never
// touches the standard library's default HTTP client or transport: the
// policy tests in internal/policy scan it with the rest of the module.
package cloudkittest
