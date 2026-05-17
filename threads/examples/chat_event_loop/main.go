package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/dop251/goja"

	anthropicwrap "github.com/mackross/agentloom/llms/anthropic"
	fireworkswrap "github.com/mackross/agentloom/llms/fireworks"
	openaiwrap "github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/simpletool"
)

func main() {
	model := configuredModel()
	if !hasProviderAPIKey(model) {
		fmt.Fprintf(os.Stderr, "set %s\n", requiredAPIKeyLabel(model))
		os.Exit(1)
	}

	ui := newTerminalUI(os.Stdin)
	defer ui.Close()

	streamer, currentModel := newStreamerForModel(model)
	executor := threads.NewThreadExecutor(streamer)
	canceler := &streamCanceler{executor: executor}
	ui.SetCancel(func() {
		if canceler.Cancel() {
			ui.PrintCanceled()
		}
	})
	delegate := threads.ThreadDelegateFuncs{
		OnRequest: func(_ *threads.Thread) {
			ui.AssistantStart()
		},
		OnStreamItemAppended: func(_ *threads.Thread, item threads.Item) {
			if text, ok := item.(threads.AssistantText); ok {
				ui.Print(string(text))
			}
		},
		OnIdle: func(_ *threads.Thread) {
			ui.AssistantDone()
		},
	}

	loop := threads.NewEventLoop(threads.New())
	runCtx, cancelRun := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(runCtx) }()
	defer func() {
		_ = loop.Close()
		cancelRun()
		if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "event loop error:", err)
		}
	}()

	if err := loop.Do(context.Background(), func(thread *threads.Thread) error {
		configureThread(thread, executor, delegate)
		return nil
	}); err != nil {
		fmt.Fprintln(os.Stderr, "event loop setup error:", err)
		os.Exit(1)
	}

	fmt.Printf("model: %s\n", currentModel)
	fmt.Println("type your message and press enter")
	fmt.Println("commands: /instruction <text>, /model <name>, Esc to cancel stream, /exit, /quit")
	fmt.Println("tool: javascript(code) runs arbitrary JavaScript in a fresh goja sandbox")
	fmt.Println("note: you can type the next message while the assistant is streaming")
	fmt.Println("note: all thread access in this example is serialized through threads.EventLoop.Do")

	input := ui.ReadInputLines()
	ui.Prompt()
	for entry := range input {
		if entry.Err != nil {
			ui.Errorf("stdin error: %v\n", entry.Err)
			os.Exit(1)
		}
		line := strings.TrimSpace(entry.Text)
		if line == "" {
			ui.Prompt()
			continue
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		if strings.HasPrefix(line, "/instruction") {
			instruction := strings.TrimSpace(strings.TrimPrefix(line, "/instruction"))
			if instruction == "" {
				ui.Println("usage: /instruction <text>")
				ui.Prompt()
				continue
			}
			if err := loop.Do(context.Background(), func(thread *threads.Thread) error {
				thread.QueueItem(threads.AssistantInstruction(instruction))
				return nil
			}); err != nil {
				ui.Println("instruction error:", err)
				ui.Prompt()
				continue
			}
			ui.Println("instruction updated")
			ui.Prompt()
			continue
		}
		if strings.HasPrefix(line, "/model") {
			nextModel := strings.TrimSpace(strings.TrimPrefix(line, "/model"))
			if nextModel == "" {
				ui.Println("current model:", currentModel)
				ui.Prompt()
				continue
			}
			var resolvedModel string
			if err := loop.Do(context.Background(), func(thread *threads.Thread) error {
				nextExecutor, resolved, err := switchModelIfIdle(thread, nextModel)
				if err != nil {
					return err
				}
				canceler.Set(nextExecutor)
				resolvedModel = resolved
				return nil
			}); err != nil {
				ui.Println("model switch error:", err)
				ui.Prompt()
				continue
			}
			currentModel = resolvedModel
			ui.Println("model switched:", currentModel)
			ui.Prompt()
			continue
		}

		if err := loop.Do(context.Background(), func(thread *threads.Thread) error {
			thread.QueueItem(threads.UserText(line))
			thread.QueueItem(threads.SendItem{})
			return nil
		}); err != nil {
			ui.Println("send error:", err)
			ui.Prompt()
		}
	}
}

type inputLine struct {
	Text string
	Err  error
}

type terminalUI struct {
	file        *os.File
	interactive bool
	original    string
	mu          sync.Mutex
	streaming   bool
	buf         []rune
	hiddenLines []string
	cancel      func()
}

type streamCanceler struct {
	mu       sync.Mutex
	executor *threads.ThreadExecutor
}

func (c *streamCanceler) Set(executor *threads.ThreadExecutor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.executor = executor
}

