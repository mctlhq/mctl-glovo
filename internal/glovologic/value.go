package glovologic

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
	"time"
	"unicode"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Object is a decoded JSON object. Values are map[string]any, []any, string,
// json.Number, bool, float64 (computed values) or Null. A missing key is
// JavaScript undefined.
type Object = map[string]any

type nullValue struct{}

// MarshalJSON writes a JSON null.
func (nullValue) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// Null is the decoded form of a JSON null.
var Null any = nullValue{}

// decodeJSON parses exactly one JSON value, like JSON.parse.
func decodeJSON(data []byte) (any, error) {
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

func isNull(v any) bool {
	_, ok := v.(nullValue)
	return ok
}

// nullish is JavaScript `v == null`.
func nullish(v any) bool { return v == nil || isNull(v) }

// get is optional chaining v?.k1?.k2: a missing key or a non-object step is
// undefined (nil).
func get(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v, ok = m[k]
		if !ok {
			return nil
		}
	}
	return v
}

// truthy is JavaScript Boolean(v).
func truthy(v any) bool {
	switch t := v.(type) {
	case nil, nullValue:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number, float64, int:
		f := toNumber(t)
		return f != 0 && !math.IsNaN(f)
	default:
		return true
	}
}

// coalesce is a `??` chain: the first non-nullish value, else the last one.
func coalesce(vs ...any) any {
	for _, v := range vs {
		if !nullish(v) {
			return v
		}
	}
	return vs[len(vs)-1]
}

// or is a `||` chain: the first truthy value, else the last one.
func or(vs ...any) any {
	for _, v := range vs {
		if truthy(v) {
			return v
		}
	}
	return vs[len(vs)-1]
}

// toNumber is JavaScript Number(v).
func toNumber(v any) float64 {
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
	case float64:
		return t
	case int:
		return float64(t)
	case json.Number:
		return stringToNumber(string(t))
	case string:
		return stringToNumber(t)
	case []any:
		return stringToNumber(jsString(t))
	default:
		return math.NaN()
	}
}

// jsSpace is the StrWhiteSpaceChar set: Unicode spaces and line terminators
// plus U+FEFF.
func jsSpace(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' }

var decimalLiteral = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// stringToNumber is the StringToNumber abstract operation.
func stringToNumber(s string) float64 {
	s = strings.TrimFunc(s, jsSpace)
	switch s {
	case "":
		return 0
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, ok := new(big.Int).SetString(s[2:], base)
			if !ok {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
	}
	if !decimalLiteral.MatchString(s) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
			return f
		}
		return math.NaN()
	}
	return f
}

var floatPrefix = regexp.MustCompile(`^[+-]?(Infinity|(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?)`)

// parseFloatJS is JavaScript Number.parseFloat(s).
func parseFloatJS(s string) float64 {
	s = strings.TrimLeftFunc(s, jsSpace)
	m := floatPrefix.FindString(s)
	if m == "" {
		return math.NaN()
	}
	return stringToNumber(m)
}

// numberToString is Number::toString for a finite or non-finite double.
func numberToString(f float64) string {
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
	if abs >= 1e-7 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[:1]
	digits := strings.TrimLeft(exp[1:], "0")
	return mant + "e" + sign + digits
}

// jsString is JavaScript String(v).
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case nullValue:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case string:
		return t
	case json.Number, float64, int:
		return numberToString(toNumber(t))
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if !nullish(e) {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// toFixed is Number(x.toFixed(digits)): the exact binary value rounded half
// up on its magnitude, as the ECMAScript algorithm does.
func toFixed(x float64, digits int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) >= 1e21 {
		return x
	}
	neg := x < 0
	r := new(big.Rat).SetFloat64(math.Abs(x))
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	r.Mul(r, new(big.Rat).SetInt(scale))
	n := new(big.Int).Quo(r.Num(), r.Denom())
	rem := new(big.Rat).Sub(r, new(big.Rat).SetInt(n))
	if rem.Cmp(big.NewRat(1, 2)) >= 0 {
		n.Add(n, big.NewInt(1))
	}
	f, err := strconv.ParseFloat(n.String()+"e-"+strconv.Itoa(digits), 64)
	if err != nil {
		return x
	}
	if neg && f != 0 {
		f = -f
	}
	return f
}

// maxTimeMS is the ECMAScript TimeClip bound.
const maxTimeMS = 8.64e15

