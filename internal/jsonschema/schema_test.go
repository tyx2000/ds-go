package jsonschema

import "testing"

func TestJSONSchemaReferencesUnionsAndPreciseNumbers(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"value":{"$ref":"#/$defs/choice"},"integer":{"type":"integer","minimum":9007199254740993}},"required":["value","integer"],"additionalProperties":false,"$defs":{"choice":{"oneOf":[{"type":"null"},{"type":"string","minLength":2,"pattern":"^[a-z]+$"}]}}}`)
	for _, input := range []string{`{"value":null,"integer":9007199254740993}`, `{"value":"okay","integer":9007199254740994.0}`} {
		if err := Validate(schema, []byte(input)); err != nil {
			t.Fatalf("valid %s: %v", input, err)
		}
	}
	for _, input := range []string{`{"value":"x","integer":9007199254740993}`, `{"value":"UP","integer":9007199254740993}`, `{"value":null,"integer":9007199254740992}`, `{"value":null,"integer":9007199254740993.5}`, `{"value":null,"integer":9007199254740993,"extra":1}`} {
		if err := Validate(schema, []byte(input)); err == nil {
			t.Fatalf("invalid accepted: %s", input)
		}
	}
}
func TestSchemaDoesNotReadRemoteOrLocalReferences(t *testing.T) {
	for _, ref := range []string{"https://example.invalid/private", "file:///etc/passwd"} {
		if _, err := Compile([]byte(`{"$ref":"` + ref + `"}`)); err == nil {
			t.Fatalf("external ref accepted: %s", ref)
		}
	}
}
