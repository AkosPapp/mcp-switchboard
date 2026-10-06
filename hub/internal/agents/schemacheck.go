package agents

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// A small JSON-schema checker for model-supplied tool arguments: required
// properties, basic types, enum membership and array items. Unknown keywords
// are ignored, so an unusual upstream schema never blocks a call by itself.

// normalizeArgs repairs the harmless mistakes small models make, so a call that
// means the right thing is not bounced: a JSON-string wrapper around the whole
// argument object is unwrapped (unwrapArgs), and values of the wrong but
// obviously equivalent type are converted to the type the schema declares
// (coerceValue). What it cannot repair is left for validateArgs to report.
func normalizeArgs(schema map[string]any, args map[string]any) map[string]any {
	args = unwrapArgs(schema, args)
	if len(schema) == 0 || args == nil {
		return args
	}
	if out, ok := coerceValue(schema, args).(map[string]any); ok {
		return out
	}
	return args
}

// unwrapArgs tolerates the small-model glitch of arguments arriving as a
// JSON string containing an object (possibly double-encoded): it is decoded
// once. Only a lone wrapper key the schema does not declare is unwrapped.
func unwrapArgs(schema map[string]any, args map[string]any) map[string]any {
	if len(args) != 1 {
		return args
	}
	props, _ := schema["properties"].(map[string]any)
	for k, v := range args {
		s, ok := v.(string)
		if !ok || props[k] != nil {
			return args
		}
		if k != "arguments" && k != "args" && k != "input" && k != "_raw_arguments" {
			return args
		}
		var inner map[string]any
		raw := strings.TrimSpace(s)
		if json.Unmarshal([]byte(raw), &inner) == nil && inner != nil {
			return inner
		}
		var str string
		if json.Unmarshal([]byte(raw), &str) == nil && json.Unmarshal([]byte(strings.TrimSpace(str)), &inner) == nil && inner != nil {
			return inner
		}
	}
	return args
}

// validateArgs returns "" when args satisfy schema, else a message naming the
// first problems and the expected shape.
func validateArgs(schema map[string]any, args map[string]any) string {
	if len(schema) == 0 {
		return ""
	}
	var probs []string
	checkValue(schema, args, "arguments", &probs)
	if len(probs) == 0 {
		return ""
	}
	if len(probs) > 5 {
		probs = probs[:5]
	}
	return "invalid arguments: " + strings.Join(probs, "; ") + ". Expected " + schemaHint(schema) + ". Nothing was executed; retry the call with corrected arguments."
}

