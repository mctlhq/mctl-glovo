package glovotools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// schema is the subset of JSON Schema (draft-07) that glovo-skill's zod
// input schemas compile to: type, properties, required, items, anyOf, enum,
// minimum, maximum, minLength, maxLength and pattern. Any other keyword in a
// tool definition is a load error, so a schema this validator would only
// half check is never served.
type schema struct {
	Type       string
	Properties []property // in declaration order, as zod reports issues
	Required   []string
	Items      *schema
	AnyOf      []*schema
	Enum       []any
	Minimum    *float64
	Maximum    *float64
	MinLength  *int
	MaxLength  *int
	Pattern    *regexp.Regexp
	PatternSrc string
}

type property struct {
	Name   string
	Schema *schema
}

var knownKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "items": true, "anyOf": true,
	"enum": true, "minimum": true, "maximum": true, "minLength": true, "maxLength": true,
	"pattern": true, "description": true, "$schema": true,
}

func parseSchema(raw json.RawMessage) (*schema, error) {
	keys, err := orderedKeys(raw)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	s := &schema{}
	for _, k := range keys {
		if !knownKeywords[k] {
			return nil, fmt.Errorf("unsupported schema keyword %q", k)
		}
	}
	if v, ok := m["type"]; ok {
		if err := json.Unmarshal(v, &s.Type); err != nil {
			return nil, fmt.Errorf("type: %w", err)
		}
		switch s.Type {
		case "string", "number", "integer", "boolean", "array", "object":
		default:
			return nil, fmt.Errorf("unsupported type %q", s.Type)
		}
	}
	if v, ok := m["properties"]; ok {
		names, err := orderedKeys(v)
		if err != nil {
			return nil, fmt.Errorf("properties: %w", err)
		}
		var props map[string]json.RawMessage
		if err := json.Unmarshal(v, &props); err != nil {
			return nil, fmt.Errorf("properties: %w", err)
		}
		for _, n := range names {
			ps, err := parseSchema(props[n])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", n, err)
			}
			s.Properties = append(s.Properties, property{n, ps})
		}
	}
	if v, ok := m["required"]; ok {
		if err := json.Unmarshal(v, &s.Required); err != nil {
			return nil, fmt.Errorf("required: %w", err)
		}
	}
	if v, ok := m["items"]; ok {
		if s.Items, err = parseSchema(v); err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
	}
	if v, ok := m["anyOf"]; ok {
		var alts []json.RawMessage
		if err := json.Unmarshal(v, &alts); err != nil {
			return nil, fmt.Errorf("anyOf: %w", err)
		}
		for i, a := range alts {
			as, err := parseSchema(a)
			if err != nil {
				return nil, fmt.Errorf("anyOf[%d]: %w", i, err)
			}
			s.AnyOf = append(s.AnyOf, as)
		}
	}
	if v, ok := m["enum"]; ok {
		if err := json.Unmarshal(v, &s.Enum); err != nil {
			return nil, fmt.Errorf("enum: %w", err)
		}
	}
	for _, f := range []struct {
		key string
		dst **float64
	}{{"minimum", &s.Minimum}, {"maximum", &s.Maximum}} {
		if v, ok := m[f.key]; ok {
			var x float64
			if err := json.Unmarshal(v, &x); err != nil {
				return nil, fmt.Errorf("%s: %w", f.key, err)
			}
			*f.dst = &x
		}
	}
	for _, f := range []struct {
		key string
		dst **int
	}{{"minLength", &s.MinLength}, {"maxLength", &s.MaxLength}} {
		if v, ok := m[f.key]; ok {
			var x int
			if err := json.Unmarshal(v, &x); err != nil {
				return nil, fmt.Errorf("%s: %w", f.key, err)
			}
			*f.dst = &x
		}
	}
	if v, ok := m["pattern"]; ok {
		if err := json.Unmarshal(v, &s.PatternSrc); err != nil {
			return nil, fmt.Errorf("pattern: %w", err)
		}
		if s.Pattern, err = regexp.Compile(s.PatternSrc); err != nil {
			return nil, fmt.Errorf("pattern: %w", err)
		}
	}
	return s, nil
}

