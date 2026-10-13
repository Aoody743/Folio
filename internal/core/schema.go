package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/mail"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

func schemaArray(value any) []any {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() != reflect.Array && rv.Kind() != reflect.Slice {
		return nil
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out
}
func schemaNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case json.Number:
		v, e := n.Float64()
		return v, e == nil
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}
func validateSchemaDefinition(schema map[string]any, depth int) error {
	if schema == nil || depth > 16 {
		return fmt.Errorf("missing schema or nesting exceeds 16")
	}
	if _, ok := schema["$ref"]; ok {
		return fmt.Errorf("use inline schemas; external references are not accepted")
	}
	if kind, ok := schema["type"].(string); ok {
		switch kind {
		case "object", "array", "string", "integer", "number", "boolean", "null":
		default:
			return fmt.Errorf("unsupported schema type %q", kind)
		}
	} else if schema["type"] != nil {
		types := schemaArray(schema["type"])
		if len(types) == 0 {
			return fmt.Errorf("type must be a valid type name or nonempty array")
		}
		for _, kind := range types {
			if _, ok := kind.(string); !ok {
				return fmt.Errorf("type array entries must be type names")
			}
			if err := validateSchemaDefinition(map[string]any{"type": kind}, depth+1); err != nil {
				return err
			}
		}
	}
	if required, exists := schema["required"]; exists {
		items := schemaArray(required)
		if items == nil {
			return fmt.Errorf("required must be an array of field names")
		}
		seen := map[string]bool{}
		for _, item := range items {
			name, ok := item.(string)
			if !ok || name == "" || seen[name] {
				return fmt.Errorf("required entries must be unique nonempty field names")
			}
			seen[name] = true
		}
	}
	if pattern, ok := schema["pattern"].(string); ok {
		if len(pattern) > 2048 {
			return fmt.Errorf("pattern too large")
		}
		if _, e := regexp.Compile(pattern); e != nil {
			return fmt.Errorf("invalid pattern")
		}
	}
	for _, key := range []string{"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties"} {
		if n, ok := schemaNumber(schema[key]); schema[key] != nil && (!ok || n < 0 || math.Trunc(n) != n) {
			return fmt.Errorf("%s must be a nonnegative integer", key)
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for name, value := range properties {
			child, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("property %s is not a schema", name)
			}
			if e := validateSchemaDefinition(child, depth+1); e != nil {
				return e
			}
		}
	}
	if child, ok := schema["items"].(map[string]any); ok {
		if e := validateSchemaDefinition(child, depth+1); e != nil {
			return e
		}
	}
	if child, ok := schema["additionalProperties"].(map[string]any); ok {
		if e := validateSchemaDefinition(child, depth+1); e != nil {
			return e
		}
	}
	for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
		for _, value := range schemaArray(schema[keyword]) {
			child, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s entries must be schemas", keyword)
			}
			if e := validateSchemaDefinition(child, depth+1); e != nil {
				return e
			}
		}
	}
	return nil
}