func checkValue(schema map[string]any, v any, where string, probs *[]string) {
	if t, ok := schema["type"].(string); ok && !typeMatches(t, v) {
		*probs = append(*probs, fmt.Sprintf("%s must be %s, got %s", where, t, jsonTypeOf(v)))
		return
	}
	if enum, ok := schema["enum"].([]any); ok && !inEnum(enum, v) {
		*probs = append(*probs, fmt.Sprintf("%s must be one of %s, got %s", where, enumList(enum), short(v)))
		return
	}
	if enum, ok := schema["enum"].([]string); ok {
		l := make([]any, len(enum))
		for i, e := range enum {
			l[i] = e
		}
		if !inEnum(l, v) {
			*probs = append(*probs, fmt.Sprintf("%s must be one of %s, got %s", where, enumList(l), short(v)))
			return
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for _, r := range requiredOf(schema) {
			if _, ok := x[r]; !ok {
				*probs = append(*probs, fmt.Sprintf("missing required property %q", r))
			}
		}
		props, _ := schema["properties"].(map[string]any)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(map[string]any); ok {
				checkValue(ps, x[k], fmt.Sprintf("property %q", k), probs)
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, e := range x {
				checkValue(items, e, fmt.Sprintf("%s[%d]", where, i), probs)
			}
		}
	}
}

func requiredOf(schema map[string]any) []string {
	switch r := schema["required"].(type) {
	case []string:
		return r
	case []any:
		out := make([]string, 0, len(r))
		for _, e := range r {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func typeMatches(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "null":
		return v == nil
	case "number":
		return isNum(v)
	case "integer":
		f, ok := asFloat(v)
		return ok && f == math.Trunc(f)
	}
	return true // unknown type keyword: do not block
}

func isNum(v any) bool { _, ok := asFloat(v); return ok }

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	if isNum(v) {
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func inEnum(enum []any, v any) bool {
	for _, e := range enum {
		if f1, ok := asFloat(e); ok {
			if f2, ok := asFloat(v); ok && f1 == f2 {
				return true
			}
			continue
		}
		if e == v {
			return true
		}
	}
	return false
}

func enumList(enum []any) string {
	parts := make([]string, len(enum))
	for i, e := range enum {
		b, _ := json.Marshal(e)
		parts[i] = string(b)
	}
	return strings.Join(parts, ", ")
}

func short(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// schemaHint renders a compact description: required fields and each
// property's type, with enum values.
func schemaHint(schema map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return "an object with no properties (pass {})"
	}
	req := map[string]bool{}
	for _, r := range requiredOf(schema) {
		req[r] = true
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		ps, _ := props[k].(map[string]any)
		t, _ := ps["type"].(string)
		if t == "" {
			t = "any"
		}
		d := k + ": " + t
		switch e := ps["enum"].(type) {
		case []any:
			d += " (one of " + enumList(e) + ")"
		case []string:
			l := make([]any, len(e))
			for i, x := range e {
				l[i] = x
			}
			d += " (one of " + enumList(l) + ")"
		}
		if req[k] {
			d += " [required]"
		}
		parts = append(parts, d)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// coerceValue converts v to what schema declares when the conversion is
// unambiguous, recursing through objects and arrays, and returns v unchanged
// otherwise. It never mutates its input; a changed container is a copy.
//
//	boolean  <- "true"/"false" in any case
//	integer  <- "42", "42.0"       number <- "3.5", "1e3"
//	string   <- a number or boolean (its JSON text)
//	array    <- a string holding a JSON array
//	object   <- a string holding a JSON object
//	enum     <- a string that differs from exactly one member only by case
func coerceValue(schema map[string]any, v any) any {
	if t, ok := schema["type"].(string); ok {
		v = coerceScalar(t, v)
	}
	v = coerceEnum(schema, v)

	switch x := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			return x
		}
		var out map[string]any
		for k, e := range x {
			ps, ok := props[k].(map[string]any)
			if !ok {
				continue
			}
			if c := coerceValue(ps, e); !sameJSON(c, e) {
				if out == nil {
					out = make(map[string]any, len(x))
					for kk, ee := range x {
						out[kk] = ee
					}
				}
				out[k] = c
			}
		}
		if out != nil {
			return out
		}
	case []any:
		items, ok := schema["items"].(map[string]any)
		if !ok {
			return x
		}
		var out []any
		for i, e := range x {
			if c := coerceValue(items, e); !sameJSON(c, e) {
				if out == nil {
					out = append([]any(nil), x...)
				}
				out[i] = c
			}
		}
		if out != nil {
			return out
		}
	}
	return v
}

func coerceScalar(t string, v any) any {
	switch t {
	case "boolean":
		if s, ok := v.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true":
				return true
			case "false":
				return false
			}
		}
	case "integer", "number":
		if s, ok := v.(string); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && (t == "number" || f == math.Trunc(f)) {
				return f
			}
		}
	case "string":
		switch x := v.(type) {
		case bool:
			return strconv.FormatBool(x)
		default:
			if f, ok := asFloat(v); ok {
				return strconv.FormatFloat(f, 'f', -1, 64)
			}
		}
	case "array":
		if s, ok := v.(string); ok {
			var a []any
			if json.Unmarshal([]byte(strings.TrimSpace(s)), &a) == nil && a != nil {
				return a
			}
		}
	case "object":
		if s, ok := v.(string); ok {
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(s)), &m) == nil && m != nil {
				return m
			}
		}
	}
	return v
}

func coerceEnum(schema map[string]any, v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	var members []string
	switch e := schema["enum"].(type) {
	case []string:
		members = e
	case []any:
		for _, m := range e {
			if ms, ok := m.(string); ok {
				members = append(members, ms)
			}
		}
	}
	match := ""
	for _, m := range members {
		if m == s {
			return v // already valid
		}
		if strings.EqualFold(m, strings.TrimSpace(s)) {
			if match != "" {
				return v // ambiguous
			}
			match = m
		}
	}
	if match != "" {
		return match
	}
	return v
}

// sameJSON reports whether two decoded JSON values are identical, so a
// container is only copied when a member actually changed.
func sameJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
