//go:build live

package llms_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	gschema "github.com/google/jsonschema-go/jsonschema"

	"github.com/mackross/agentloom/llms/anthropic"
	"github.com/mackross/agentloom/llms/cerebras"
	"github.com/mackross/agentloom/llms/fireworks"
	openaiwrap "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/llms/xai"
	"github.com/mackross/agentloom/threads"
)

func TestLiveParallelRecoveryAndPostRecoveryRequests(t *testing.T) {

	tests := []struct {
		name       string
		keyEnv     string
		callPrefix string
		new        func() threads.LLMStreamer
	}{
		{
			name:       "openai",
			keyEnv:     "OPENAI_API_KEY",
			callPrefix: "call_",
			new: func() threads.LLMStreamer {
				s := openaiwrap.NewResponsesStreamer(envOr("OPENAI_MODEL", openaiwrap.DefaultModel))
				s.Transport = openaiwrap.ResponsesTransportSSE
				return s
			},
		},
		{
			name:       "anthropic",
			keyEnv:     "ANTHROPIC_API_KEY",
			callPrefix: "toolu_",
			new: func() threads.LLMStreamer {
				return anthropic.NewMessagesStreamer(envOr("ANTHROPIC_MODEL", string(anthropic.DefaultModel)))
			},
		},
		{
			name:       "xai",
			keyEnv:     "XAI_API_KEY",
			callPrefix: "call_",
			new: func() threads.LLMStreamer {
				return xai.NewResponsesStreamer(envOr("XAI_MODEL", xai.DefaultModel))
			},
		},
		{
			name:       "fireworks",
			keyEnv:     "FIREWORKS_API_KEY",
			callPrefix: "call_",
			new: func() threads.LLMStreamer {
				return fireworks.NewChatCompletionsStreamer(fireworks.Kimi3Model)
			},
		},
		{
			name:       "cerebras",
			keyEnv:     "CEREBRAS_API_KEY",
			callPrefix: "call_",
			new: func() threads.LLMStreamer {
				return cerebras.NewChatCompletionsStreamer(cerebras.DefaultModel)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.TrimSpace(os.Getenv(tt.keyEnv)) == "" {
				t.Fatalf("%s is not set", tt.keyEnv)
			}
			streamer := tt.new()
			tools := parallelRecoveryTools()
			recovery, recovered := parallelRecoveryRequests(t, tt.callPrefix, tools)

			if err := streamer.StreamReq(recovery, func(threads.Item) error { return nil }); err != nil {
				t.Fatalf("provider rejected projected parallel recovery request: %v", err)
			}
			if err := streamer.StreamReq(recovered, func(threads.Item) error { return nil }); err != nil {
				t.Fatalf("provider rejected post-recovery request without hints: %v", err)
			}
		})
	}
}

func parallelRecoveryRequests(t testing.TB, callPrefix string, tools threads.ToolOfferSnapshot) (threads.Req, threads.Req) {
	t.Helper()
	alpha := threads.ToolCall{CallID: callPrefix + "parallel_alpha", Name: "alpha_once", Payload: `{"token":"alpha"}`}
	beta := threads.ToolCall{CallID: callPrefix + "parallel_beta", Name: "beta_once", Payload: `{"token":"beta"}`}
	failed := threads.ToolCall{CallID: callPrefix + "failed_gamma", Name: "gamma_once", Payload: `{"token":`}
	repaired := threads.ToolCall{CallID: callPrefix + "repaired_gamma", Name: "gamma_once", Payload: `{"token":"gamma"}`}
	hint := `<tool_call_hint tool="gamma_once">Call "gamma_once" exactly once with {"token":"gamma"}.</tool_call_hint>`
	base := []threads.Item{
		threads.UserText("Run the alpha, beta, and gamma probes."),
		alpha,
		beta,
		failed,
		threads.ToolCallResult{CallID: alpha.CallID, Output: `{"ok":true}`},
		threads.ToolCallResult{CallID: beta.CallID, Output: `{"ok":true}`},
		threads.ToolCallResult{CallID: failed.CallID, Output: "invalid JSON", SafeRollback: &threads.ToolCallSafeRollback{
			SteeringHint: hint,
			RetryAttempt: 1,
			MaxRetries:   2,
		}},
	}
	recovery := threads.DefaultRequestBuilder.Build(base, threads.StreamerCapabilities{AssistantPrefix: true})
	recovery.Tools = tools
	recoveredTranscript := append(append([]threads.Item(nil), base...),
		repaired,
		threads.ToolCallResult{CallID: repaired.CallID, Output: `{"ok":true}`},
	)
	recovered := threads.DefaultRequestBuilder.Build(recoveredTranscript, threads.StreamerCapabilities{AssistantPrefix: true})
	recovered.Tools = tools

	wantRecovery := []threads.Item{
		threads.UserText("Run the alpha, beta, and gamma probes."),
		alpha,
		beta,
		threads.ToolCallResult{CallID: alpha.CallID, Output: `{"ok":true}`},
		threads.ToolCallResult{CallID: beta.CallID, Output: `{"ok":true}`},
		threads.UserText(hint),
	}
	if !reflect.DeepEqual(recovery.Items, wantRecovery) {
		t.Fatalf("unexpected projected recovery request:\n got: %#v\nwant: %#v", recovery.Items, wantRecovery)
	}
	wantRecovered := append(append([]threads.Item(nil), wantRecovery[:5]...),
		repaired,
		threads.ToolCallResult{CallID: repaired.CallID, Output: `{"ok":true}`},
	)
	if !reflect.DeepEqual(recovered.Items, wantRecovered) {
		t.Fatalf("successful retry retained rollback exchange or hint:\n got: %#v\nwant: %#v", recovered.Items, wantRecovered)
	}
	return recovery, recovered
}

func parallelRecoveryTools() threads.ToolOfferSnapshot {
	parallel := true
	tool := func(name string) threads.ToolSpec {
		return threads.ToolSpec{
			Name:        name,
			Description: "Records the supplied probe token.",
			Payload: threads.ToolPayloadJSONSchema(gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"token": {Type: "string"},
				},
				Required: []string{"token"},
			}),
		}
	}
	return threads.ToolOfferSnapshot{
		Offered: []threads.ToolSpec{
			tool("alpha_once"),
			tool("beta_once"),
			tool("gamma_once"),
		},
		Parallel: &parallel,
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
