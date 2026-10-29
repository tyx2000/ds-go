package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

type paramHeaderDeclaration struct {
	Path       []string
	Name, Type string
}

var headerToken = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")
var inaccessibleHeaderSchemas = []string{"items", "prefixItems", "contains", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "propertyNames", "patternProperties", "dependentSchemas", "oneOf", "anyOf", "allOf", "not", "if", "then", "else", "$defs", "definitions"}

func scanParamHeaders(raw json.RawMessage) ([]paramHeaderDeclaration, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	declarations := []paramHeaderDeclaration{}
	seen := map[string]bool{}
	var visit func(any, []string, bool) error
	visit = func(value any, path []string, reachable bool) error {
		schema, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		if raw, exists := schema["x-mcp-header"]; exists {
			if !reachable || len(path) == 0 {
				return errors.New("x-mcp-header is only permitted on properties statically reachable through properties")
			}
			name, ok := raw.(string)
			if !ok || !headerToken.MatchString(name) {
				return errors.New("x-mcp-header must be a non-empty RFC9110 token")
			}
			typeName, _ := schema["type"].(string)
			switch typeName {
			case "string", "integer", "boolean", "number":
			default:
				return errors.New("x-mcp-header requires a primitive property type")
			}
			lower := strings.ToLower(name)
			if seen[lower] {
				return fmt.Errorf("x-mcp-header %q is not case-insensitively unique", name)
			}
			seen[lower] = true
			declarations = append(declarations, paramHeaderDeclaration{Path: append([]string(nil), path...), Name: name, Type: typeName})
		}
		if properties, ok := schema["properties"].(map[string]any); ok {
			for key, child := range properties {
				next := append(append([]string(nil), path...), key)
				if err := visit(child, next, reachable); err != nil {
					return err
				}
			}
		}
		for _, keyword := range inaccessibleHeaderSchemas {
			child, exists := schema[keyword]
			if !exists {
				continue
			}
			next := append(append([]string(nil), path...), "<"+keyword+">")
			if array, ok := child.([]any); ok {
				for _, entry := range array {
					if err := visit(entry, next, false); err != nil {
						return err
					}
				}
				continue
			}
			switch keyword {
			case "patternProperties", "dependentSchemas", "$defs", "definitions":
				if object, ok := child.(map[string]any); ok {
					for _, entry := range object {
						if err := visit(entry, next, false); err != nil {
							return err
						}
					}
					continue
				}
			}
			if err := visit(child, next, false); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(value, nil, true); err != nil {
		return nil, err
	}
	return declarations, nil
}

func paramPrimitive(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case bool:
		if value {
			return "true", true
		}
		return "false", true
	case json.Number:
		n, err := value.Float64()
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return "", false
		}
		if math.Trunc(n) == n && math.Abs(n) > 9007199254740991 {
			return "", false
		}
		if n == 0 {
			return "0", true
		}
		encoded, err := json.Marshal(n)
		return string(encoded), err == nil
	}
	return "", false
}

type paramHeadersContextKey struct{}

func (c *Client) excludeInvalidHeaderTools(items []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		var tool Tool
		if json.Unmarshal(raw, &tool) != nil {
			result = append(result, raw)
			continue
		}
		if len(tool.InputSchema) == 0 {
			result = append(result, raw)
			continue
		}
		if _, err := scanParamHeaders(tool.InputSchema); err != nil {
			select {
			case c.warnings <- fmt.Sprintf("excluding MCP tool %q: invalid x-mcp-header declaration: %s", tool.Name, err):
			default:
			}
			continue
		}
		result = append(result, raw)
	}
	return result
}

func withParamHeaders(ctx context.Context, declarations []paramHeaderDeclaration, args json.RawMessage) context.Context {
	if len(declarations) == 0 {
		return ctx
	}
	var arguments any
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.UseNumber()
	if decoder.Decode(&arguments) != nil {
		return ctx
	}
	headers := map[string]string{}
	for _, declaration := range declarations {
		value := arguments
		present := true
		for _, part := range declaration.Path {
			object, ok := value.(map[string]any)
			if !ok {
				present = false
				break
			}
			value, present = object[part]
			if !present {
				break
			}
		}
		if !present || value == nil {
			continue
		}
		if text, ok := paramPrimitive(value); ok {
			headers["Mcp-Param-"+declaration.Name] = encodeHeaderValue(text)
		}
	}
	if len(headers) == 0 {
		return ctx
	}
	return context.WithValue(ctx, paramHeadersContextKey{}, headers)
}
