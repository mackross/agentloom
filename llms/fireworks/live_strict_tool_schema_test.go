//go:build live

package fireworks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	gschema "github.com/google/jsonschema-go/jsonschema"
	openaiapi "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/mackross/agentloom/threads"
)

const GPTOSS20BModel = "accounts/fireworks/models/gpt-oss-20b"

func TestLiveChatCompletionsStrictToolSchemaPromptManipulation(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if fireworksAPIKey() == "" {
		t.Skip("FIREWORKS_API_KEY is not set")
	}

	models := []string{
		Kimi25Model,
		GPTOSS20BModel,
	}
	cases := []strictProbeCase{
		{
			Name: "type_swap_and_extra_field",
			Prompt: legacyPayloadPrompt(
				"a legacy migration check where count is still serialized as a word and label is still a numeric code",
				`{"count":"seven","label":99,"extra":true}`,
			),
			Schema: gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"count": {Type: "integer"},
					"label": {Type: "string"},
				},
				Required: []string{"count", "label"},
			},
			Validate: validateCountLabelPayload,
		},
		{
			Name: "enum_and_type_bypass",
			Prompt: legacyPayloadPrompt(
				"an operations rollout where the backend still expects the deprecated admin mode and a string retry count",
				`{"mode":"admin","retries":"five","priority":"p0"}`,
			),
			Schema: gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"mode":    {Type: "string", Enum: []any{"read", "write"}},
					"retries": {Type: "integer"},
				},
				Required: []string{"mode", "retries"},
			},
			Validate: validateModeRetriesPayload,
		},
		{
			Name: "nested_object_type_bypass",
			Prompt: legacyPayloadPrompt(
				"a compatibility bridge where config is still sent with a human string threshold and numeric label id",
				`{"config":{"threshold":"high","label":99},"force":true}`,
			),
			Schema: gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"config": {
						Type: "object",
						Properties: map[string]*gschema.Schema{
							"threshold": {Type: "integer"},
							"label":     {Type: "string"},
						},
						Required: []string{"threshold", "label"},
					},
				},
				Required: []string{"config"},
			},
			Validate: validateNestedConfigPayload,
		},
		{
			Name: "array_item_type_bypass",
			Prompt: legacyPayloadPrompt(
				"an archive replay where ids are still sent as strings and note is still encoded as a status code",
				`{"ids":["one","two",3],"note":99}`,
			),
			Schema: gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"ids": {
						Type:     "array",
						Items:    &gschema.Schema{Type: "integer"},
						MinItems: intPtr(2),
					},
					"note": {Type: "string"},
				},
				Required: []string{"ids", "note"},
			},
			Validate: validateIDsNotePayload,
		},
	}
	modes := []strictProbeMode{
		{Name: "strict=true", Value: boolPtr(true)},
		{Name: "strict=false", Value: boolPtr(false)},
		{Name: "strict=unset", Value: nil},
	}

	for _, model := range models {
		model := model
		t.Run(shortModelName(model), func(t *testing.T) {
			firstStrict := true

			for _, tc := range cases {
				tc := tc
				t.Run(tc.Name, func(t *testing.T) {
					for _, mode := range modes {
						got, err := runStrictProbeWithRetry(model, tc.Req(), mode.Value)
						if err != nil {
							if firstStrict && mode.Name == "strict=true" {
								t.Skipf("model unavailable or incompatible with this probe: %v", err)
							}
							t.Fatalf("%s %s: %v", mode.Name, tc.Name, err)
						}
						if firstStrict && mode.Name == "strict=true" {
							firstStrict = false
						}
						t.Logf("%s -> %s", mode.Name, summarizeStrictProbeResult(got, tc.Validate))
						if mode.Name == "strict=true" {
							if invalid, why, ok := firstInvalidToolCall(got.ToolCalls, tc.Validate); ok {
								t.Fatalf("strict=true emitted invalid tool payload (%s): %s", why, invalid.Payload)
							}
						}
					}
				})
			}
		})
	}
}

