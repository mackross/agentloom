// Package bashtool provides an AgentLoom-compatible bash tool.
package bashtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/mackross/agentloom/threads"
	"github.com/mackross/agentloom/threads/tools/beta/internal/loomutil"
	"github.com/mackross/agentloom/threads/tools/beta/internal/pathutil"
	"github.com/mackross/agentloom/threads/tools/beta/internal/truncate"
)

const name = "bash"

// Config is the durable bash tool configuration.
type Config struct {
	CWD        string         `json:"cwd,omitempty"`
	Shell      string         `json:"shell,omitempty"`
	MaxLines   int            `json:"maxLines,omitempty"`
	MaxBytes   int            `json:"maxBytes,omitempty"`
	Async      bool           `json:"async,omitempty"`
	LastResort bool           `json:"lastResort,omitempty"`
	Thread     threads.Thread `json:"-"`
}

type args struct {
	Command string `json:"command" jsonschema:"Bash command to execute"`
	Timeout int    `json:"timeout" jsonschema:"Timeout (sec). 0 = no timeout."`
}

// Tool implements threads.ToolProvider and threads.ToolResolver.
type Tool struct {
	cfg Config
}

// New creates a bash tool.
func New(cfg Config) *Tool { return &Tool{cfg: cfg} }

// ToolsSnapshot returns the durable AgentLoom tool snapshot.
func (t *Tool) ToolsSnapshot(_ threads.Thread) threads.ToolsSnapshot {
	desc := "Execute a bash command in CWD."
	if t.cfg.LastResort {
		desc = "Execute a bash command in the current working directory. Use only as a last resort; prefer go* tools for Go discovery, diagnostics, tests, and refactors."
	}
	return loomutil.Snapshot(name, desc, threads.ToolPayloadFor[args](), t.cfg)
}

// ResolveTool executes a bash call using durable handler load data.
func (t *Tool) ResolveTool(ctx context.Context, thread threads.Thread, call threads.ToolCall, load json.RawMessage) (threads.ToolDispatch, error) {
	var cfg Config
	if err := loomutil.DecodeLoad(load, &cfg); err != nil {
		return threads.ToolDispatch{}, err
	}
	cfg.Thread = t.cfg.Thread
	a, err := decodePayload(call)
	if err != nil {
		return result(call, false, loomutil.Error(call, err)), nil
	}
	if cfg.Async {
		return loomutil.AsyncResult(ctx, cfg.Thread, call, threads.ToolRecoveryUnsafe, func(ctx context.Context) threads.ToolCallResult {
			return run(ctx, cfg, a)
		})
	}
	res := run(ctx, cfg, a)
	return result(call, true, res), nil
}
func decodePayload(call threads.ToolCall) (args, error) {
	var a args
	if err := loomutil.DecodePayload(call, &a); err != nil {
		return a, err
	}
	if a.Command == "" {
		return a, fmt.Errorf("command is required")
	}
	return a, nil
}

func run(ctx context.Context, cfg Config, a args) threads.ToolCallResult {
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
		defer cancel()
	}
	shell := cfg.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", a.Command)
	cmd.Dir = pathutil.Resolve(cfg.CWD, ".")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Start()
	if err == nil {
		err = wait(ctx, cmd)
	}
	text := formatOutput(out.String(), cfg)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("command timeout or canceled: %w", ctx.Err())
		} else if exit, ok := err.(*exec.ExitError); ok {
			err = fmt.Errorf("command exited with exit code %d", exit.ExitCode())
		}
		if text != "" {
			text += "\n\n"
		}
		return threads.ToolCallResult{Output: text + err.Error(), Data: map[string]any{"error": err.Error()}}
	}
	if text == "" {
		text = "(no output)"
	}
	return threads.ToolCallResult{Output: text}
}

func wait(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

func formatOutput(s string, cfg Config) string {
	r := truncate.Tail(s, cfg.MaxLines, cfg.MaxBytes)
	if r.Truncated {
		return r.Content + r.Notice
	}
	return r.Content
}

func result(call threads.ToolCall, started bool, item threads.ToolCallResult) threads.ToolDispatch {
	item.CallID = call.CallID
	d := threads.ToolDispatch{Started: started, Items: []threads.Item{item}}
	if started {
		d.Recovery = threads.ToolRecoveryUnsafe
	}
	return d
}
