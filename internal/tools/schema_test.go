package tools

import "testing"

func TestStrictSchemaRejectsUnknownAndAcceptsWholeJSONNumbers(t *testing.T) {
	schema := []byte(`{"type":"object","additionalProperties":false,"properties":{"weekdays":{"type":"array","items":{"type":"integer"}}},"required":["weekdays"]}`)
	if err := validate(schema, []byte(`{"weekdays":[1.0,2e0]}`)); err != nil {
		t.Fatalf("whole JSON numbers rejected: %v", err)
	}
	for _, input := range []string{`{"weekdays":[1.5]}`, `{"weekdays":[1],"unexpected":true}`} {
		if err := validate(schema, []byte(input)); err == nil {
			t.Fatalf("invalid input accepted: %s", input)
		}
	}
}