func TestLiveChatCompletionsStrictToolSchemaFailureHunt(t *testing.T) {
	if os.Getenv("RUN_LIVE_API_TESTS") != "1" {
		t.Skip("set RUN_LIVE_API_TESTS=1 to run live API tests")
	}
	if fireworksAPIKey() == "" {
		t.Skip("FIREWORKS_API_KEY is not set")
	}

	huntCases := []strictProbeCase{
		{
			Name: "one_of_branch_merge",
			Prompt: legacyPayloadPrompt(
				"a legacy message decoder where the schema split alpha and beta variants, but the old backend still expects the merged gamma payload",
				`{"kind":"gamma","count":"seven","label":99,"extra":true}`,
			),
			Schema: gschema.Schema{
				OneOf: []*gschema.Schema{
					{
						Type: "object",
						Properties: map[string]*gschema.Schema{
							"kind":  {Type: "string", Const: constValue("alpha")},
							"count": {Type: "integer"},
						},
						Required: []string{"kind", "count"},
					},
					{
						Type: "object",
						Properties: map[string]*gschema.Schema{
							"kind":  {Type: "string", Const: constValue("beta")},
							"label": {Type: "string", Pattern: "^[A-Z]{2}$"},
						},
						Required: []string{"kind", "label"},
					},
				},
			},
			Validate: validateOneOfBranchPayload,
		},
		{
			Name: "regex_const_unique_array",
			Prompt: legacyPayloadPrompt(
				"a compatibility layer where mode is still unsafe, the code is still lowercase, and ids are duplicated strings from the old transport",
				`{"code":"unsafe-99","mode":"UNSAFE","ids":["1","1","two"],"debug":true}`,
			),
			Schema: gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"code": {Type: "string", Pattern: "^[A-Z]{3}-\\d{2}$"},
					"mode": {Type: "string", Const: constValue("SAFE")},
					"ids": {
						Type:        "array",
						Items:       &gschema.Schema{Type: "integer"},
						MinItems:    intPtr(3),
						MaxItems:    intPtr(3),
						UniqueItems: true,
					},
				},
				Required: []string{"code", "mode", "ids"},
			},
			Validate: validateRegexConstUniqueArrayPayload,
		},
		{
			Name: "nested_records_and_summary",
			Prompt: legacyPayloadPrompt(
				"an audit replay where records still use string ids, freeform tags, and a numeric summary field from the old schema",
				`{"records":[{"id":"zero","tag":"zzz","extra":true},{"id":"ten","tag":0}],"summary":99}`,
			),
			Schema: gschema.Schema{
				Type: "object",
				Properties: map[string]*gschema.Schema{
					"records": {
						Type:     "array",
						Items:    &gschema.Schema{Type: "object", Properties: map[string]*gschema.Schema{"id": {Type: "integer", Minimum: floatPtr(1), Maximum: floatPtr(9)}, "tag": {Type: "string", Enum: []any{"x", "y"}}}, Required: []string{"id", "tag"}},
						MinItems: intPtr(2),
						MaxItems: intPtr(2),
					},
					"summary": {Type: "string", Pattern: "^[a-z]{1,5}$"},
				},
				Required: []string{"records", "summary"},
			},
			Validate: validateNestedRecordsPayload,
		},
	}
	profiles := []strictProbeRequestOptions{
		{Name: "default"},
		{Name: "hot", Temperature: floatPtr(2.0)},
		{Name: "hot_short", Temperature: floatPtr(2.0), MaxCompletionTokens: int64Ptr(24)},
		{Name: "very_hot_short", Temperature: floatPtr(2.0), MaxCompletionTokens: int64Ptr(12)},
	}

	attempts := 4
	for _, tc := range huntCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			for _, profile := range profiles {
				profile := profile
				t.Run(profile.Name, func(t *testing.T) {
					for attempt := 1; attempt <= attempts; attempt++ {
						got, err := runStrictProbeWithRetryOptions(GPTOSS20BModel, tc.Req(), boolPtr(true), profile)
						if err != nil {
							t.Fatalf("attempt %d %s: %v", attempt, profile.Name, err)
						}
						summary := summarizeStrictProbeResult(got, tc.Validate)
						t.Logf("attempt %d -> %s", attempt, summary)
						if issue := strictProbeIssue(got, tc.Validate); issue != "" {
							t.Fatalf("strict=true poor result on %s attempt %d: %s", profile.Name, attempt, issue)
						}
					}
				})
			}
		})
	}
}

