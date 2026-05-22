package voicethread

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mackross/agentloom/threads"
)

type functionCallBuffer struct {
	ItemID     string
	CallID     string
	Name       string
	Arguments  string
	Done       bool
	Dispatched bool
}

func (s *VoiceSession) handleRealtimeEvent(raw []byte) {
	var ev map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ev); err != nil {
		s.emit(Event{Type: "error", Message: "decode realtime event: " + err.Error()})
		return
	}
	typeName := jsonString(ev, "type")
	switch typeName {
	case "session.created", "session.updated":
		s.emit(Event{Type: typeName, Raw: cloneRaw(raw)})
	case "input_audio_buffer.speech_started":
		s.emit(Event{Type: "user.speech.started", Raw: cloneRaw(raw)})
	case "input_audio_buffer.speech_stopped":
		s.emit(Event{Type: "user.speech.stopped", Raw: cloneRaw(raw)})
	case "input_audio_buffer.committed":
		s.emit(Event{Type: "user.audio.committed", Raw: cloneRaw(raw)})
	case "conversation.item.input_audio_transcription.completed":
		s.emit(Event{Type: "user.transcript", Text: jsonString(ev, "transcript")})
	case "response.created":
		responseID, kind := s.rememberResponseKind(ev)
		if kind == "summary" {
			s.emit(Event{Type: "summary.started", ResponseID: responseID, Raw: cloneRaw(raw)})
		}
		s.emit(Event{Type: "response.created", ResponseID: responseID, Raw: cloneRaw(raw)})
	case "response.output_text.delta":
		responseID := jsonString(ev, "response_id")
		if s.responseKind(responseID) == "summary" {
			s.emit(Event{Type: "summary.text.delta", Text: jsonString(ev, "delta"), ResponseID: responseID})
		} else {
			s.emit(Event{Type: "assistant.text.delta", Text: jsonString(ev, "delta"), ResponseID: responseID})
		}
	case "response.output_audio_transcript.delta":
		s.emit(Event{Type: "assistant.text.delta", Text: jsonString(ev, "delta")})
	case "response.output_audio.delta":
		data := firstString(ev, "delta", "audio", "data")
		if data != "" {
			s.emit(Event{
				Type:         "assistant.audio.delta",
				Data:         data,
				ItemID:       jsonString(ev, "item_id"),
				ContentIndex: jsonIntPtr(ev, "content_index"),
			})
		}
	case "response.function_call_arguments.delta":
		s.mergeFunctionCallDelta(ev)
	case "response.function_call_arguments.done":
		s.mergeFunctionCallDone(ev)
	case "response.output_item.done":
		s.mergeOutputItemDone(ev)
	case "conversation.item.done":
		s.rememberConversationItem(ev)
		s.emit(Event{Type: "debug", Message: typeName, Raw: cloneRaw(raw)})
	case "response.done":
		responseID := responseIDFromDone(ev)
		if s.responseKind(responseID) == "summary" {
			s.markSummaryComplete()
			s.emit(Event{Type: "summary.done", ResponseID: responseID, Raw: cloneRaw(raw)})
		}
		if responseCancelled(ev) {
			s.emit(Event{Type: "assistant.cancelled", ResponseID: responseID, Raw: cloneRaw(raw)})
		}
		s.emit(Event{Type: "response.done", ResponseID: responseID, Raw: cloneRaw(raw)})
	case "error":
		if s.suppressResponseCancelNotActive(ev) {
			s.emit(Event{Type: "debug", Message: realtimeErrorMessage(ev), Raw: cloneRaw(raw)})
			return
		}
		s.emit(Event{Type: "error", Message: realtimeErrorMessage(ev), Raw: cloneRaw(raw)})
	default:
		// Keep unmapped events visible enough for the spike without flooding the UI
		// with every audio-buffer lifecycle event.
		s.emit(Event{Type: "debug", Message: typeName, Raw: cloneRaw(raw)})
	}
}

