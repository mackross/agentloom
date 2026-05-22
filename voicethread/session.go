package voicethread

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/mackross/agentloom/threads"
)

// Event is the small server-to-browser event shape used by the voice spike.
type Event struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Data         string          `json:"data,omitempty"`
	Name         string          `json:"name,omitempty"`
	Arguments    string          `json:"arguments,omitempty"`
	Output       string          `json:"output,omitempty"`
	Message      string          `json:"message,omitempty"`
	ItemID       string          `json:"item_id,omitempty"`
	ContentIndex *int            `json:"content_index,omitempty"`
	ResponseID   string          `json:"response_id,omitempty"`
	ClientTimeMS int64           `json:"client_time_ms,omitempty"`
	ServerTimeMS int64           `json:"server_time_ms,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

// EventHandler receives mapped Realtime/browser events.
type EventHandler func(Event)

// Options configures a VoiceSession.
type Options struct {
	APIKey       string
	Model        string
	Voice        string
	Instructions string

	// BaseURL defaults to wss://api.openai.com/v1/realtime.
	BaseURL string

	// TranscriptionModel defaults to gpt-realtime-whisper.
	TranscriptionModel string
	// TranscriptionLanguage is optional, e.g. "en".
	TranscriptionLanguage string

	ToolRuntime ToolRuntime
	OnEvent     EventHandler
}

// VoiceSession owns one OpenAI Realtime websocket session.
type VoiceSession struct {
	opts Options

	ctx    context.Context
	cancel context.CancelFunc

	conn   realtimeConn
	sendMu sync.Mutex

	mu         sync.Mutex
	callBufs   map[string]*functionCallBuffer
	dispatched map[string]struct{}

	responseKinds        map[string]string
	conversationItemIDs  []string
	lastSummaryItemIndex int
	suppressCancelErrors int
}

// New creates a VoiceSession. Call Start to connect to OpenAI.
func New(opts Options) *VoiceSession {
	if opts.Model == "" {
		opts.Model = "gpt-realtime-2"
	}
	if opts.Voice == "" {
		opts.Voice = "marin"
	}
	if opts.Instructions == "" {
		opts.Instructions = "You are a concise, helpful voice assistant. Speak clearly and briefly."
	}
	if opts.TranscriptionModel == "" {
		opts.TranscriptionModel = "gpt-realtime-whisper"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &VoiceSession{
		opts:       opts,
		ctx:        ctx,
		cancel:     cancel,
		callBufs:   map[string]*functionCallBuffer{},
		dispatched: map[string]struct{}{},
		responseKinds: map[string]string{},
	}
}

// Start connects to OpenAI Realtime and sends the initial session.update.
func (s *VoiceSession) Start(ctx context.Context) error {
	if s.opts.APIKey == "" {
		return fmt.Errorf("voicethread: missing OpenAI API key")
	}
	conn, err := dialRealtime(ctx, s.opts)
	if err != nil {
		return err
	}
	s.conn = conn

	if err := s.sendSessionUpdate(ctx); err != nil {
		_ = conn.CloseNow()
		return err
	}
	s.emit(Event{Type: "session.started"})
	go s.readLoop()
	return nil
}

// Close closes the session and websocket.
func (s *VoiceSession) Close() error {
	s.cancel()
	if s.conn == nil {
		return nil
	}
	return s.conn.CloseNow()
}

// AppendAudio sends one base64-encoded PCM16 audio chunk to OpenAI.
func (s *VoiceSession) AppendAudio(ctx context.Context, base64PCM16 string) error {
	return s.sendJSON(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64PCM16,
	})
}

// CommitAudio commits the input audio buffer and asks the model to respond.
func (s *VoiceSession) CommitAudio(ctx context.Context) error {
	if err := s.sendJSON(ctx, map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		return err
	}
	return s.CreateResponse(ctx)
}

// SendText sends a user text item and asks the model to respond.
func (s *VoiceSession) SendText(ctx context.Context, text string) error {
	if err := s.sendJSON(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": text,
			}},
		},
	}); err != nil {
		return err
	}
	return s.CreateResponse(ctx)
}

// SummarizeSinceLast requests an out-of-band text-only summary of conversation
// items added since the previous successful summary request. The summary
// response is not added to the default conversation.
func (s *VoiceSession) SummarizeSinceLast(ctx context.Context) error {
	s.mu.Lock()
	refs := append([]string(nil), s.conversationItemIDs[s.lastSummaryItemIndex:]...)
	s.mu.Unlock()

	input := make([]map[string]any, 0, len(refs)+1)
	for _, id := range refs {
		input = append(input, map[string]any{
			"type": "item_reference",
			"id":   id,
		})
	}
	input = append(input, map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]any{{
			"type": "input_text",
			"text": "Summarize everything in these conversation items since the last summary. Be concise but include important decisions, unresolved questions, and useful details for continuing the session.",
		}},
	})

	return s.sendJSON(ctx, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"conversation":      "none",
			"metadata":          map[string]any{"agentloom_kind": "summary"},
			"output_modalities": []string{"text"},
			"instructions":      "You are producing a private running summary for the UI. Do not speak. Return only the summary text.",
			"input":             input,
		},
	})
}

// Interrupt cancels the current Realtime response if one is active.
func (s *VoiceSession) Interrupt(ctx context.Context) error {
	return s.sendJSON(ctx, map[string]any{"type": "response.cancel"})
}

// TruncateAssistantAudio tells Realtime how much of an assistant audio item was
// actually heard by the user. This should be sent when local playback is cut off
// by a button interrupt or barge-in, so future model turns do not include
// unheard assistant audio.
func (s *VoiceSession) TruncateAssistantAudio(ctx context.Context, itemID string, contentIndex, audioEndMS int) error {
	if itemID == "" {
		return fmt.Errorf("voicethread: truncate requires item id")
	}
	if contentIndex < 0 {
		return fmt.Errorf("voicethread: truncate requires non-negative content index")
	}
	if audioEndMS < 0 {
		audioEndMS = 0
	}
	return s.sendJSON(ctx, map[string]any{
		"type":          "conversation.item.truncate",
		"item_id":       itemID,
		"content_index": contentIndex,
		"audio_end_ms":  audioEndMS,
	})
}

// CreateResponse asks OpenAI Realtime to continue/respond.
func (s *VoiceSession) CreateResponse(ctx context.Context) error {
	return s.sendJSON(ctx, map[string]any{"type": "response.create"})
}

func (s *VoiceSession) sendSessionUpdate(ctx context.Context) error {
	session := map[string]any{
		"type":              "realtime",
		"model":             s.opts.Model,
		"instructions":      s.opts.Instructions,
		"output_modalities": []string{"audio"},
		"audio": map[string]any{
			"input": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				"turn_detection": map[string]any{
					"type":            "semantic_vad",
					"create_response": true,
				},
				"transcription": transcriptionConfig(s.opts),
			},
			"output": map[string]any{
				"format": map[string]any{"type": "audio/pcm", "rate": 24000},
				"voice":  s.opts.Voice,
			},
		},
	}
	tools := []map[string]any{selfInterruptToolForRealtime()}
	if s.opts.ToolRuntime != nil {
		runtimeTools, err := toolsForRealtime(s.opts.ToolRuntime.Snapshot())
		if err != nil {
			return err
		}
		tools = append(tools, runtimeTools...)
	}
	if len(tools) > 0 {
		session["tools"] = tools
		session["tool_choice"] = "auto"
	}
	return s.sendJSON(ctx, map[string]any{
		"type":    "session.update",
		"session": session,
	})
}

func transcriptionConfig(opts Options) map[string]any {
	cfg := map[string]any{"model": opts.TranscriptionModel}
	if opts.TranscriptionLanguage != "" {
		cfg["language"] = opts.TranscriptionLanguage
	}
	return cfg
}

func (s *VoiceSession) sendJSON(ctx context.Context, v any) error {
	if s.conn == nil {
		return fmt.Errorf("voicethread: session not started")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.conn.Write(ctx, b)
}

func (s *VoiceSession) readLoop() {
	for {
		b, err := s.conn.Read(s.ctx)
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				s.emit(Event{Type: "error", Message: err.Error()})
				return
			}
		}
		s.handleRealtimeEvent(b)
	}
}

func (s *VoiceSession) emit(ev Event) {
	if s.opts.OnEvent != nil {
		s.opts.OnEvent(ev)
	}
}

func (s *VoiceSession) returnToolItem(ctx context.Context, call threads.ToolCall, item threads.Item) error {
	switch v := item.(type) {
	case threads.ToolCallResult:
		return s.sendToolResult(ctx, call, v)
	case *threads.ToolCallResult:
		if v == nil {
			return fmt.Errorf("tool %q returned nil *ToolCallResult", call.Name)
		}
		return s.sendToolResult(ctx, call, *v)
	default:
		return fmt.Errorf("tool %q returned unsupported item %T", call.Name, item)
	}
}

func (s *VoiceSession) sendToolResult(ctx context.Context, call threads.ToolCall, result threads.ToolCallResult) error {
	output := result.Output
	if output == "" {
		output = "{}"
	}
	s.emit(Event{Type: "tool.result", Name: call.Name, Output: output})
	if err := s.sendJSON(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": call.CallID,
			"output":  output,
		},
	}); err != nil {
		return err
	}
	return s.CreateResponse(ctx)
}
