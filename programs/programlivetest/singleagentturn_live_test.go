//go:build programlive

// Package programlivetest contains opt-in live tests for AgentLoom programs.
package programlivetest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mackross/agentloom/llms/openai"
	"github.com/mackross/agentloom/programs"
	"github.com/mackross/agentloom/programs/adapters"
	"github.com/mackross/agentloom/threads"
	threadtool "github.com/mackross/agentloom/threads/tool"
	"github.com/openai/openai-go/v3/shared"
)

const (
	lunaModel             = "gpt-5.6-luna"
	liveOrderID           = "ORD-LIVE-2048"
	liveOrderStatus       = "delayed"
	liveEstimatedDelivery = "2026-04-03"
)

type deliveryQuestion struct {
	OrderID         string `json:"order_id" jsonschema:"customer order identifier"`
	CustomerMessage string `json:"customer_message" jsonschema:"customer's delivery question"`
}

type supportResolution struct {
	OrderID           string `json:"order_id" jsonschema:"order identifier copied from lookup_order"`
	OrderStatus       string `json:"order_status" jsonschema:"order status copied from lookup_order"`
	EstimatedDelivery string `json:"estimated_delivery" jsonschema:"estimated delivery copied exactly from lookup_order"`
	Escalate          bool   `json:"escalate" jsonschema:"whether the lookup result requires human escalation"`
}

type lookupOrderArgs struct {
	OrderID string `json:"order_id" jsonschema:"order identifier to look up"`
}

type lookupOrderResult struct {
	OrderID            string `json:"order_id"`
	Status             string `json:"status"`
	LastScan           string `json:"last_scan"`
	EstimatedDelivery  string `json:"estimated_delivery"`
	EscalationRequired bool   `json:"escalation_required"`
}

func TestSingleAgentTurnGPT56LunaStructuredDeliveryResolution(t *testing.T) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
		t.Fatal("OPENAI_API_KEY is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	branch := newLiveBranch(t, ctx)
	streamer := openai.NewResponsesStreamer(lunaModel)
	streamer.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortLow}
	t.Cleanup(func() {
		if err := streamer.Close(); err != nil {
			t.Errorf("close OpenAI streamer: %v", err)
		}
	})
	branch.SetExecutor(threads.NewThreadExecutor(streamer))

	var lookupCalls atomic.Int32
	lookupSpec, lookupHandler := threadtool.JSON[lookupOrderArgs](
		"lookup_order",
		"Look up the current fulfillment and delivery state for an order.",
		func(_ context.Context, _ threads.Thread, call threadtool.Call, args lookupOrderArgs) threadtool.Item {
			lookupCalls.Add(1)
			if args.OrderID != liveOrderID {
				return threadtool.ResultError(call, fmt.Errorf("unknown order %q", args.OrderID))
			}
			return threadtool.ResultJSON(call, lookupOrderResult{
				OrderID:            liveOrderID,
				Status:             liveOrderStatus,
				LastScan:           "Sydney distribution centre",
				EstimatedDelivery:  liveEstimatedDelivery,
				EscalationRequired: false,
			})
		},
	)

	agent := adapters.SingleAgentTurn[deliveryQuestion, supportResolution]{
		Signature: programs.Signature[deliveryQuestion, supportResolution]{
			Name: "resolve_delivery_question",
			Instruction: strings.Join([]string{
				"Act as a delivery-support agent.",
				"You must call lookup_order for the supplied order_id before completing.",
				"Build the structured output only from that tool result.",
				"Copy order_id, status, and estimated_delivery exactly.",
				"Set escalate to the value of escalation_required.",
				"Do not invent or reformat shipment details.",
			}, " "),
		},
		Tools: threadtool.NewCatalog().Add(lookupSpec, lookupHandler),
	}

	out, err := agent.Run(ctx, branch, deliveryQuestion{
		OrderID:         liveOrderID,
		CustomerMessage: "My delivery is late. What is its current status?",
	})
	if err != nil {
		t.Fatalf("SingleAgentTurn.Run with %s: %v", lunaModel, err)
	}
	if calls := lookupCalls.Load(); calls != 1 {
		t.Fatalf("lookup_order calls = %d, want 1", calls)
	}
	if out.OrderID != liveOrderID {
		t.Errorf("OrderID = %q, want %q", out.OrderID, liveOrderID)
	}
	if out.OrderStatus != liveOrderStatus {
		t.Errorf("OrderStatus = %q, want %q", out.OrderStatus, liveOrderStatus)
	}
	if out.EstimatedDelivery != liveEstimatedDelivery {
		t.Errorf("EstimatedDelivery = %q, want %q", out.EstimatedDelivery, liveEstimatedDelivery)
	}
	if out.Escalate {
		t.Error("Escalate = true, want false")
	}
}

func newLiveBranch(t *testing.T, ctx context.Context) *threads.Branch {
	t.Helper()
	store := threads.NewMemoryBranchStore()
	stored, err := store.CreateBranch(ctx, threads.BranchCreateOptions{ID: "program-live-single-agent-turn"})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if err := stored.Close(); err != nil {
		t.Fatalf("close stored branch: %v", err)
	}
	branch, err := threads.NewDefaultBranchManager(store, "programlivetest").Open(ctx, "/branch/program-live-single-agent-turn")
	if err != nil {
		t.Fatalf("open branch: %v", err)
	}
	t.Cleanup(func() {
		if err := branch.Close(); err != nil {
			t.Errorf("close branch: %v", err)
		}
	})
	return branch
}