// isoString is new Date(ms).toISOString(); an out-of-range time is an error,
// as the RangeError upstream.
func isoString(ms float64) (string, error) {
	if math.IsNaN(ms) || math.Abs(ms) > maxTimeMS {
		return "", fmt.Errorf("glovologic: invalid time value %v", ms)
	}
	t := time.UnixMilli(int64(math.Trunc(ms))).UTC()
	year := t.Year()
	rest := t.Format("-01-02T15:04:05.000Z")
	switch {
	case year >= 0 && year <= 9999:
		return fmt.Sprintf("%04d%s", year, rest), nil
	case year < 0:
		return fmt.Sprintf("-%06d%s", -year, rest), nil
	default:
		return fmt.Sprintf("+%06d%s", year, rest), nil
	}
}

var isoDate = regexp.MustCompile(`^([+-]\d{6}|\d{4})(?:-(\d{2})(?:-(\d{2}))?)?(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?(Z|[+-]\d{2}:\d{2})?)?$`)

var legacyLayouts = []string{
	time.RFC1123, time.RFC1123Z, time.RFC822, time.RFC822Z, time.RFC850,
	"Mon Jan 2 2006 15:04:05 GMT-0700", "Jan 2 2006", "January 2 2006", "Jan 2, 2006", "January 2, 2006",
}

// parseDate approximates Date.parse for the formats Glovo sends: ISO 8601
// date and date-time strings (a date-time without an offset is read as UTC,
// not local time) and RFC 1123/822 strings. Anything else is not a date.
func parseDate(s string) (float64, bool) {
	if m := isoDate.FindStringSubmatch(s); m != nil {
		year, _ := strconv.Atoi(strings.TrimPrefix(m[1], "+"))
		month, day := 1, 1
		if m[2] != "" {
			month, _ = strconv.Atoi(m[2])
		}
		if m[3] != "" {
			day, _ = strconv.Atoi(m[3])
		}
		hour, minute, sec, msec := 0, 0, 0, 0
		if m[4] != "" {
			hour, _ = strconv.Atoi(m[4])
			minute, _ = strconv.Atoi(m[5])
		}
		if m[6] != "" {
			sec, _ = strconv.Atoi(m[6])
		}
		if m[7] != "" {
			frac := (m[7] + "00")[:3]
			msec, _ = strconv.Atoi(frac)
		}
		if month < 1 || month > 12 || day < 1 || day > 31 || hour > 24 || minute > 59 || sec > 59 ||
			(hour == 24 && (minute != 0 || sec != 0 || msec != 0)) {
			return 0, false
		}
		loc := time.UTC
		if z := m[8]; z != "" && z != "Z" {
			oh, _ := strconv.Atoi(z[1:3])
			om, _ := strconv.Atoi(z[4:6])
			off := oh*3600 + om*60
			if z[0] == '-' {
				off = -off
			}
			loc = time.FixedZone("", off)
		}
		t := time.Date(year, time.Month(month), day, hour, minute, sec, msec*int(time.Millisecond), loc)
		if t.Day() != day {
			return 0, false
		}
		ms := float64(t.UnixMilli())
		return ms, math.Abs(ms) <= maxTimeMS
	}
	for _, layout := range legacyLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixMilli()), true
		}
	}
	return 0, false
}

// collator returns a fresh collator for String.prototype.localeCompare in the
// "en" locale. Collators are not safe for concurrent use.
func collator() *collate.Collator { return collate.New(language.English) }

// encodeURIComponent is the JavaScript global of the same name.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// marshalJSON encodes like JSON.stringify: no HTML escaping, no newline.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// copyObject is a shallow `{...o}`.
func copyObject(o Object) Object {
	out := make(Object, len(o)+4)
	for k, v := range o {
		out[k] = v
	}
	return out
}

// setOrDelete assigns o[k] = v, where a nil v (undefined) removes the key, as
// JSON.stringify drops an undefined property.
func setOrDelete(o Object, k string, v any) {
	if v == nil {
		delete(o, k)
		return
	}
	o[k] = v
}

// nullable maps a Go nil to Null so a non-omitted output field reads null.
func nullable(v any) any {
	if v == nil {
		return Null
	}
	return v
}

// finiteOrNil returns a pointer to f, or nil (JSON null) for a non-finite f.
func finiteOrNil(f float64) *float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}