func (s *VoiceSession) suppressResponseCancelNotActive(ev map[string]json.RawMessage) bool {
	errObj, ok := jsonObject(ev, "error")
	if !ok {
		return false
	}
	if jsonString(errObj, "code") != "response_cancel_not_active" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suppressCancelErrors <= 0 {
		return false
	}
	s.suppressCancelErrors--
	return true
}

func (s *VoiceSession) rememberResponseKind(ev map[string]json.RawMessage) (string, string) {
	response, ok := jsonObject(ev, "response")
	if !ok {
		return "", ""
	}
	responseID := jsonString(response, "id")
	metadata, ok := jsonObject(response, "metadata")
	if !ok {
		return responseID, ""
	}
	kind := firstString(metadata, "agentloom_kind", "kind", "topic")
	if responseID != "" && kind != "" {
		s.mu.Lock()
		s.responseKinds[responseID] = kind
		s.mu.Unlock()
	}
	return responseID, kind
}

func (s *VoiceSession) responseKind(responseID string) string {
	if responseID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.responseKinds[responseID]
}

func (s *VoiceSession) rememberConversationItem(ev map[string]json.RawMessage) {
	item, ok := jsonObject(ev, "item")
	if !ok {
		return
	}
	id := jsonString(item, "id")
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conversationItemIDs) > 0 && s.conversationItemIDs[len(s.conversationItemIDs)-1] == id {
		return
	}
	s.conversationItemIDs = append(s.conversationItemIDs, id)
}

func (s *VoiceSession) markSummaryComplete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSummaryItemIndex = len(s.conversationItemIDs)
}

func (s *VoiceSession) mergeFunctionCallDelta(ev map[string]json.RawMessage) {
	key := functionCallKey(ev)
	delta := jsonString(ev, "delta")
	if key == "" || delta == "" {
		return
	}
	s.mu.Lock()
	buf := s.bufLocked(key)
	mergeFunctionCallFields(buf, ev)
	buf.Arguments += delta
	s.mu.Unlock()
}

func (s *VoiceSession) mergeFunctionCallDone(ev map[string]json.RawMessage) {
	key := functionCallKey(ev)
	if key == "" {
		return
	}
	var ready *functionCallBuffer
	s.mu.Lock()
	buf := s.bufLocked(key)
	mergeFunctionCallFields(buf, ev)
	if args := jsonString(ev, "arguments"); args != "" {
		buf.Arguments = args
	}
	buf.Done = true
	if bufReady(buf) && !buf.Dispatched {
		buf.Dispatched = true
		ready = cloneBuf(buf)
	}
	s.mu.Unlock()
	if ready != nil {
		s.dispatchToolCall(ready)
	}
}

func (s *VoiceSession) mergeOutputItemDone(ev map[string]json.RawMessage) {
	item, ok := jsonObject(ev, "item")
	if !ok || jsonString(item, "type") != "function_call" {
		return
	}
	key := firstString(item, "call_id", "id")
	if key == "" {
		key = functionCallKey(ev)
	}
	if key == "" {
		return
	}
	var ready *functionCallBuffer
	s.mu.Lock()
	buf := s.bufLocked(key)
	mergeFunctionCallFields(buf, item)
	if args := jsonString(item, "arguments"); args != "" {
		buf.Arguments = args
	}
	buf.Done = true
	if bufReady(buf) && !buf.Dispatched {
		buf.Dispatched = true
		ready = cloneBuf(buf)
	}
	s.mu.Unlock()
	if ready != nil {
		s.dispatchToolCall(ready)
	}
}

