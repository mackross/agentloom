package fileprocess

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestProcessorFuncAdapter(t *testing.T) {
	called := false
	processor := ProcessorFunc(func(ctx context.Context, req Request) (Result, error) {
		called = true
		if req.DisplayPath != "sample.txt" {
			t.Fatalf("DisplayPath = %q, want sample.txt", req.DisplayPath)
		}
		return Result{Content: []byte("next"), ContentChanged: true}, nil
	})

	res, err := processor.ProcessFile(context.Background(), Request{DisplayPath: "sample.txt", Content: []byte("old")})
	if err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	if !called {
		t.Fatal("ProcessorFunc was not called")
	}
	if string(res.Content) != "next" || !res.ContentChanged {
		t.Fatalf("unexpected result %#v", res)
	}
}

func TestPipelineOrderDisableDefaultAndFreshSlice(t *testing.T) {
	before := namedProcessor("before")
	def := namedProcessor("default")
	after := namedProcessor("after")

	got := Pipeline(Config{BeforeDefault: []Processor{before}, AfterDefault: []Processor{after}}, []Processor{def})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if !reflect.DeepEqual(processorNames(got), []string{"before", "default", "after"}) {
		t.Fatalf("unexpected order %#v", processorNames(got))
	}

	disabled := Pipeline(Config{BeforeDefault: []Processor{before}, DisableDefault: true, AfterDefault: []Processor{after}}, []Processor{def})
	if !reflect.DeepEqual(processorNames(disabled), []string{"before", "after"}) {
		t.Fatalf("unexpected disabled order %#v", processorNames(disabled))
	}

	got[0] = nil
	fresh := Pipeline(Config{BeforeDefault: []Processor{before}, AfterDefault: []Processor{after}}, []Processor{def})
	if reflect.DeepEqual(processorNames(got), processorNames(fresh)) {
		t.Fatalf("Pipeline did not return a fresh independent slice")
	}
}

func TestRunSequentialContentReportsAndNilProcessors(t *testing.T) {
	var seen []string
	processors := []Processor{
		ProcessorFunc(func(ctx context.Context, req Request) (Result, error) {
			seen = append(seen, string(req.Content))
			return Result{
				Content:        append(req.Content, '1'),
				ContentChanged: true,
				Report:         &Report{Processor: "one", Summary: "first"},
			}, nil
		}),
		nil,
		ProcessorFunc(func(ctx context.Context, req Request) (Result, error) {
			seen = append(seen, string(req.Content))
			return Result{Report: &Report{Processor: "two", Summary: "second"}}, nil
		}),
		ProcessorFunc(func(ctx context.Context, req Request) (Result, error) {
			seen = append(seen, string(req.Content))
			return Result{Content: append(req.Content, '3'), ContentChanged: true}, nil
		}),
	}

	res, reports, err := Run(context.Background(), Request{Content: []byte("0")}, processors)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Content) != "013" || !res.ContentChanged {
		t.Fatalf("unexpected result %#v", res)
	}
	if !reflect.DeepEqual(seen, []string{"0", "01", "01"}) {
		t.Fatalf("unexpected seen contents %#v", seen)
	}
	if !reflect.DeepEqual(reports, []Report{{Processor: "one", Summary: "first"}, {Processor: "two", Summary: "second"}}) {
		t.Fatalf("unexpected reports %#v", reports)
	}
}

func TestRunUnchangedAndEmptyContentChange(t *testing.T) {
	unchanged, reports, err := Run(context.Background(), Request{Content: []byte("same")}, []Processor{
		ProcessorFunc(func(context.Context, Request) (Result, error) { return Result{}, nil }),
	})
	if err != nil {
		t.Fatalf("Run unchanged: %v", err)
	}
	if unchanged.ContentChanged || string(unchanged.Content) != "same" || len(reports) != 0 {
		t.Fatalf("unexpected unchanged result %#v reports %#v", unchanged, reports)
	}

	empty, _, err := Run(context.Background(), Request{Content: []byte("not empty")}, []Processor{
		ProcessorFunc(func(context.Context, Request) (Result, error) {
			return Result{Content: nil, ContentChanged: true}, nil
		}),
	})
	if err != nil {
		t.Fatalf("Run empty: %v", err)
	}
	if !empty.ContentChanged || len(empty.Content) != 0 {
		t.Fatalf("unexpected empty result %#v", empty)
	}
}

func TestRunReportsChangedEvenWhenContentReturnsToOriginal(t *testing.T) {
	res, _, err := Run(context.Background(), Request{Content: []byte("a")}, []Processor{
		ProcessorFunc(func(context.Context, Request) (Result, error) {
			return Result{Content: []byte("b"), ContentChanged: true}, nil
		}),
		ProcessorFunc(func(context.Context, Request) (Result, error) {
			return Result{Content: []byte("a"), ContentChanged: true}, nil
		}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ContentChanged || string(res.Content) != "a" {
		t.Fatalf("unexpected result %#v", res)
	}
}

func TestRunErrorStopsAndReturnsNoPartialResult(t *testing.T) {
	boom := errors.New("boom")
	calledAfter := false
	res, reports, err := Run(context.Background(), Request{Content: []byte("original")}, []Processor{
		ProcessorFunc(func(context.Context, Request) (Result, error) {
			return Result{Content: []byte("partial"), ContentChanged: true, Report: &Report{Processor: "partial"}}, nil
		}),
		ProcessorFunc(func(context.Context, Request) (Result, error) { return Result{}, boom }),
		ProcessorFunc(func(context.Context, Request) (Result, error) {
			calledAfter = true
			return Result{}, nil
		}),
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}
	if calledAfter {
		t.Fatal("processor after error was called")
	}
	if string(res.Content) != "original" || res.ContentChanged {
		t.Fatalf("unexpected error result %#v", res)
	}
	if reports != nil {
		t.Fatalf("reports = %#v, want nil", reports)
	}
}

type namedProcessor string

func (n namedProcessor) ProcessFile(context.Context, Request) (Result, error) {
	return Result{Content: []byte(n), ContentChanged: true}, nil
}

func processorNames(processors []Processor) []string {
	names := make([]string, 0, len(processors))
	for _, processor := range processors {
		if processor == nil {
			names = append(names, "<nil>")
			continue
		}
		if p, ok := processor.(namedProcessor); ok {
			names = append(names, string(p))
			continue
		}
		names = append(names, "unknown")
	}
	return names
}
