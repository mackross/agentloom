package programs

import (
	"encoding/json"
	"fmt"
	"reflect"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

// Signature describes a typed input/output contract.
//
// Input and output field names are controlled by ordinary Go JSON handling. Use
// json tags for field names and jsonschema tags for field descriptions.
type Signature[I, O any] struct {
	Name        string
	Instruction string
}

// InputJSON marshals input using ordinary Go JSON handling.
func (s Signature[I, O]) InputJSON(input I) ([]byte, error) {
	data, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal signature input: %w", err)
	}
	return data, nil
}

// InputSchema returns the JSON Schema for the input type.
func (s Signature[I, O]) InputSchema() (*gschema.Schema, error) {
	schema, err := gschema.ForType(reflect.TypeFor[I](), nil)
	if err != nil {
		return nil, fmt.Errorf("build signature input schema: %w", err)
	}
	return schema, nil
}

// OutputSchema returns the JSON Schema for the output type.
func (s Signature[I, O]) OutputSchema() (*gschema.Schema, error) {
	schema, err := gschema.ForType(reflect.TypeFor[O](), nil)
	if err != nil {
		return nil, fmt.Errorf("build signature output schema: %w", err)
	}
	return schema, nil
}