func (s *VoiceSession) dispatchToolCall(buf *functionCallBuffer) {
	callID := buf.CallID
	if callID == "" {
		callID = buf.ItemID
	}
	call := threads.ToolCall{CallID: callID, Name: buf.Name, Payload: buf.Arguments}
	s.emit(Event{Type: "tool.call", Name: call.Name, Arguments: call.Payload})

	if call.Name == selfInterruptToolName {
		go s.handleSelfInterruptTool(call)
		return
	}

	if s.opts.ToolRuntime == nil {
		s.emit(Event{Type: "error", Message: fmt.Sprintf("tool call %q requested with no tool runtime", buf.Name)})
		return
	}

	go func() {
		ctx := s.ctx
		ret := func(item threads.Item) error {
			return s.returnToolItem(context.Background(), call, item)
		}
		dispatch, err := s.opts.ToolRuntime.Dispatch(ctx, call, ret)
		if err != nil {
			s.emit(Event{Type: "error", Message: fmt.Sprintf("tool %q: %v", call.Name, err)})
			_ = s.sendToolResult(context.Background(), call, threads.ToolCallResult{CallID: call.CallID, Output: "tool error: " + err.Error()})
			return
		}
		for _, item := range dispatch.Items {
			if err := s.returnToolItem(context.Background(), call, item); err != nil {
				s.emit(Event{Type: "error", Message: err.Error()})
			}
		}
	}()
}

func (s *VoiceSession) handleSelfInterruptTool(call threads.ToolCall) {
	s.mu.Lock()
	s.suppressCancelErrors++
	s.mu.Unlock()

	if err := s.Interrupt(context.Background()); err != nil {
		s.emit(Event{Type: "error", Message: fmt.Sprintf("%s cancel failed: %v", selfInterruptToolName, err)})
	}
	output := "Self-interrupt succeeded. Stop the prior spoken response. Use this tool result as the interruption marker, incorporate the current conversation context, then continue with a corrected concise response."
	if call.Payload != "" {
		output += "\n\nTool arguments: " + call.Payload
	}
	if err := s.sendToolResult(context.Background(), call, threads.ToolCallResult{
		CallID: call.CallID,
		Output: output,
		Data: map[string]any{
			"builtin": "self_interrupt",
		},
	}); err != nil {
		s.emit(Event{Type: "error", Message: fmt.Sprintf("%s result failed: %v", selfInterruptToolName, err)})
	}
}

func (s *VoiceSession) bufLocked(key string) *functionCallBuffer {
	buf := s.callBufs[key]
	if buf == nil {
		buf = &functionCallBuffer{}
		s.callBufs[key] = buf
	}
	return buf
}

func mergeFunctionCallFields(buf *functionCallBuffer, ev map[string]json.RawMessage) {
	if itemID := firstString(ev, "item_id", "itemID", "id"); itemID != "" {
		buf.ItemID = itemID
	}
	if callID := firstString(ev, "call_id", "callID"); callID != "" {
		buf.CallID = callID
	}
	if name := firstString(ev, "name", "function_name"); name != "" {
		buf.Name = name
	}
}

func bufReady(buf *functionCallBuffer) bool {
	return buf.Done && buf.Name != "" && (buf.CallID != "" || buf.ItemID != "")
}

func cloneBuf(buf *functionCallBuffer) *functionCallBuffer {
	copy := *buf
	return &copy
}

func functionCallKey(ev map[string]json.RawMessage) string {
	return firstString(ev, "call_id", "item_id", "id", "output_index")
}

func realtimeErrorMessage(ev map[string]json.RawMessage) string {
	if msg := jsonString(ev, "message"); msg != "" {
		return msg
	}
	obj, ok := jsonObject(ev, "error")
	if !ok {
		return "OpenAI Realtime error"
	}
	if msg := jsonString(obj, "message"); msg != "" {
		return msg
	}
	return "OpenAI Realtime error"
}

func responseCancelled(ev map[string]json.RawMessage) bool {
	response, ok := jsonObject(ev, "response")
	if !ok {
		return false
	}
	return jsonString(response, "status") == "cancelled"
}

func responseIDFromDone(ev map[string]json.RawMessage) string {
	response, ok := jsonObject(ev, "response")
	if !ok {
		return ""
	}
	return jsonString(response, "id")
}

func jsonString(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

func firstString(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if s := jsonString(obj, key); s != "" {
			return s
		}
	}
	return ""
}

func jsonObject(obj map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, ok := obj[key]
	if !ok {
		return nil, false
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

func jsonIntPtr(obj map[string]json.RawMessage, key string) *int {
	raw, ok := obj[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return &n
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if i, err := number.Int64(); err == nil {
			n := int(i)
			return &n
		}
	}
	return nil
}

func cloneRaw(raw []byte) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}