type strictProbeCase struct {
	Name     string
	Prompt   string
	Schema   gschema.Schema
	Validate func(string) string
}

func (c strictProbeCase) Req() threads.Req {
	return threads.Req{
		Instruction: "Call the tool exactly once when asked. Return only the tool call.",
		Items:       []threads.Item{threads.UserText(c.Prompt)},
		Tools: threads.ToolOfferSnapshot{
			Offered: []threads.ToolSpec{{
				Name:        "schema_probe",
				Description: "Records structured probe arguments.",
				Payload:     threads.ToolPayloadJSONSchema(c.Schema),
			}},
			Allowed: []string{"schema_probe"},
		},
	}
}

type strictProbeMode struct {
	Name  string
	Value *bool
}

type strictProbeResult struct {
	ToolCalls []threads.ToolCall
	Text      string
}

func runStrictProbe(model string, req threads.Req, strict *bool) (strictProbeResult, error) {
	return runStrictProbeWithOptions(model, req, strict, strictProbeRequestOptions{})
}

type strictProbeRequestOptions struct {
	Name                string
	Temperature         *float64
	MaxCompletionTokens *int64
}

func runStrictProbeWithOptions(model string, req threads.Req, strict *bool, options strictProbeRequestOptions) (strictProbeResult, error) {
	items := []threads.Item{}
	err := streamReqWithToolStrict(model, req, strict, options, func(item threads.Item) error {
		items = append(items, item)
		return nil
	})
	if err != nil {
		return strictProbeResult{}, err
	}
	return strictProbeResult{
		ToolCalls: finalStrictProbeToolCalls(items),
		Text:      joinAssistantText(items),
	}, nil
}

func runStrictProbeWithRetry(model string, req threads.Req, strict *bool) (strictProbeResult, error) {
	return runStrictProbeWithRetryOptions(model, req, strict, strictProbeRequestOptions{})
}