func validateSchema(raw json.RawMessage, schema map[string]any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 64<<10 {
		return &Error{Code: "payload_too_large", Message: "Action input exceeds 64 KiB", Status: 413}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if e := d.Decode(&value); e != nil || !json.Valid(raw) {
		return invalid("Action input must be valid JSON")
	}
	if _, ok := value.(map[string]any); !ok {
		return invalid("Action input must be a JSON object")
	}
	if e := validateValue(value, schema, "input", 0); e != nil {
		return invalid(e.Error())
	}
	return nil
}
func validateValue(value any, schema map[string]any, path string, depth int) error {
	if depth > 24 {
		return fmt.Errorf("%s exceeds supported nesting", path)
	}
	if choices := schemaArray(schema["enum"]); len(choices) > 0 {
		match := false
		for _, choice := range choices {
			if canonicalEqual(value, choice) {
				match = true
				break
			}
		}
		if !match {
			return fmt.Errorf("%s is not an allowed enum value", path)
		}
	}
	if choice, ok := schema["const"]; ok && !canonicalEqual(value, choice) {
		return fmt.Errorf("%s does not match its constant", path)
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		options := schemaArray(schema[keyword])
		if len(options) == 0 {
			continue
		}
		matches := 0
		for _, option := range options {
			child, _ := option.(map[string]any)
			if validateValue(value, child, path, depth+1) == nil {
				matches++
			}
		}
		if keyword == "allOf" && matches != len(options) || keyword == "anyOf" && matches == 0 || keyword == "oneOf" && matches != 1 {
			return fmt.Errorf("%s fails %s validation", path, keyword)
		}
	}
	types := schemaArray(schema["type"])
	if kind, ok := schema["type"].(string); ok {
		types = []any{kind}
	}
	if len(types) > 0 {
		match := false
		for _, kind := range types {
			if typeMatches(value, fmt.Sprint(kind)) {
				match = true
				break
			}
		}
		if !match {
			return fmt.Errorf("%s has an invalid type", path)
		}
	}
	switch v := value.(type) {
	case map[string]any:
		if n, ok := schemaNumber(schema["minProperties"]); ok && len(v) < int(n) {
			return fmt.Errorf("%s has too few properties", path)
		}
		if n, ok := schemaNumber(schema["maxProperties"]); ok && len(v) > int(n) {
			return fmt.Errorf("%s has too many properties", path)
		}
		for _, required := range schemaArray(schema["required"]) {
			key, _ := required.(string)
			if _, ok := v[key]; !ok {
				return fmt.Errorf("%s.%s is required", path, key)
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, item := range v {
			child, known := properties[key].(map[string]any)
			if !known {
				if allowed, ok := schema["additionalProperties"].(bool); ok && !allowed {
					return fmt.Errorf("%s.%s is not a known field", path, key)
				}
				if additional, ok := schema["additionalProperties"].(map[string]any); ok {
					child = additional
				} else {
					continue
				}
			}
			if e := validateValue(item, child, path+"."+key, depth+1); e != nil {
				return e
			}
		}
	case []any:
		if n, ok := schemaNumber(schema["minItems"]); ok && len(v) < int(n) {
			return fmt.Errorf("%s has too few items", path)
		}
		if n, ok := schemaNumber(schema["maxItems"]); ok && len(v) > int(n) {
			return fmt.Errorf("%s has too many items", path)
		}
		seen := map[string]bool{}
		for i, item := range v {
			if unique, _ := schema["uniqueItems"].(bool); unique {
				raw, _ := json.Marshal(item)
				key := string(raw)
				if seen[key] {
					return fmt.Errorf("%s requires unique items", path)
				}
				seen[key] = true
			}
			if child, ok := schema["items"].(map[string]any); ok {
				if e := validateValue(item, child, fmt.Sprintf("%s[%d]", path, i), depth+1); e != nil {
					return e
				}
			}
		}
	case string:
		length := utf8.RuneCountInString(v)
		if n, ok := schemaNumber(schema["minLength"]); ok && length < int(n) {
			return fmt.Errorf("%s is too short", path)
		}
		if n, ok := schemaNumber(schema["maxLength"]); ok && length > int(n) {
			return fmt.Errorf("%s is too long", path)
		}
		if pattern, ok := schema["pattern"].(string); ok {
			re, e := regexp.Compile(pattern)
			if e != nil || !re.MatchString(v) {
				return fmt.Errorf("%s does not match its pattern", path)
			}
		}
		if format, ok := schema["format"].(string); ok && !validFormat(v, format) {
			return fmt.Errorf("%s has invalid %s format", path, format)
		}
	case json.Number:
		n, e := v.Float64()
		if e != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return fmt.Errorf("%s is not a finite number", path)
		}
		if minimum, ok := schemaNumber(schema["minimum"]); ok && n < minimum {
			return fmt.Errorf("%s is below its minimum", path)
		}
		if maximum, ok := schemaNumber(schema["maximum"]); ok && n > maximum {
			return fmt.Errorf("%s exceeds its maximum", path)
		}
		if minimum, ok := schemaNumber(schema["exclusiveMinimum"]); ok && n <= minimum {
			return fmt.Errorf("%s is below its exclusive minimum", path)
		}
		if maximum, ok := schemaNumber(schema["exclusiveMaximum"]); ok && n >= maximum {
			return fmt.Errorf("%s exceeds its exclusive maximum", path)
		}
		if multiple, ok := schemaNumber(schema["multipleOf"]); ok && (multiple <= 0 || math.Abs(n/multiple-math.Round(n/multiple)) > 1e-9) {
			return fmt.Errorf("%s is not a supported multiple", path)
		}
	}
	return nil
}
func typeMatches(v any, kind string) bool {
	switch kind {
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
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, e := n.Int64()
		return e == nil
	}
	return false
}
func canonicalEqual(a, b any) bool {
	if n, ok := schemaNumber(a); ok {
		if m, ok := schemaNumber(b); ok {
			return n == m
		}
	}
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return bytes.Equal(ra, rb)
}
func validFormat(value, format string) bool {
	switch format {
	case "date-time":
		_, e := time.Parse(time.RFC3339Nano, value)
		return e == nil
	case "uri", "url":
		u, e := url.Parse(value)
		return e == nil && u.Scheme != ""
	case "email":
		a, e := mail.ParseAddress(value)
		return e == nil && a.Address == value
	case "ipv4":
		ip := net.ParseIP(value)
		return ip != nil && ip.To4() != nil
	case "ipv6":
		ip := net.ParseIP(value)
		return ip != nil && ip.To4() == nil
	case "hostname":
		return len(value) > 0 && len(value) <= 253 && !strings.ContainsAny(value, " /\\\x00")
	default:
		return true
	}
}