func (c *streamCanceler) Cancel() bool {
	c.mu.Lock()
	executor := c.executor
	c.mu.Unlock()
	return executor != nil && executor.CancelCurrentStream()
}

func newTerminalUI(file *os.File) *terminalUI {
	info, err := file.Stat()
	ui := &terminalUI{file: file, interactive: err == nil && info.Mode()&os.ModeCharDevice != 0}
	if !ui.interactive {
		return ui
	}
	if original, err := ui.sttyOutput("-g"); err == nil {
		ui.original = strings.TrimSpace(original)
		_ = ui.stty("-icanon", "min", "1", "time", "0", "-echo")
	}
	return ui
}

func (ui *terminalUI) Close() {
	if ui == nil || !ui.interactive || ui.original == "" {
		return
	}
	_ = ui.stty(ui.original)
}

func (ui *terminalUI) SetCancel(cancel func()) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	ui.cancel = cancel
}

func (ui *terminalUI) Prompt() {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	fmt.Print("you> ")
}

func (ui *terminalUI) AssistantStart() {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	ui.streaming = true
	fmt.Print("ai> ")
}

func (ui *terminalUI) AssistantDone() {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	ui.streaming = false
	fmt.Println()
	for _, line := range ui.hiddenLines {
		fmt.Printf("you> %s\n", line)
	}
	if len(ui.hiddenLines) > 0 {
		ui.hiddenLines = nil
		return
	}
	ui.hiddenLines = nil
	fmt.Print("you> ")
	if len(ui.buf) > 0 {
		fmt.Print(string(ui.buf))
	}
}

func (ui *terminalUI) Print(s string) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	fmt.Print(s)
}

func (ui *terminalUI) Println(v ...any) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	fmt.Println(v...)
}

func (ui *terminalUI) PrintCanceled() {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.streaming {
		fmt.Print("\n[canceling stream]\n")
	}
}

func (ui *terminalUI) Errorf(format string, v ...any) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	fmt.Fprintf(os.Stderr, format, v...)
}

func (ui *terminalUI) ReadInputLines() <-chan inputLine {
	out := make(chan inputLine, 16)
	go func() {
		defer close(out)
		if !ui.interactive {
			scanner := bufio.NewScanner(ui.file)
			for scanner.Scan() {
				out <- inputLine{Text: scanner.Text()}
			}
			if err := scanner.Err(); err != nil {
				out <- inputLine{Err: err}
			}
			return
		}

		reader := bufio.NewReader(ui.file)
		for {
			r, _, err := reader.ReadRune()
			if err != nil {
				out <- inputLine{Err: err}
				return
			}
			switch r {
			case '\r', '\n':
				line, hidden := ui.submitInputBuffer()
				if !hidden {
					ui.Println()
				}
				out <- inputLine{Text: line}
			case 27:
				ui.cancelStream()
			case 4:
				return
			case 8, 127:
				ui.backspace()
			default:
				if r >= 32 || r == '\t' {
					ui.appendRune(r)
				}
			}
		}
	}()
	return out
}

func (ui *terminalUI) cancelStream() {
	ui.mu.Lock()
	cancel := ui.cancel
	ui.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (ui *terminalUI) submitInputBuffer() (string, bool) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	line := string(ui.buf)
	ui.buf = nil
	if ui.streaming {
		ui.hiddenLines = append(ui.hiddenLines, line)
		return line, true
	}
	return line, false
}

func (ui *terminalUI) appendRune(r rune) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	ui.buf = append(ui.buf, r)
	if !ui.streaming {
		fmt.Print(string(r))
	}
}

func (ui *terminalUI) backspace() {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if len(ui.buf) == 0 {
		return
	}
	ui.buf = ui.buf[:len(ui.buf)-1]
	if !ui.streaming {
		fmt.Print("\b \b")
	}
}

func (ui *terminalUI) stty(args ...string) error {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = ui.file
	return cmd.Run()
}

func (ui *terminalUI) sttyOutput(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = ui.file
	out, err := cmd.Output()
	return string(out), err
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

type jsToolArgs struct {
	Code string `json:"code" jsonschema:"JavaScript source to execute in a fresh sandbox"`
}

func configureThread(thread *threads.Thread, executor *threads.ThreadExecutor, delegate threads.ThreadDelegate) {
	thread.SetExecutor(executor)
	thread.SetDelegate(delegate)
	thread.SetToolProvider(simpletool.ProviderFunc(func(_ *threads.Thread) threads.ToolsSnapshot {
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
	thread.SetToolResolver(simpletool.ResolverFunc(func(_ context.Context, _ *threads.Thread, call threads.ToolCall, _ json.RawMessage) (threads.ToolDispatch, error) {
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