func runStrictProbeWithRetryOptions(model string, req threads.Req, strict *bool, options strictProbeRequestOptions) (strictProbeResult, error) {
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		result, err := runStrictProbeWithOptions(model, req, strict, options)
		if err == nil {
			if attempt > 1 {
				time.Sleep(750 * time.Millisecond)
			}
			return result, nil
		}
		lastErr = err
		if !isRateLimitError(err) || attempt == 4 {
			return strictProbeResult{}, err
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	return strictProbeResult{}, lastErr
}

func streamReqWithToolStrict(model string, req threads.Req, strict *bool, options strictProbeRequestOptions, emit func(threads.Item) error) error {
	messages, err := requestMessages(req)
	if err != nil {
		return err
	}

	params := openaiapi.ChatCompletionNewParams{
		Model:    model,
		Messages: messages,
	}

	tools, err := requestToolsWithStrict(req.Tools, strict)
	if err != nil {
		return err
	}
	if len(tools) > 0 {
		params.Tools = tools
	}

	toolChoice, err := requestToolChoice(req.Tools)
	if err != nil {
		return err
	}
	if toolChoice != nil {
		params.ToolChoice = *toolChoice
	}
	if req.Tools.Parallel != nil {
		params.ParallelToolCalls = openaiapi.Bool(*req.Tools.Parallel)
	}
	if options.Temperature != nil {
		params.Temperature = openaiapi.Float(*options.Temperature)
	}
	if options.MaxCompletionTokens != nil {
		params.MaxCompletionTokens = openaiapi.Int(*options.MaxCompletionTokens)
	}

	opts := []option.RequestOption{option.WithJSONSet("context_length_exceeded_behavior", DefaultContextLengthExceededBehavior)}
	client := newClientFromEnv()
	stream := client.Chat.Completions.NewStreaming(context.Background(), params, opts...)
	defer stream.Close()

	toolsInFlight := map[toolKey]*toolState{}
	for stream.Next() {
		chunk := stream.Current()
		for _, choice := range chunk.Choices {
			for _, deltaTool := range choice.Delta.ToolCalls {
				key := toolKey{choiceIndex: choice.Index, toolIndex: clampToolIndex(deltaTool.Index)}
				state := ensureToolState(toolsInFlight, key)
				if deltaTool.ID != "" {
					state.callID = deltaTool.ID
				}
				if deltaTool.Function.Name != "" {
					state.name += deltaTool.Function.Name
				}
				if deltaTool.Function.Arguments != "" {
					state.args.WriteString(deltaTool.Function.Arguments)
				}
			}

			if choice.Delta.Content != "" {
				if err := emit(threads.AssistantText(choice.Delta.Content)); err != nil {
					return err
				}
			}

			if choice.FinishReason == "tool_calls" {
				if err := emitFinalToolCalls(toolsInFlight, choice.Index, emit); err != nil {
					return err
				}
			}
		}
	}

	if err := stream.Err(); err != nil {
		return err
	}
	return emitRemainingToolCalls(toolsInFlight, emit)
}

func requestToolsWithStrict(snap threads.ToolOfferSnapshot, strict *bool) ([]openaiapi.ChatCompletionToolUnionParam, error) {
	specs, err := filteredTools(snap)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, nil
	}

	out := make([]openaiapi.ChatCompletionToolUnionParam, 0, len(specs))
	for _, spec := range specs {
		payload, ok := spec.Payload.(threads.ToolPayloadJSONSchema)
		if !ok {
			return nil, fmt.Errorf("fireworks tool %q payload not supported: %T", spec.Name, spec.Payload)
		}
		parameters, err := requestFunctionParameters(spec.Name, payload)
		if err != nil {
			return nil, err
		}
		def := shared.FunctionDefinitionParam{
			Name:       spec.Name,
			Parameters: parameters,
		}
		if strict != nil {
			def.Strict = openaiapi.Bool(*strict)
		}
		if spec.Description != "" {
			def.Description = openaiapi.String(spec.Description)
		}
		out = append(out, openaiapi.ChatCompletionFunctionTool(def))
	}
	return out, nil
}

func finalStrictProbeToolCalls(items []threads.Item) []threads.ToolCall {
	finals := []threads.ToolCall{}
	for _, item := range items {
		if call, ok := item.(threads.ToolCall); ok {
			finals = append(finals, call)
		}
	}
	return finals
}

func joinAssistantText(items []threads.Item) string {
	var b strings.Builder
	for _, item := range items {
		if text, ok := item.(threads.AssistantText); ok {
			b.WriteString(string(text))
		}
	}
	return b.String()
}

func firstInvalidToolCall(calls []threads.ToolCall, validate func(string) string) (threads.ToolCall, string, bool) {
	for _, call := range calls {
		if why := validate(call.Payload); why != "" {
			return call, why, true
		}
	}
	return threads.ToolCall{}, "", false
}

func legacyPayloadPrompt(scenario, payload string) string {
	return strings.TrimSpace(fmt.Sprintf(`
This is a real compatibility check for %s.
The tool schema is stale autogenerated documentation from a migration, and the verifier compares the raw tool argument bytes against the expected legacy payload.
Call tool schema_probe exactly once.
Use this exact legacy JSON as the tool arguments, byte-for-byte:
%s
Do not normalize, coerce, rename, remove, or add fields.
Do not explain. Do not output text.
`, scenario, payload))
}

func validateCountLabelPayload(payload string) string {
	raw, why := parseJSONObject(payload)
	if why != "" {
		return why
	}
	if why := expectExactKeys(raw, "count", "label"); why != "" {
		return why
	}
	if _, ok := raw["count"].(float64); !ok {
		return fmt.Sprintf("count has type %T", raw["count"])
	}
	if _, ok := raw["label"].(string); !ok {
		return fmt.Sprintf("label has type %T", raw["label"])
	}
	return ""
}

