package cloudkit

import (
	"fmt"
	"log/slog"
)

// redacted is what a Secret prints as, everywhere.
const redacted = "[REDACTED]"

// Secret holds a credential. It redacts itself in fmt (every verb), log/slog,
// encoding/json and encoding/text, so a token that reaches a log line or an
// error string by mistake prints as [REDACTED]. The zero value is empty.
type Secret struct{ v []byte }

// NewSecret copies b into a Secret; the caller may clear b afterwards.
func NewSecret(b []byte) Secret {
	return Secret{v: append([]byte(nil), b...)}
}

// Reveal returns a copy of the value, for the one place that must send it.
func (s Secret) Reveal() []byte { return append([]byte(nil), s.v...) }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return len(s.v) == 0 }

// String implements fmt.Stringer.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, so %#v redacts too.
func (s Secret) GoString() string { return redacted }

// Format implements fmt.Formatter, so no verb (%x, %q, %d) reaches the bytes.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
