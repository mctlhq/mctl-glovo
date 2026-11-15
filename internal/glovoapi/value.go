package glovoapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Value is a decoded JSON tree: map[string]any, []any, string, json.Number,
// bool, or Null for a JSON null. A nil Value means "absent" (JavaScript
// undefined); Null means "present and null". The distinction keeps the
// upstream behaviour where a missing field is omitted from tool output and a
// null field is written as null.
type Value = any

type nullValue struct{}

// MarshalJSON writes a JSON null.
func (nullValue) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// Null is the decoded form of a JSON null.
var Null Value = nullValue{}

// decodeJSON parses exactly one JSON value. Trailing data is an error, like
// JSON.parse.
func decodeJSON(data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return markNulls(v), nil
}

func markNulls(v any) any {
	switch t := v.(type) {
	case nil:
		return Null
	case map[string]any:
		for k, e := range t {
			t[k] = markNulls(e)
		}
	case []any:
		for i, e := range t {
			t[i] = markNulls(e)
		}
	}
	return v
}

// marshalJSON encodes like JSON.stringify: no HTML escaping, no trailing
// newline.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func isNull(v Value) bool {
	_, ok := v.(nullValue)
	return ok
}

// nullish is JavaScript `v == null`.
func nullish(v Value) bool { return v == nil || isNull(v) }

// get is JavaScript optional chaining: v?.k1?.k2. A missing key or a
// non-object parent yields nil (undefined).
func get(v Value, keys ...string) Value {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		next, present := m[k]
		if !present {
			return nil
		}
		v = next
	}
	return v
}

