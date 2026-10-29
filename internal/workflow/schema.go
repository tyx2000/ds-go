package workflow

import (
	"encoding/json"
	"errors"

	"dsgo/internal/jsonschema"
)

// ValidateStructured validates the admitted object schema without converting
// JSON numbers through float64. The workflow request parser limits the schema
// language before execution; this shared validator handles its exact values.
func ValidateStructured(raw json.RawMessage, value any) error {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return err
	}
	if schema["type"] != "object" {
		return errors.New("schema root must have type object")
	}
	instance, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return jsonschema.Validate(raw, instance)
}
