package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// jsonFieldError carries the JSON path of a schema-violating value so the
// agent receives an actionable message rather than a bare "invalid args".
type jsonFieldError struct {
	Path string
	Msg  string
}

func (e *jsonFieldError) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Msg) }

// errMissing is the sentinel for a missing required value.
var errMissing = fmt.Errorf("missing required field")

// validateArgs validates args (a JSON object as raw bytes) against schema (a
// JSON Schema subset). It implements the subset that meaningfully catches
// agent-constructed bad args before a round trip: object `required`, `type`
// for primitives/object/array, `enum`, `properties` recursion, and `enum`/type
// on nested fields. Full JSON Schema (allOf/anyOf/$ref/pattern/minLength/etc.)
// is intentionally out of scope — the cached schema is a mitigation, not a
// hard guarantee (see design D6).
//
// An empty/nil schema (no inputSchema cached) accepts anything, because a
// server that advertised no schema cannot be validated client-side; rejecting
// would block every call to such a tool. The presence/absence of a schema is
// itself surfaced by the unknown-tool check separately.
func validateArgs(schema json.RawMessage, args json.RawMessage) error {
	if len(schema) == 0 {
		return nil
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		// A malformed cached schema is a server/config bug, not an agent error;
		// accept rather than block every call. The remote call will surface a
		// real arg error if one exists.
		return nil
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return &jsonFieldError{Path: "$", Msg: fmt.Sprintf("arguments are not valid JSON: %v", err)}
	}
	return validateValue("$", s, v)
}

// validateValue checks v against the schema node sch at the given JSON path.
func validateValue(path string, sch map[string]any, v any) error {
	// `type` may be a single string or an array of allowed types.
	if t, ok := sch["type"]; ok {
		if err := checkType(path, t, v); err != nil {
			return err
		}
	}

	// `enum` restricts v to one of the listed values.
	if enum, ok := sch["enum"].([]any); ok {
		matched := false
		for _, e := range enum {
			if reflect.DeepEqual(normalizeJSON(e), normalizeJSON(v)) {
				matched = true
				break
			}
		}
		if !matched {
			return &jsonFieldError{Path: path, Msg: fmt.Sprintf("value %v is not one of %v", v, enum)}
		}
	}

	// Object: enforce required + recurse into properties.
	obj, isObj := v.(map[string]any)
	if isObj {
		if req, ok := sch["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if name == "" {
					continue
				}
				if _, present := obj[name]; !present {
					return &jsonFieldError{Path: path + "." + name, Msg: "missing required field"}
				}
			}
		}
		if props, ok := sch["properties"].(map[string]any); ok {
			// Stable iteration order for deterministic error messages.
			names := make([]string, 0, len(props))
			for n := range props {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, name := range names {
				field, present := obj[name]
				if !present {
					continue
				}
				sub, ok := props[name].(map[string]any)
				if !ok {
					continue
				}
				if err := validateValue(path+"."+name, sub, field); err != nil {
					return err
				}
			}
		}
	}

	// Array: recurse into items for each element.
	if arr, ok := v.([]any); ok {
		if items, ok := sch["items"].(map[string]any); ok {
			for i, el := range arr {
				if err := validateValue(fmt.Sprintf("%s[%d]", path, i), items, el); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// jsonTypeName maps a parsed JSON value to the JSON Schema type name.
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		// json.Unmarshal decodes all numbers as float64.
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return reflect.TypeOf(v).Kind().String()
}

// checkType validates v against the schema's `type` node (string or array).
func checkType(path string, t any, v any) error {
	allowed := typeNames(t)
	if len(allowed) == 0 {
		return nil
	}
	// JSON Schema treats an absent (null) value as the "null" type; many
	// schemas rely on a type list that omits null, so a JSON null that the
	// agent emitted is reported as a type mismatch only when null is not among
	// the allowed types.
	got := jsonTypeName(v)
	for _, want := range allowed {
		if want == got {
			return nil
		}
		// JSON Schema "integer" is a subtype of "number".
		if want == "number" && got == "number" {
			return nil
		}
		if want == "integer" && got == "number" && isInteger(v) {
			return nil
		}
	}
	return &jsonFieldError{Path: path, Msg: fmt.Sprintf("expected type %s, got %s", strings.Join(allowed, "|"), got)}
}

// typeNames normalizes a schema `type` node into a list of allowed type names.
func typeNames(t any) []string {
	switch x := t.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// isInteger reports whether a float64 (from json) has no fractional part.
func isInteger(v any) bool {
	f, ok := v.(float64)
	if !ok {
		return false
	}
	return f == float64(int64(f))
}

// normalizeJSON returns v with numbers canonicalized so reflect.DeepEqual
// compares enum members and values consistently (json decodes 1 as float64
// even when the schema enum listed an int).
func normalizeJSON(v any) any {
	switch x := v.(type) {
	case float64:
		if isInteger(x) {
			return int64(x)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeJSON(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizeJSON(val)
		}
		return out
	}
	return v
}