// orderedKeys returns the keys of a JSON object in document order.
func orderedKeys(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errors.New("want a JSON object")
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, t.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// issue is one validation failure, shaped like a zod v4 issue so the
// isError text reads like glovo-skill's "Input validation error".
type issue []kv

type kv struct {
	K string
	V any
}

func (is issue) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range is {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.K)
		v, err := marshalNoEscape(e.V)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// validate checks args against s and returns the issues, empty when valid.
func (s *schema) validate(v any, path []any) []issue {
	if len(s.AnyOf) > 0 {
		var alts [][]issue
		for _, a := range s.AnyOf {
			is := a.validate(v, path)
			if len(is) == 0 {
				return nil
			}
			alts = append(alts, is)
		}
		errs := make([]any, len(alts))
		for i, a := range alts {
			// zod reports union branches with paths relative to the union.
			rel := make([]any, len(a))
			for j, is := range a {
				rel[j] = relativePath(is, len(path))
			}
			errs[i] = rel
		}
		return []issue{{{"code", "invalid_union"}, {"errors", errs}, {"path", pathOf(path)}, {"message", "Invalid input"}}}
	}
	if len(s.Enum) > 0 {
		for _, e := range s.Enum {
			if e == v {
				return nil
			}
		}
		quoted := make([]string, len(s.Enum))
		for i, e := range s.Enum {
			b, _ := json.Marshal(e)
			quoted[i] = string(b)
		}
		return []issue{{{"code", "invalid_value"}, {"values", s.Enum}, {"path", pathOf(path)}, {"message", "Invalid option: expected one of " + strings.Join(quoted, "|")}}}
	}
	switch s.Type {
	case "string":
		str, ok := v.(string)
		if !ok {
			return []issue{invalidType("string", v, path)}
		}
		var out []issue
		n := utf8.RuneCountInString(str)
		if s.MinLength != nil && n < *s.MinLength {
			out = append(out, issue{{"origin", "string"}, {"code", "too_small"}, {"minimum", *s.MinLength}, {"inclusive", true}, {"path", pathOf(path)},
				{"message", fmt.Sprintf("Too small: expected string to have >=%d characters", *s.MinLength)}})
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			out = append(out, issue{{"origin", "string"}, {"code", "too_big"}, {"maximum", *s.MaxLength}, {"inclusive", true}, {"path", pathOf(path)},
				{"message", fmt.Sprintf("Too big: expected string to have <=%d characters", *s.MaxLength)}})
		}
		if s.Pattern != nil && !s.Pattern.MatchString(str) {
			pat := "/" + s.PatternSrc + "/"
			out = append(out, issue{{"origin", "string"}, {"code", "invalid_format"}, {"format", "regex"}, {"pattern", pat}, {"path", pathOf(path)},
				{"message", "Invalid string: must match pattern " + pat}})
		}
		return out
	case "number", "integer":
		f, ok := v.(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return []issue{invalidType("number", v, path)}
		}
		if s.Type == "integer" && f != math.Trunc(f) {
			return []issue{{{"expected", "int"}, {"format", "safeint"}, {"code", "invalid_type"}, {"path", pathOf(path)}, {"message", "Invalid input: expected int, received number"}}}
		}
		var out []issue
		if s.Minimum != nil && f < *s.Minimum {
			out = append(out, issue{{"origin", "number"}, {"code", "too_small"}, {"minimum", *s.Minimum}, {"inclusive", true}, {"path", pathOf(path)},
				{"message", "Too small: expected number to be >=" + numText(*s.Minimum)}})
		}
		if s.Maximum != nil && f > *s.Maximum {
			out = append(out, issue{{"origin", "number"}, {"code", "too_big"}, {"maximum", *s.Maximum}, {"inclusive", true}, {"path", pathOf(path)},
				{"message", "Too big: expected number to be <=" + numText(*s.Maximum)}})
		}
		return out
	case "boolean":
		if _, ok := v.(bool); !ok {
			return []issue{invalidType("boolean", v, path)}
		}
		return nil
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return []issue{invalidType("array", v, path)}
		}
		var out []issue
		if s.Items != nil {
			for i, e := range arr {
				out = append(out, s.Items.validate(e, appendPath(path, i))...)
			}
		}
		return out
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return []issue{invalidType("object", v, path)}
		}
		required := map[string]bool{}
		for _, r := range s.Required {
			required[r] = true
		}
		var out []issue
		for _, p := range s.Properties {
			pv, present := obj[p.Name]
			if !present {
				if required[p.Name] {
					out = append(out, p.Schema.missing(appendPath(path, p.Name)))
				}
				continue
			}
			out = append(out, p.Schema.validate(pv, appendPath(path, p.Name))...)
		}
		// Unknown keys are dropped, as zod's default object strips them.
		return out
	}
	return nil
}

// missing is the issue for an absent required property.
func (s *schema) missing(path []any) issue {
	if len(s.AnyOf) > 0 {
		return issue{{"code", "invalid_union"}, {"errors", []any{}}, {"path", pathOf(path)}, {"message", "Invalid input"}}
	}
	t := s.Type
	if t == "integer" {
		t = "number"
	}
	return issue{{"expected", t}, {"code", "invalid_type"}, {"path", pathOf(path)}, {"message", "Invalid input: expected " + t + ", received undefined"}}
}

func invalidType(want string, v any, path []any) issue {
	return issue{{"expected", want}, {"code", "invalid_type"}, {"path", pathOf(path)}, {"message", "Invalid input: expected " + want + ", received " + jsTypeName(v)}}
}

func jsTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func relativePath(is issue, depth int) issue {
	out := make(issue, len(is))
	for i, e := range is {
		if e.K == "path" {
			if p, ok := e.V.([]any); ok && len(p) >= depth {
				e.V = append([]any{}, p[depth:]...)
			}
		}
		out[i] = e
	}
	return out
}

func appendPath(path []any, el any) []any {
	out := make([]any, len(path), len(path)+1)
	copy(out, path)
	return append(out, el)
}

func pathOf(path []any) []any {
	if path == nil {
		return []any{}
	}
	return path
}

func numText(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// stringify is JSON.stringify(v, null, 2): two-space indent, no HTML
// escaping, no trailing newline.
func stringify(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}
