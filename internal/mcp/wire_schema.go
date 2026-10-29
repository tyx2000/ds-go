package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dsgo/internal/jsonschema"
)

// These contracts cover the bridge's default result path. They are transcribed
// from the fixed SDK's self-contained rev2025/rev2026 buildSchemas.ts files;
// testdata/wire-corpus.json is also checked against those actual SDK schemas by
// testdata/check-sdk-wire.mjs. This is not the entire SDK schema registry.
type wireShape map[string]any

func wsType(kind string) wireShape      { return wireShape{"type": kind} }
func wsRef(name string) wireShape       { return wireShape{"$ref": "#/$defs/" + name} }
func wsArray(items any) wireShape       { return wireShape{"type": "array", "items": items} }
func wsEnum(values ...string) wireShape { return wireShape{"type": "string", "enum": values} }
func wsObject(required []string, properties wireShape) wireShape {
	s := wireShape{"type": "object", "properties": properties}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func wsFields(base wireShape, extra wireShape) wireShape {
	out := wireShape{}
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

var resultWireSchemas [2]struct {
	once    sync.Once
	schemas map[string]*jsonschema.Schema
	err     error
}

func buildResultWireSchemas(modern bool) (map[string]*jsonschema.Schema, error) {
	index := 0
	if modern {
		index = 1
	}
	state := &resultWireSchemas[index]
	state.once.Do(func() {
		text, boolean, object := wsType("string"), wsType("boolean"), wsType("object")
		defs := wireShape{}
		defs["icon"] = wsObject([]string{"src"}, wireShape{"src": text, "mimeType": text, "sizes": wsArray(text), "theme": wsEnum("light", "dark")})
		defs["annotations"] = wsObject(nil, wireShape{"audience": wsArray(wsEnum("user", "assistant")), "priority": wireShape{"type": "number", "minimum": 0, "maximum": 1}, "lastModified": text})
		metadata := wireShape{"name": text, "title": text, "icons": wsArray(wsRef("icon"))}
		defs["implementation"] = wsObject([]string{"name", "version"}, wsFields(metadata, wireShape{"version": text, "websiteUrl": text, "description": text}))
		resourceFields := wsFields(metadata, wireShape{"uri": text, "description": text, "mimeType": text, "size": wsType("number"), "annotations": wsRef("annotations"), "_meta": object})
		defs["resource"] = wsObject([]string{"name", "uri"}, resourceFields)
		defs["template"] = wsObject([]string{"name", "uriTemplate"}, wsFields(metadata, wireShape{"uriTemplate": text, "description": text, "mimeType": text, "annotations": wsRef("annotations"), "_meta": object}))
		contentsFields := wireShape{"uri": text, "mimeType": text, "_meta": object}
		defs["resourceContents"] = wireShape{"anyOf": []any{wsObject([]string{"uri", "text"}, wsFields(contentsFields, wireShape{"text": text})), wsObject([]string{"uri", "blob"}, wsFields(contentsFields, wireShape{"blob": text}))}}
		blockFields := wireShape{"annotations": wsRef("annotations"), "_meta": object}
		block := func(kind string, required []string, fields wireShape) wireShape {
			fields = wsFields(blockFields, fields)
			fields["type"] = wireShape{"const": kind}
			return wsObject(append([]string{"type"}, required...), fields)
		}
		defs["content"] = wireShape{"anyOf": []any{
			block("text", []string{"text"}, wireShape{"text": text}),
			block("image", []string{"data", "mimeType"}, wireShape{"data": text, "mimeType": text}),
			block("audio", []string{"data", "mimeType"}, wireShape{"data": text, "mimeType": text}),
			block("resource_link", []string{"name", "uri"}, resourceFields),
			block("resource", []string{"resource"}, wireShape{"resource": wsRef("resourceContents")}),
		}}
		inputFields := wireShape{"type": wireShape{"const": "object"}}
		outputFields := wireShape{}
		outputRequired := []string{}
		if modern {
			inputFields["$schema"], outputFields["$schema"] = text, text
		} else {
			inputFields["properties"], inputFields["required"] = object, wsArray(text)
			outputFields = wsFields(inputFields, nil)
			outputRequired = []string{"type"}
		}
		toolFields := wsFields(metadata, wireShape{"description": text, "inputSchema": wsObject([]string{"type"}, inputFields), "outputSchema": wsObject(outputRequired, outputFields), "annotations": wsObject(nil, wireShape{"title": text, "readOnlyHint": boolean, "destructiveHint": boolean, "idempotentHint": boolean, "openWorldHint": boolean}), "_meta": object})
		if !modern {
			toolFields["execution"] = wsObject(nil, wireShape{"taskSupport": wsEnum("required", "optional", "forbidden")})
		}
		defs["tool"] = wsObject([]string{"name", "inputSchema"}, toolFields)
		capFields := wireShape{"experimental": wireShape{"type": "object", "additionalProperties": object}, "logging": object, "completions": object, "prompts": wsObject(nil, wireShape{"listChanged": boolean}), "resources": wsObject(nil, wireShape{"listChanged": boolean, "subscribe": boolean}), "tools": wsObject(nil, wireShape{"listChanged": boolean}), "extensions": wireShape{"type": "object", "additionalProperties": object}}
		if !modern {
			capFields["tasks"] = object
		}
		defs["capabilities"] = wsObject(nil, capFields)
		root := func(required []string, fields wireShape) wireShape {
			return wsObject(required, wsFields(wireShape{"_meta": object}, fields))
		}
		callFields := wireShape{"content": wsArray(wsRef("content")), "isError": boolean}
		if !modern {
			callFields["structuredContent"] = object
		}
		methods := map[string]wireShape{
			"tools/call":               root([]string{"content"}, callFields),
			"tools/list":               root([]string{"tools"}, wireShape{"tools": wsArray(wsRef("tool")), "nextCursor": text}),
			"resources/list":           root([]string{"resources"}, wireShape{"resources": wsArray(wsRef("resource")), "nextCursor": text}),
			"resources/templates/list": root([]string{"resourceTemplates"}, wireShape{"resourceTemplates": wsArray(wsRef("template")), "nextCursor": text}),
			"resources/read":           root([]string{"contents"}, wireShape{"contents": wsArray(wsRef("resourceContents"))}),
			"initialize":               root([]string{"protocolVersion", "capabilities", "serverInfo"}, wireShape{"protocolVersion": text, "capabilities": wsRef("capabilities"), "serverInfo": wsRef("implementation"), "instructions": text}),
			"server/discover":          root([]string{"supportedVersions", "capabilities"}, wireShape{"supportedVersions": wsArray(text), "capabilities": wsRef("capabilities"), "instructions": text, "resultType": text}),
		}
		state.schemas = map[string]*jsonschema.Schema{}
		for method, schema := range methods {
			if modern {
				fields := schema["properties"].(wireShape)
				fields["resultType"] = text
				if method == "tools/list" || method == "resources/list" || method == "resources/templates/list" || method == "resources/read" {
					fields["ttlMs"] = wireShape{"type": "integer", "minimum": 0, "maximum": 9007199254740991}
					fields["cacheScope"] = wsEnum("public", "private")
					schema["required"] = append(schema["required"].([]string), "ttlMs", "cacheScope")
				}
			}
			schema["$defs"] = defs
			compiled, err := jsonschema.Compile(mustJSON(schema))
			if err != nil {
				state.err = err
				return
			}
			state.schemas[method] = compiled
		}
	})
	return state.schemas, state.err
}

func validateResultWire(method string, raw json.RawMessage, modern bool) error {
	if !modern && method == "tools/call" {
		raw = normalizeLegacyToolResult(raw)
	}
	schemas, err := buildResultWireSchemas(modern)
	if err != nil {
		return fmt.Errorf("compile MCP wire contract: %w", err)
	}
	schema := schemas[method]
	if schema == nil {
		return nil
	}
	if err := schema.ValidateJSON(raw); err != nil {
		return fmt.Errorf("invalid MCP %s result: %w", method, err)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(raw, &result)
	validateItems := func(key string, validate func(json.RawMessage) error) error {
		var items []json.RawMessage
		_ = json.Unmarshal(result[key], &items)
		for _, item := range items {
			if err := validate(item); err != nil {
				return fmt.Errorf("invalid MCP %s result: %w", method, err)
			}
		}
		return nil
	}
	switch method {
	case "tools/call":
		return validateItems("content", validateWireContent)
	case "resources/read":
		return validateItems("contents", validateWireResourceContents)
	case "resources/list":
		return validateItems("resources", validateWireAnnotated)
	case "resources/templates/list":
		return validateItems("resourceTemplates", validateWireAnnotated)
	case "tools/list":
		// Tool annotations are hint objects, not content/resource annotations.
		if !modern {
			return validateItems("tools", func(raw json.RawMessage) error {
				var tool map[string]json.RawMessage
				_ = json.Unmarshal(raw, &tool)
				for _, key := range []string{"inputSchema", "outputSchema"} {
					var schema map[string]json.RawMessage
					_ = json.Unmarshal(tool[key], &schema)
					if value, ok := schema["properties"]; ok && !finiteJSON(value) {
						return errors.New("schema properties contain a non-finite JSON number")
					}
				}
				return nil
			})
		}
	}
	return nil
}

func normalizeLegacyToolResult(raw json.RawMessage) json.RawMessage {
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return raw
	}
	if _, exists := result["content"]; exists {
		return raw
	}
	for _, foreign := range []string{"task", "inputRequests", "requestState"} {
		if _, exists := result[foreign]; exists {
			return raw
		}
	}
	result["content"] = json.RawMessage(`[]`)
	return mustJSON(result)
}

func validateWireContent(raw json.RawMessage) error {
	if err := validateWireAnnotated(raw); err != nil {
		return err
	}
	var block map[string]json.RawMessage
	_ = json.Unmarshal(raw, &block)
	var kind string
	_ = json.Unmarshal(block["type"], &kind)
	switch kind {
	case "image", "audio":
		var data string
		_ = json.Unmarshal(block["data"], &data)
		if !validAtob(data) {
			return errors.New("MCP binary content contains invalid Base64")
		}
	case "resource":
		return validateWireResourceContents(block["resource"])
	}
	return nil
}

func validateWireResourceContents(raw json.RawMessage) error {
	var contents map[string]json.RawMessage
	_ = json.Unmarshal(raw, &contents)
	if value, ok := contents["text"]; ok && len(value) > 0 && value[0] == '"' {
		return nil
	}
	var blob string
	_ = json.Unmarshal(contents["blob"], &blob)
	if !validAtob(blob) {
		return errors.New("MCP resource contains invalid Base64")
	}
	return nil
}

var wireDateTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T([01][0-9]|2[0-3]):[0-5][0-9](:[0-5][0-9](\.[0-9]+)?)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func validateWireAnnotated(raw json.RawMessage) error {
	var value map[string]json.RawMessage
	_ = json.Unmarshal(raw, &value)
	if size, ok := value["size"]; ok {
		n, err := strconv.ParseFloat(string(size), 64)
		if err != nil || math.IsInf(n, 0) {
			return errors.New("MCP resource size must be finite")
		}
	}
	var annotations map[string]json.RawMessage
	_ = json.Unmarshal(value["annotations"], &annotations)
	if date, ok := annotations["lastModified"]; ok {
		var text string
		_ = json.Unmarshal(date, &text)
		if !wireDateTime.MatchString(text) {
			return errors.New("MCP annotations lastModified must be an ISO datetime")
		}
		if _, err := time.Parse("2006-01-02", text[:10]); err != nil {
			return errors.New("MCP annotations lastModified has an invalid calendar date")
		}
	}
	return nil
}

// HTML atob uses forgiving-base64 decode, rather than the bridge's stricter
// canonical padded encoding used at the later durable-image admission boundary.
func validAtob(value string) bool {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\t', '\n', '\f', '\r', ' ':
			continue
		}
		if r > 127 {
			return false
		}
		b.WriteByte(byte(r))
	}
	text := b.String()
	if len(text)%4 == 0 {
		text = strings.TrimSuffix(text, "=")
		text = strings.TrimSuffix(text, "=")
	}
	_, err := base64.RawStdEncoding.DecodeString(text)
	return err == nil
}

func finiteJSON(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(v any) bool {
		switch x := v.(type) {
		case json.Number:
			n, err := x.Float64()
			return err == nil && !math.IsInf(n, 0)
		case map[string]any:
			for _, child := range x {
				if !visit(child) {
					return false
				}
			}
		case []any:
			for _, child := range x {
				if !visit(child) {
					return false
				}
			}
		}
		return true
	}
	return visit(value)
}