// coalesce is the JavaScript `a ?? b ?? c` chain: the first non-nullish value,
// or the last value when all are nullish.
func coalesce(vs ...Value) Value {
	for _, v := range vs {
		if !nullish(v) {
			return v
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return vs[len(vs)-1]
}

// or is the JavaScript `a || b || c` chain.
func or(vs ...Value) Value {
	for _, v := range vs {
		if truthy(v) {
			return v
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return vs[len(vs)-1]
}

// firstValue mirrors the upstream helper: the first value that is neither
// nullish nor the empty string, else undefined.
func firstValue(vs ...Value) Value {
	for _, v := range vs {
		if nullish(v) {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		return v
	}
	return nil
}

// clean mirrors cleanObject for one field: nullish and "" become absent.
func clean(v Value) Value {
	if nullish(v) {
		return nil
	}
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	return v
}

// orNull turns an absent or null value into Null, for `x ?? null`.
func orNull(v Value) Value {
	if nullish(v) {
		return Null
	}
	return v
}

func truthy(v Value) bool {
	switch t := v.(type) {
	case nil, nullValue:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f := jsNumber(t)
		return f != 0 && !math.IsNaN(f)
	case float64:
		return t != 0 && !math.IsNaN(t)
	case int:
		return t != 0
	default:
		return true
	}
}

// list returns v as an array, or nil when it is not one. It is only for
// probing shapes (the upstream Array.isArray checks); a field that must be a
// list is read with strictList.
func list(v Value) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// ErrUnexpectedShape is wrapped by every ShapeError.
var ErrUnexpectedShape = errors.New("unexpected Glovo response shape")

// ShapeError reports a response field that must be a list but is present
// with another type. Upstream throws a TypeError in that case; this package
// returns the error instead of treating the field as empty.
type ShapeError struct {
	Field string
	Got   string
}

func (e *ShapeError) Error() string {
	return fmt.Sprintf("%v: %s is %s, not an array", ErrUnexpectedShape, e.Field, e.Got)
}

// Unwrap makes errors.Is(err, ErrUnexpectedShape) work.
func (e *ShapeError) Unwrap() error { return ErrUnexpectedShape }

func jsTypeName(v Value) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case string:
		return "a string"
	case json.Number, float64, int, Number:
		return "a number"
	case bool:
		return "a boolean"
	}
	return fmt.Sprintf("%T", v)
}

// shapePanic carries a ShapeError from deep inside a compactor to the
// exported function that recovers it with catchShape. It never escapes the
// package: every exported entry point that can reach strictList defers
// catchShape.
type shapePanic struct{ err *ShapeError }

// strictList reads a field that must be a list: absent or null is empty, an
// array is returned as is, and anything else is a ShapeError.
func strictList(v Value, field string) []any {
	if nullish(v) {
		return nil
	}
	if s, ok := v.([]any); ok {
		return s
	}
	panic(shapePanic{&ShapeError{Field: field, Got: jsTypeName(v)}})
}

// catchShape turns a strictList failure into *err. Other panics propagate.
func catchShape(err *error) {
	if r := recover(); r != nil {
		sp, ok := r.(shapePanic)
		if !ok {
			panic(r)
		}
		*err = sp.err
	}
}

// guardShape runs fn and returns a strictList failure as an error.
func guardShape(fn func()) (err error) {
	defer catchShape(&err)
	fn()
	return nil
}

// flatItems is one step of Array.prototype.flatMap over `x || []` where x
// must be a list. Upstream would keep a non-array value as a single element;
// this package rejects it like any other wrongly typed list field.
func flatItems(v Value, field string) []any { return strictList(v, field) }

// jsString is JavaScript String(v).
func jsString(v Value) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case nullValue:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return jsNumberString(jsNumber(t))
	case float64:
		return jsNumberString(t)
	case int:
		return strconv.Itoa(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if !nullish(e) {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// jsNumberString formats a float the way Number.prototype.toString does for
// the ranges that occur in API data.
func jsNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		s = strings.Replace(s, "e-0", "e-", 1)
		s = strings.Replace(s, "e+0", "e+", 1)
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

var jsNumericLiteral = regexp.MustCompile(`^[+-]?(\d+\.?\d*([eE][+-]?\d+)?|\.\d+([eE][+-]?\d+)?)$`)

// jsNumber is JavaScript Number(v) for the value kinds in a JSON tree.
func jsNumber(v Value) float64 {
	switch t := v.(type) {
	case nil:
		return math.NaN()
	case nullValue:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		switch s {
		case "Infinity", "+Infinity":
			return math.Inf(1)
		case "-Infinity":
			return math.Inf(-1)
		}
		if !jsNumericLiteral.MatchString(s) {
			return math.NaN()
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case []any:
		switch len(t) {
		case 0:
			return 0
		case 1:
			return jsNumber(jsString(t[0]))
		}
		return math.NaN()
	default:
		return math.NaN()
	}
}

var parseFloatPrefix = regexp.MustCompile(`^[+-]?(Infinity|\d+\.?\d*([eE][+-]?\d+)?|\.\d+([eE][+-]?\d+)?)`)

// jsParseFloat is Number.parseFloat: the longest numeric prefix.
func jsParseFloat(s string) float64 {
	m := parseFloatPrefix.FindString(strings.TrimLeft(s, jsWhitespace))
	if m == "" {
		return math.NaN()
	}
	switch m {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

const jsWhitespace = " \t\n\r\f\v\u00a0\ufeff"

var parseIntPrefix = regexp.MustCompile(`^[+-]?\d+`)

// jsParseInt is Number.parseInt(s, 10); ok is false for NaN.
func jsParseInt(s string) (float64, bool) {
	m := parseIntPrefix.FindString(strings.TrimLeft(s, jsWhitespace))
	if m == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// strictNumberEquals is JavaScript `v === n` for a number n.
func strictNumberEquals(v Value, n float64) bool {
	switch v.(type) {
	case json.Number, float64, int:
		return jsNumber(v) == n
	}
	return false
}

// Number is a JSON number that, like JSON.stringify, encodes NaN and
// infinities as null.
type Number float64

// MarshalJSON implements json.Marshaler.
func (n Number) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return []byte(jsNumberString(f)), nil
}

// jsToFixed2 is Number.prototype.toFixed(2). It rounds the exact binary
// value of f half up, as the specification requires, so 1.125 gives "1.13"
// while 1.005 (stored as 1.00499...) gives "1.00". strconv rounds ties to
// even and would give "1.12".
func jsToFixed2(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.Abs(f) >= 1e21:
		return jsNumberString(f)
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	r := new(big.Rat).SetFloat64(f)
	r.Mul(r, big.NewRat(100, 1))
	r.Add(r, big.NewRat(1, 2))
	n := new(big.Int).Quo(r.Num(), r.Denom()).String()
	for len(n) < 3 {
		n = "0" + n
	}
	return sign + n[:len(n)-2] + "." + n[len(n)-2:]
}

// numberValue wraps a computed float as a pass-through Value.
func numberValue(f float64) Value { return Number(f) }

func boolPtr(b bool) *bool { return &b }

func stringPtr(s string) *string { return &s }

// encodeURIComponent mirrors the JavaScript function of the same name.
func encodeURIComponent(s string) string {
	const unreserved = "-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%")
		b.WriteString(strings.ToUpper(strconv.FormatUint(uint64(c)>>4, 16)))
		b.WriteString(strings.ToUpper(strconv.FormatUint(uint64(c)&0xf, 16)))
	}
	return b.String()
}

// query is an ordered URLSearchParams.
type query [][2]string

func (q *query) set(k, v string) {
	for i := range *q {
		if (*q)[i][0] == k {
			(*q)[i][1] = v
			return
		}
	}
	*q = append(*q, [2]string{k, v})
}

func formEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '*' || c == '-' || c == '.' || c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(strconv.FormatUint(uint64(c)>>4, 16)))
			b.WriteString(strings.ToUpper(strconv.FormatUint(uint64(c)&0xf, 16)))
		}
	}
	return b.String()
}

// String encodes the query like URLSearchParams.toString.
func (q query) String() string {
	parts := make([]string, len(q))
	for i, kv := range q {
		parts[i] = formEncode(kv[0]) + "=" + formEncode(kv[1])
	}
	return strings.Join(parts, "&")
}

// withQuery appends "?query" when the query is non-empty.
func withQuery(path string, q query) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.String()
}