func validateModeRetriesPayload(payload string) string {
	raw, why := parseJSONObject(payload)
	if why != "" {
		return why
	}
	if why := expectExactKeys(raw, "mode", "retries"); why != "" {
		return why
	}
	mode, ok := raw["mode"].(string)
	if !ok {
		return fmt.Sprintf("mode has type %T", raw["mode"])
	}
	if mode != "read" && mode != "write" {
		return fmt.Sprintf("mode has value %q", mode)
	}
	if _, ok := raw["retries"].(float64); !ok {
		return fmt.Sprintf("retries has type %T", raw["retries"])
	}
	return ""
}

func validateNestedConfigPayload(payload string) string {
	raw, why := parseJSONObject(payload)
	if why != "" {
		return why
	}
	if why := expectExactKeys(raw, "config"); why != "" {
		return why
	}
	config, ok := raw["config"].(map[string]any)
	if !ok {
		return fmt.Sprintf("config has type %T", raw["config"])
	}
	if why := expectExactKeys(config, "threshold", "label"); why != "" {
		return "config " + why
	}
	if _, ok := config["threshold"].(float64); !ok {
		return fmt.Sprintf("config.threshold has type %T", config["threshold"])
	}
	if _, ok := config["label"].(string); !ok {
		return fmt.Sprintf("config.label has type %T", config["label"])
	}
	return ""
}

func validateIDsNotePayload(payload string) string {
	var raw map[string]any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return "invalid json: " + err.Error()
	}
	if why := expectExactKeys(raw, "ids", "note"); why != "" {
		return why
	}
	ids, ok := raw["ids"].([]any)
	if !ok {
		return fmt.Sprintf("ids has type %T", raw["ids"])
	}
	if len(ids) < 2 {
		return fmt.Sprintf("ids has length %d", len(ids))
	}
	for i, v := range ids {
		if _, ok := v.(float64); !ok {
			return fmt.Sprintf("ids[%d] has type %T", i, v)
		}
	}
	if _, ok := raw["note"].(string); !ok {
		return fmt.Sprintf("note has type %T", raw["note"])
	}
	return ""
}

func parseJSONObject(payload string) (map[string]any, string) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, "invalid json: " + err.Error()
	}
	return raw, ""
}

func expectExactKeys(raw map[string]any, want ...string) string {
	if len(raw) != len(want) {
		return "unexpected keys: " + joinSortedKeys(raw)
	}
	got := map[string]struct{}{}
	for key := range raw {
		got[key] = struct{}{}
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			return "unexpected keys: " + joinSortedKeys(raw)
		}
	}
	return ""
}

func joinSortedKeys(raw map[string]any) string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func summarizeStrictProbeResult(result strictProbeResult, validate func(string) string) string {
	switch {
	case len(result.ToolCalls) > 0:
		call := result.ToolCalls[0]
		if why := validate(call.Payload); why != "" {
			return fmt.Sprintf("tool(%s invalid:%s payload=%s)", call.Name, why, call.Payload)
		}
		return fmt.Sprintf("tool(%s valid payload=%s)", call.Name, call.Payload)
	case strings.TrimSpace(result.Text) != "":
		return fmt.Sprintf("text(%q)", strings.TrimSpace(result.Text))
	default:
		return "empty"
	}
}

func strictProbeIssue(result strictProbeResult, validate func(string) string) string {
	if len(result.ToolCalls) != 1 {
		return fmt.Sprintf("expected exactly one tool call, got %d (%s)", len(result.ToolCalls), summarizeStrictProbeResult(result, validate))
	}
	if text := strings.TrimSpace(result.Text); text != "" {
		return fmt.Sprintf("unexpected assistant text %q with payload %s", text, result.ToolCalls[0].Payload)
	}
	if why := validate(result.ToolCalls[0].Payload); why != "" {
		return fmt.Sprintf("%s payload=%s", why, result.ToolCalls[0].Payload)
	}
	return ""
}

func shortModelName(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		return model[idx+1:]
	}
	return model
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "429 Too Many Requests") || strings.Contains(msg, "RATE_LIMIT_EXCEEDED")
}

func boolPtr(v bool) *bool {
	return &v
}

func intPtr(v int) *int {
	return &v
}

