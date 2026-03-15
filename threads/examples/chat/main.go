package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/durability"
	"github.com/mackross/agentloom/threads/simpletool"
	"strings"

	"github.com/dop251/goja"

	fireworkswrap "github.com/mackross/agentloom/llms/fireworks"
	openaiwrap "github.com/mackross/agentloom/llms/openai"

	anthropicwrap "github.com/mackross/agentloom/llms/anthropic"
)

func main() {
	model := configuredModel()
	if !hasProviderAPIKey(model) {
		fmt.Fprintf(os.Stderr, "set %s\n", requiredAPIKeyLabel(model))
		os.Exit(1)
	}

	streamer, resolvedModel := newStreamerForModel(model)
	executor := threads.NewThreadExecutor(streamer)
	currentModel := resolvedModel
	delegate := threads.ThreadDelegateFuncs{
		OnRequest: func(_ *threads.Thread) {
			fmt.Print("ai> ")
		},
		OnStreamItemAppended: func(_ *threads.Thread, item threads.Item) {
			if text, ok := item.(threads.AssistantText); ok {
				fmt.Print(string(text))
			}
		},
		OnIdle: func(_ *threads.Thread) {
			fmt.Println()
		},
	}

	thread := threads.New()
	configureThread(thread, executor, delegate)
	var store threads.DurableStore
	var storePath string

	fmt.Printf("model: %s\n", currentModel)
	fmt.Println("type your message and press enter")
	fmt.Println("commands: /instruction <text>, /model <name>, /save <file>, /load <file>, /checkpoint, /exit, /quit")
	fmt.Println("tool: javascript(code) runs arbitrary JavaScript in a fresh goja sandbox")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("you> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		if strings.HasPrefix(line, "/instruction") {
			instruction := strings.TrimSpace(strings.TrimPrefix(line, "/instruction"))
			if instruction == "" {
				fmt.Println("usage: /instruction <text>")
				continue
			}
			thread.QueueItem(threads.AssistantInstruction(instruction))
			fmt.Println("instruction updated")
			continue
		}
		if strings.HasPrefix(line, "/model") {
			nextModel := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
			if nextModel == "" {
				fmt.Println("current model:", currentModel)
				continue
			}
			nextExecutor, resolvedModel, err := switchModelIfIdle(thread, nextModel)
			if err != nil {
				fmt.Println("model switch error:", err)
				continue
			}
			executor = nextExecutor
			currentModel = resolvedModel
			fmt.Println("model switched:", currentModel)
			continue
		}
		if strings.HasPrefix(line, "/save") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "/save"))
			if path == "" {
				fmt.Println("usage: /save <file>")
				continue
			}
			s := durability.NewFileStore(path)
			if err := runSafe(func() { thread.SetDurableStore(s) }); err != nil {
				fmt.Println("save error:", err)
				continue
			}
			store = s
			storePath = path
			fmt.Println("durable store set:", path)
			continue
		}
		if strings.HasPrefix(line, "/load") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "/load"))
			if path == "" {
				fmt.Println("usage: /load <file>")
				continue
			}
			s := durability.NewFileStore(path)
			cp, wal, err := loadFromStore(s)
			if err != nil {
				fmt.Println("load error:", err)
				continue
			}
			loaded, err := threads.RestoreFromCheckpointAndWAL(cp, wal, threads.RestoreOptions{})
			if err != nil {
				fmt.Println("load error:", err)
				continue
			}
			configureThread(loaded, executor, delegate)
			if err := runSafe(func() { loaded.SetDurableStore(s) }); err != nil {
				fmt.Println("load attach store error:", err)
				continue
			}
			thread = loaded
			store = s
			storePath = path
			fmt.Println("loaded:", path)
			continue
		}
		if line == "/checkpoint" {
			cp, err := thread.Checkpoint(threads.CheckpointOptions{Policy: threads.InflightSkip})
			if err != nil {
				fmt.Println("checkpoint error:", err)
				continue
			}
			if store == nil {
				fmt.Printf("checkpoint created (not persisted), seq %d\n", cp.Seq)
				continue
			}
			if err := runSafe(func() { store.ReplaceSnapshot(cp) }); err != nil {
				fmt.Println("checkpoint persist error:", err)
				continue
			}
			fmt.Printf("checkpoint persisted: %s (seq %d)\n", storePath, cp.Seq)
			continue
		}

		thread.QueueItem(threads.UserText(line))
		thread.QueueItem(threads.SendItem{})
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "stdin error:", err)
		os.Exit(1)
	}
}

func configuredModel() string {
	for _, key := range []string{"MODEL", "OPENAI_MODEL", "ANTHROPIC_MODEL", "FIREWORKS_MODEL"} {
		if model := strings.TrimSpace(os.Getenv(key)); model != "" {
			return model
		}
	}
	return ""
}

type exampleProvider string

const (
	exampleProviderOpenAI    exampleProvider = "openai"
	exampleProviderAnthropic exampleProvider = "anthropic"
	exampleProviderFireworks exampleProvider = "fireworks"
)

func providerForModel(model string) exampleProvider {
	model = strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(model, "claude") {
		return exampleProviderAnthropic
	}
	if strings.HasPrefix(model, "accounts/fireworks/models/") {
		return exampleProviderFireworks
	}
	return exampleProviderOpenAI
}

func requiredAPIKeyLabel(model string) string {
	switch providerForModel(model) {
	case exampleProviderAnthropic:
		return "ANTHROPIC_API_KEY"
	case exampleProviderFireworks:
		return "FIREWORKS_API_KEY"
	default:
		return "OPENAI_API_KEY"
	}
}

