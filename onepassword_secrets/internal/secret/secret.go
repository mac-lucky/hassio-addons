// Package secret holds a secret value behind a type that refuses to print
// it. Every formatting path a value could leak through - fmt verbs, slog
// attributes, encoding/json, encoding.TextMarshaler - renders a fixed
// placeholder; only Reveal hands the plain string out, and only the
// renderer that writes the secrets files calls it.
package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
)

// Redacted is what every formatting path prints instead of the value.
const Redacted = "[redacted]"

// Value is one secret. The zero Value is the empty secret.
type Value struct {
	s string
}

// New wraps s.
func New(s string) Value { return Value{s: s} }

// Reveal returns the plain value. Callers: the renderer, and the
// fingerprint below. Nothing else may need it.
func (v Value) Reveal() string { return v.s }

// IsZero reports whether the value is empty.
func (v Value) IsZero() bool { return v.s == "" }

// Equal compares two values in constant time.
func (v Value) Equal(o Value) bool { return hmac.Equal([]byte(v.s), []byte(o.s)) }

// String implements fmt.Stringer.
func (Value) String() string { return Redacted }

// GoString implements fmt.GoStringer, which %#v uses.
func (Value) GoString() string { return Redacted }

// Format implements fmt.Formatter, so no verb (%x, %q, %v) reaches the
// unexported field.
func (Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue implements slog.LogValuer.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON implements json.Marshaler.
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// Fingerprint is an HMAC-SHA256 of the value under key, hex-encoded. A
// plain hash would not do: the hash of a short secret is as good as the
// value (a dictionary run recovers it), and state.json sits in every
// Supervisor backup. key is per install and never leaves /data.
func (v Value) Fingerprint(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(v.s))
	return hex.EncodeToString(mac.Sum(nil))
}