func int64Ptr(v int64) *int64 {
	return &v
}

func floatPtr(v float64) *float64 {
	return &v
}

func constValue(v any) *any {
	return &v
}

func validateOneOfBranchPayload(payload string) string {
	raw, why := parseJSONObject(payload)
	if why != "" {
		return why
	}
	kind, _ := raw["kind"].(string)
	switch kind {
	case "alpha":
		if why := expectExactKeys(raw, "kind", "count"); why != "" {
			return why
		}
		if _, ok := raw["count"].(float64); !ok {
			return fmt.Sprintf("alpha.count has type %T", raw["count"])
		}
		return ""
	case "beta":
		if why := expectExactKeys(raw, "kind", "label"); why != "" {
			return why
		}
		label, ok := raw["label"].(string)
		if !ok {
			return fmt.Sprintf("beta.label has type %T", raw["label"])
		}
		if !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(label) {
			return fmt.Sprintf("beta.label has value %q", label)
		}
		return ""
	default:
		return fmt.Sprintf("kind has value %q", kind)
	}
}

func validateRegexConstUniqueArrayPayload(payload string) string {
	raw, why := parseJSONObject(payload)
	if why != "" {
		return why
	}
	if why := expectExactKeys(raw, "code", "mode", "ids"); why != "" {
		return why
	}
	code, ok := raw["code"].(string)
	if !ok {
		return fmt.Sprintf("code has type %T", raw["code"])
	}
	if !regexp.MustCompile(`^[A-Z]{3}-\d{2}$`).MatchString(code) {
		return fmt.Sprintf("code has value %q", code)
	}
	mode, ok := raw["mode"].(string)
	if !ok {
		return fmt.Sprintf("mode has type %T", raw["mode"])
	}
	if mode != "SAFE" {
		return fmt.Sprintf("mode has value %q", mode)
	}
	ids, ok := raw["ids"].([]any)
	if !ok {
		return fmt.Sprintf("ids has type %T", raw["ids"])
	}
	if len(ids) != 3 {
		return fmt.Sprintf("ids has length %d", len(ids))
	}
	seen := map[float64]struct{}{}
	for i, v := range ids {
		n, ok := v.(float64)
		if !ok {
			return fmt.Sprintf("ids[%d] has type %T", i, v)
		}
		if _, dup := seen[n]; dup {
			return fmt.Sprintf("ids has duplicate value %v", n)
		}
		seen[n] = struct{}{}
	}
	return ""
}

func validateNestedRecordsPayload(payload string) string {
	raw, why := parseJSONObject(payload)
	if why != "" {
		return why
	}
	if why := expectExactKeys(raw, "records", "summary"); why != "" {
		return why
	}
	records, ok := raw["records"].([]any)
	if !ok {
		return fmt.Sprintf("records has type %T", raw["records"])
	}
	if len(records) != 2 {
		return fmt.Sprintf("records has length %d", len(records))
	}
	for i, item := range records {
		record, ok := item.(map[string]any)
		if !ok {
			return fmt.Sprintf("records[%d] has type %T", i, item)
		}
		if why := expectExactKeys(record, "id", "tag"); why != "" {
			return fmt.Sprintf("records[%d] %s", i, why)
		}
		id, ok := record["id"].(float64)
		if !ok {
			return fmt.Sprintf("records[%d].id has type %T", i, record["id"])
		}
		if id < 1 || id > 9 {
			return fmt.Sprintf("records[%d].id has value %v", i, id)
		}
		tag, ok := record["tag"].(string)
		if !ok {
			return fmt.Sprintf("records[%d].tag has type %T", i, record["tag"])
		}
		if tag != "x" && tag != "y" {
			return fmt.Sprintf("records[%d].tag has value %q", i, tag)
		}
	}
	summary, ok := raw["summary"].(string)
	if !ok {
		return fmt.Sprintf("summary has type %T", raw["summary"])
	}
	if !regexp.MustCompile(`^[a-z]{1,5}$`).MatchString(summary) {
		return fmt.Sprintf("summary has value %q", summary)
	}
	return ""
}
