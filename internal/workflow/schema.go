package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
)

// ValidateStructured applies the official object-rooted structured-output subset.
func ValidateStructured(raw json.RawMessage, value any) error {
	var schema map[string]any
	if e := json.Unmarshal(raw, &schema); e != nil {
		return e
	}
	if schema["type"] != "object" {
		return errors.New("schema root must have type object")
	}
	return validateValue(schema, value, "result")
}
func validateValue(s map[string]any, v any, path string) error {
	if choices, ok := s["oneOf"].([]any); ok {
		n := 0
		for _, c := range choices {
			if x, ok := c.(map[string]any); ok && validateValue(x, v, path) == nil {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("%s must match exactly one schema", path)
		}
	}
	if c, ok := s["const"]; ok && !reflect.DeepEqual(c, v) {
		return fmt.Errorf("%s differs from const", path)
	}
	if choices, ok := s["enum"].([]any); ok {
		found := false
		for _, c := range choices {
			if reflect.DeepEqual(c, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s is outside enum", path)
		}
	}
	switch s["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		props, _ := s["properties"].(map[string]any)
		if required, ok := s["required"].([]any); ok {
			for _, r := range required {
				key, _ := r.(string)
				if _, ok := obj[key]; !ok {
					return fmt.Errorf("%s.%s is required", path, key)
				}
			}
		}
		for k, x := range obj {
			if p, ok := props[k].(map[string]any); ok {
				if e := validateValue(p, x, path+"."+k); e != nil {
					return e
				}
			} else if extra, ok := s["additionalProperties"].(bool); ok && !extra {
				return fmt.Errorf("%s.%s is not allowed", path, k)
			} else if p, ok := s["additionalProperties"].(map[string]any); ok {
				if e := validateValue(p, x, path+"."+k); e != nil {
					return e
				}
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, x := range arr {
				if e := validateValue(items, x, fmt.Sprintf("%s[%d]", path, i)); e != nil {
					return e
				}
			}
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	case "number", "integer":
		n, ok := v.(float64)
		if !ok || math.IsInf(n, 0) || math.IsNaN(n) || (s["type"] == "integer" && math.Trunc(n) != n) {
			return fmt.Errorf("%s must be a %s", path, s["type"])
		}
	case "null":
		if v != nil {
			return fmt.Errorf("%s must be null", path)
		}
	}
	return nil
}
