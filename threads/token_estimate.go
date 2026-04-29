package threads

import (
	"context"
	"encoding/json"
	"unicode/utf8"
)

// RequestTokenEstimator estimates the number of model context tokens a request will use.
type RequestTokenEstimator interface {
	EstimateRequestTokens(ctx context.Context, req Req) (int, error)
}

// TextTokenEstimator estimates the number of model context tokens arbitrary text will use.
type TextTokenEstimator interface {
	EstimateTextTokens(ctx context.Context, text string) (int, error)
}

type tokenEstimateProvider interface {
	RequestTokenEstimator() RequestTokenEstimator
	TextTokenEstimator() TextTokenEstimator
}

// EstimateRequestTokens estimates the number of model context tokens for the current
// request snapshot. If the installed executor/streamer does not provide a
// tokenizer, a conservative approximation is returned.
func (t *Thread) EstimateRequestTokens(ctx context.Context) (int, error) {
	req := t.requestSnapshot()
	if p, ok := t.executor.(tokenEstimateProvider); ok {
		if estimator := p.RequestTokenEstimator(); estimator != nil {
			return estimator.EstimateRequestTokens(ctx, req)
		}
	}
	return EstimateRequestTokensApprox(req), nil
}

// EstimateTextTokens estimates the number of model context tokens text will use.
// If the installed executor/streamer does not provide a tokenizer, a
// conservative approximation is returned.
func (t *Thread) EstimateTextTokens(ctx context.Context, text string) (int, error) {
	if p, ok := t.executor.(tokenEstimateProvider); ok {
		if estimator := p.TextTokenEstimator(); estimator != nil {
			return estimator.EstimateTextTokens(ctx, text)
		}
	}
	return EstimateTextTokensApprox(text), nil
}

func (t *Thread) requestSnapshot() Req {
	return t.requestSnapshotWithBuilder(nil)
}

func (t *Thread) requestSnapshotWithBuilder(b RequestBuilder) Req {
	if b == nil {
		b = DefaultRequestBuilder
	}
	return cloneReq(b.Build(t.items.SliceThrough(t.cb.IP())))
}

// EstimateRequestTokensApprox returns a conservative tokenizer-free estimate for req.
func EstimateRequestTokensApprox(req Req) int {
	total := EstimateTextTokensApprox(req.Instruction)
	for _, item := range req.Items {
		total += estimateItemTokens(item)
	}
	if len(req.Tools.Offered) > 0 || len(req.Tools.Allowed) > 0 {
		if b, err := json.Marshal(req.Tools); err == nil {
			total += EstimateTextTokensApprox(string(b))
		}
	}
	if total == 0 {
		return 0
	}
	return total + 8
}

func estimateItemTokens(item Item) int {
	switch v := item.(type) {
	case UserText:
		return EstimateTextTokensApprox(string(v)) + 4
	case AssistantText:
		return EstimateTextTokensApprox(string(v)) + 4
	case ToolCall:
		return EstimateTextTokensApprox(v.Name) + EstimateTextTokensApprox(v.Payload) + 8
	case ToolCallResult:
		return EstimateTextTokensApprox(v.Output) + 8
	default:
		if b, err := json.Marshal(v); err == nil {
			return EstimateTextTokensApprox(string(b))
		}
		return 0
	}
}

// EstimateTextTokensApprox returns a conservative tokenizer-free estimate for text.
func EstimateTextTokensApprox(s string) int {
	if s == "" {
		return 0
	}
	ascii := 0
	cjk := 0
	for _, r := range s {
		if r <= 0x7f {
			ascii++
			continue
		}
		if isCJK(r) {
			cjk++
		}
	}
	runes := utf8.RuneCountInString(s)
	other := runes - ascii - cjk
	tokens := (ascii+3)/4 + cjk + (other+2)/3
	return tokens + tokens/10 + 1
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) ||
		(r >= 0x2B820 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF)
}

func cloneReq(req Req) Req {
	req.Items = append([]Item(nil), req.Items...)
	req.Tools = cloneToolOfferSnapshot(req.Tools)
	return req
}
