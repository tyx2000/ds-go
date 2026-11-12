// Package jsonschema validates fixed tool and MCP schemas without fetching
// remote resources or reading schema files supplied by a remote server.
package jsonschema

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sync"

	validator "github.com/santhosh-tekuri/jsonschema/v6"
)

type Schema struct{ compiled *validator.Schema }
type noExternalResources struct{}

func (noExternalResources) Load(string) (any, error) {
	return nil, errors.New("external JSON Schema references are unavailable; use an embedded $defs reference")
}

var cache = struct {
	sync.Mutex
	entries map[[32]byte]*Schema
}{entries: map[[32]byte]*Schema{}}

func Compile(raw []byte) (*Schema, error) {
	key := sha256.Sum256(raw)
	cache.Lock()
	if schema := cache.entries[key]; schema != nil {
		cache.Unlock()
		return schema, nil
	}
	cache.Unlock()
	value, err := decode(raw)
	if err != nil {
		return nil, err
	}
	c := validator.NewCompiler()
	// MCP 2024-11-05 (and most hand-written tool schemas) default to draft-07.
	// A schema that opts into another draft still does so via $schema.
	c.DefaultDraft(validator.Draft7)
	c.UseLoader(noExternalResources{})
	const location = "https://dsgo.invalid/tool-schema.json"
	if err = c.AddResource(location, value); err != nil {
		return nil, err
	}
	compiled, err := c.Compile(location)
	if err != nil {
		return nil, err
	}
	schema := &Schema{compiled: compiled}
	cache.Lock()
	defer cache.Unlock()
	if existing := cache.entries[key]; existing != nil {
		return existing, nil
	}
	// Bound retained generations when an MCP server changes its schema often.
	if len(cache.entries) >= 256 {
		cache.entries = map[[32]byte]*Schema{}
	}
	cache.entries[key] = schema
	return schema, nil
}
func decode(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("expected one JSON value")
	}
	return value, nil
}
func (s *Schema) ValidateJSON(raw []byte) error {
	value, err := decode(raw)
	if err != nil {
		return err
	}
	return s.compiled.Validate(value)
}
func Validate(schema, instance []byte) error {
	compiled, err := Compile(schema)
	if err != nil {
		return err
	}
	return compiled.ValidateJSON(instance)
}
