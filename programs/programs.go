// Package programs defines typed computations that run against AgentLoom threads.
package programs

import (
	"context"
	"errors"

	"github.com/mackross/agentloom/threads"
)

// ErrBranchRequired is returned when a program needs a branch-capable thread.
var ErrBranchRequired = errors.New("programs: branch required")

// Program is a typed computation over a thread.
//
// Programs may be implemented by structs with configuration or state. Use
// ProgramFunc to adapt a function to this interface.
type Program[I, O any] interface {
	Run(ctx context.Context, t threads.Thread, input I) (O, error)
}

// ProgramFunc adapts a function to Program.
type ProgramFunc[I, O any] func(context.Context, threads.Thread, I) (O, error)

// Run calls f(ctx, t, input).
func (f ProgramFunc[I, O]) Run(ctx context.Context, t threads.Thread, input I) (O, error) {
	return f(ctx, t, input)
}

// RequireBranch returns t as a Branch, or ErrBranchRequired if t is not a Branch.
//
// Most programs should use only the threads.Thread they are given. RequireBranch
// is for programs whose semantics specifically need branch-tree operations such
// as forking alternatives, inspecting lineage, or composing durable branch
// results. Calling it makes that stronger runtime requirement explicit while
// keeping Program itself usable with any Thread.
func RequireBranch(t threads.Thread) (*threads.Branch, error) {
	b, ok := t.(*threads.Branch)
	if !ok {
		return nil, ErrBranchRequired
	}
	return b, nil
}
