package tool

import (
	"encoding/json"
	"fmt"

	"github.com/mackross/agentloom/threads"
)

func ResultText(call Call, text string) Item {
	return threads.ToolCallResult{CallID: call.CallID, Output: text}
}

func ResultJSON(call Call, v any) Item {
	buf, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("tool.ResultJSON(%q): %v", call.Name, err))
	}
	return threads.ToolCallResult{
		CallID: call.CallID,
		Output: string(buf),
		Data:   map[string]any{"json": v},
	}
}

func ResultError(call Call, err error) Item {
	if err == nil {
		panic("tool.ResultError requires non-nil error")
	}
	return threads.ToolCallResult{
		CallID: call.CallID,
		Output: err.Error(),
		Data:   map[string]any{"error": err.Error()},
	}
}
