package threads

import (
	"encoding/json"
	"fmt"
	"reflect"

	gschema "github.com/google/jsonschema-go/jsonschema"
)

type ToolPayload interface{ toolPayload() }

type ToolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Payload     ToolPayload `json:"-"`
}

type ToolOfferSnapshot struct {
	Offered  []ToolSpec `json:"offered,omitempty"`
	Allowed  []string   `json:"allowed"`
	Parallel *bool      `json:"parallel,omitempty"`
	Required bool       `json:"required,omitempty"`
}

type ToolHandlerBinding struct {
	Name            string          `json:"name"`
	HandlerLoadData json.RawMessage `json:"handler_load_data,omitempty"`
}

type (
	toolPayloadText       string
	ToolPayloadJSONSchema gschema.Schema
	ToolPayloadLark       string
	ToolPayloadRegexp     string
)

func (toolPayloadText) toolPayload()       {}
func (ToolPayloadJSONSchema) toolPayload() {}
func (ToolPayloadLark) toolPayload()       {}
func (ToolPayloadRegexp) toolPayload()     {}

func ToolPayloadText() ToolPayload { return toolPayloadText("text") }

// ToolPayloadFor builds a JSON schema payload from a struct type.
//
//	p := ToolPayloadFor[struct {
//		Token string `json:"token" jsonschema:"token to use"`
//	}]()
//
// It panics if T is not a struct or the schema cannot be inferred.
func ToolPayloadFor[T any]() ToolPayload {
	return payloadForType(reflect.TypeFor[T]())
}

func payloadForType(t reflect.Type) ToolPayload {
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("threads.ToolPayloadFor[T] requires struct type, got %s", t))
	}
	schema, err := gschema.ForType(t, nil)
	if err != nil {
		panic(err)
	}
	return ToolPayloadJSONSchema(*schema)
}

func (c ToolCall) UnmarshalJSON(v any) error {
	if c.Payload == "" {
		return nil
	}
	return json.Unmarshal([]byte(c.Payload), v)
}

type toolSpecJSON struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Payload     toolPayloadJSON `json:"payload"`
}

type toolPayloadJSON struct {
	Type       string `json:"type"`
	Definition string `json:"definition,omitempty"`
}

func (s ToolSpec) MarshalJSON() ([]byte, error) {
	p, err := encodePayload(s.Payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(toolSpecJSON{Name: s.Name, Description: s.Description, Payload: p})
}

func (s *ToolSpec) UnmarshalJSON(data []byte) error {
	var raw toolSpecJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p, err := decodePayload(raw.Payload)
	if err != nil {
		return err
	}
	s.Name, s.Description, s.Payload = raw.Name, raw.Description, p
	return nil
}

func encodePayload(p ToolPayload) (toolPayloadJSON, error) {
	switch v := p.(type) {
	case toolPayloadText:
		return toolPayloadJSON{Type: "text"}, nil
	case ToolPayloadJSONSchema:
		buf, err := json.Marshal(gschema.Schema(v))
		if err != nil {
			return toolPayloadJSON{}, fmt.Errorf("marshal json schema payload: %w", err)
		}
		return toolPayloadJSON{Type: "json_schema", Definition: string(buf)}, nil
	case ToolPayloadRegexp:
		return toolPayloadJSON{Type: "regex", Definition: string(v)}, nil
	case ToolPayloadLark:
		return toolPayloadJSON{Type: "lark", Definition: string(v)}, nil
	default:
		return toolPayloadJSON{}, fmt.Errorf("unsupported tool payload: %T", p)
	}
}

func decodePayload(raw toolPayloadJSON) (ToolPayload, error) {
	switch raw.Type {
	case "text":
		return toolPayloadText("text"), nil
	case "json_schema":
		if raw.Definition == "" {
			return nil, fmt.Errorf("json schema payload missing definition")
		}
		var schema gschema.Schema
		if err := json.Unmarshal([]byte(raw.Definition), &schema); err != nil {
			return nil, fmt.Errorf("unmarshal json schema payload: %w", err)
		}
		return ToolPayloadJSONSchema(schema), nil
	case "regex":
		return ToolPayloadRegexp(raw.Definition), nil
	case "lark":
		return ToolPayloadLark(raw.Definition), nil
	default:
		return nil, fmt.Errorf("unsupported tool payload type: %q", raw.Type)
	}
}

func cloneToolOfferSnapshot(in ToolOfferSnapshot) ToolOfferSnapshot {
	buf, err := json.Marshal(in)
	if err != nil {
		panic("threads clone tool offer snapshot marshal failed: " + err.Error())
	}
	var out ToolOfferSnapshot
	if err := json.Unmarshal(buf, &out); err != nil {
		panic("threads clone tool offer snapshot unmarshal failed: " + err.Error())
	}
	return out
}

func cloneToolsSnapshot(in ToolsSnapshot) ToolsSnapshot {
	buf, err := json.Marshal(in)
	if err != nil {
		panic("threads clone tools snapshot marshal failed: " + err.Error())
	}
	var out ToolsSnapshot
	if err := json.Unmarshal(buf, &out); err != nil {
		panic("threads clone tools snapshot unmarshal failed: " + err.Error())
	}
	return out
}