func hasProviderAPIKey(model string) bool {
	switch providerForModel(model) {
	case exampleProviderAnthropic:
		return strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != ""
	case exampleProviderFireworks:
		return strings.TrimSpace(os.Getenv("FIREWORKS_API_KEY")) != "" || strings.TrimSpace(os.Getenv("FIREWORKS_AI_API_KEY")) != ""
	default:
		return strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != ""
	}
}

func newStreamerForModel(model string) (threads.LLMStreamer, string) {
	model = strings.TrimSpace(model)
	switch providerForModel(model) {
	case exampleProviderAnthropic:
		return anthropicwrap.NewMessagesStreamer(model), modelOrDefault(model)
	case exampleProviderFireworks:
		return fireworkswrap.NewChatCompletionsStreamer(model), modelOrDefault(model)
	default:
		return openaiwrap.NewResponsesStreamer(model), modelOrDefault(model)
	}
}

func switchModelIfIdle(thread *threads.Thread, model string) (*threads.ThreadExecutor, string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, "", fmt.Errorf("usage: /model <name>")
	}
	if thread.State() != threads.StateIdle {
		return nil, "", fmt.Errorf("thread is %s", thread.State())
	}
	if !hasProviderAPIKey(model) {
		return nil, "", fmt.Errorf("set %s", requiredAPIKeyLabel(model))
	}
	streamer, resolvedModel := newStreamerForModel(model)
	executor := threads.NewThreadExecutor(streamer)
	thread.SetExecutor(executor)
	return executor, resolvedModel, nil
}

func modelOrDefault(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		if providerForModel(model) == exampleProviderAnthropic {
			return string(anthropicwrap.DefaultModel)
		}
		return openaiwrap.DefaultModel
	}
	return model
}

func runSafe(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	fn()
	return nil
}

func loadFromStore(store threads.DurableStore) (cp threads.Checkpoint, wal []threads.WALEvent, err error) {
	err = runSafe(func() {
		cp, wal = store.Load()
	})
	return cp, wal, err
}

type jsToolArgs struct {
	Code string `json:"code" jsonschema:"JavaScript source to execute in a fresh sandbox"`
}

func configureThread(thread *threads.Thread, executor *threads.ThreadExecutor, delegate threads.ThreadDelegate) {
	thread.SetExecutor(executor)
	thread.SetDelegate(delegate)
	thread.SetToolProvider(simpletool.ProviderFunc(func() threads.ToolsSnapshot {
		return threads.ToolsSnapshot{
			Snapshot: threads.ToolOfferSnapshot{Offered: []threads.ToolSpec{{
				Name:        "javascript",
				Description: "Run arbitrary JavaScript in a fresh goja sandbox and return the result.",
				Payload:     threads.ToolPayloadFor[jsToolArgs](),
			}}},
			Handlers: []threads.ToolHandlerBinding{{
				Name:            "javascript",
				HandlerLoadData: json.RawMessage(`{"runtime":"goja","mode":"fresh"}`),
			}},
		}
	}))
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, call threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
		if call.Name != "javascript" {
			return threads.ToolDispatch{}, fmt.Errorf("unknown tool %q", call.Name)
		}
		fmt.Printf("tool> %s args: %s\n", call.Name, call.Payload)
		var args jsToolArgs
		if err := call.UnmarshalJSON(&args); err != nil {
			result := jsToolResult(call.CallID, map[string]any{"error": err.Error()})
			fmt.Printf("tool> %s result data: %s\n", call.Name, mustJSON(result.Data))
			return threads.ToolDispatch{Started: true, Items: []threads.Item{result}}, nil
		}
		result := jsToolResult(call.CallID, evalJavaScript(args.Code))
		fmt.Printf("tool> %s result data: %s\n", call.Name, mustJSON(result.Data))
		return threads.ToolDispatch{Started: true, Items: []threads.Item{result}}, nil
	}))
}

func evalJavaScript(code string) map[string]any {
	vm := goja.New()
	logs := []string{}
	appendLog := func(values []goja.Value) {
		parts := make([]string, 0, len(values))
		for _, value := range values {
			parts = append(parts, value.String())
		}
		logs = append(logs, strings.Join(parts, " "))
	}

	console := vm.NewObject()
	if err := console.Set("log", func(call goja.FunctionCall) goja.Value {
		appendLog(call.Arguments)
		return goja.Undefined()
	}); err != nil {
		return map[string]any{"error": err.Error()}
	}
	if err := vm.Set("console", console); err != nil {
		return map[string]any{"error": err.Error()}
	}
	if err := vm.Set("print", func(call goja.FunctionCall) goja.Value {
		appendLog(call.Arguments)
		return goja.Undefined()
	}); err != nil {
		return map[string]any{"error": err.Error()}
	}

	value, err := vm.RunString(code)
	if err != nil {
		return map[string]any{"error": err.Error(), "logs": logs}
	}

	out := map[string]any{"logs": logs}
	switch {
	case value == nil || goja.IsUndefined(value):
		out["result"] = "undefined"
	default:
		exported := value.Export()
		if _, err := json.Marshal(exported); err == nil {
			out["result"] = exported
		} else {
			out["result"] = value.String()
		}
	}
	return out
}

func jsToolResult(callID string, payload map[string]any) threads.ToolCallResult {
	buf, err := json.Marshal(payload)
	if err != nil {
		return threads.ToolCallResult{CallID: callID, Output: err.Error(), Data: map[string]any{"error": err.Error()}}
	}
	return threads.ToolCallResult{
		CallID: callID,
		Output: string(buf),
		Data:   map[string]any{"json": payload},
	}
}

func mustJSON(v any) string {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(buf)
}
