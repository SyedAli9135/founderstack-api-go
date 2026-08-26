package llm

import (
	"context"
	"errors"
	"testing"
)

func TestMockChatClient_ReturnsResponsesInOrder(t *testing.T) {
	client := NewMockChatClient(
		ChatResponse{ToolCalls: []ToolCall{{ID: "call_0", Name: "stripe.list_invoices"}}, StopReason: StopReasonToolUse},
		ChatResponse{Content: "done", StopReason: StopReasonEndTurn},
	)

	first, err := client.Send(context.Background(), "sys", []Message{{Role: RoleUser, Content: "go"}}, nil)
	if err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	if first.StopReason != StopReasonToolUse || len(first.ToolCalls) != 1 {
		t.Fatalf("first response = %+v, want the first canned response", first)
	}

	second, err := client.Send(context.Background(), "sys", nil, nil)
	if err != nil {
		t.Fatalf("second Send() error = %v", err)
	}
	if second.Content != "done" || second.StopReason != StopReasonEndTurn {
		t.Fatalf("second response = %+v, want the second canned response", second)
	}

	if len(client.Calls) != 2 {
		t.Fatalf("Calls recorded = %d, want 2", len(client.Calls))
	}
	if client.Calls[0].SystemPrompt != "sys" {
		t.Fatalf("Calls[0].SystemPrompt = %q, want %q", client.Calls[0].SystemPrompt, "sys")
	}
}

func TestMockChatClient_ErrorsOnceResponsesExhausted(t *testing.T) {
	client := NewMockChatClient(ChatResponse{Content: "only one"})

	if _, err := client.Send(context.Background(), "", nil, nil); err != nil {
		t.Fatalf("first Send() error = %v, want nil", err)
	}

	_, err := client.Send(context.Background(), "", nil, nil)
	if err == nil {
		t.Fatal("second Send() error = nil, want an error (responses exhausted)")
	}
	if errors.Is(err, ErrChatRejected) || errors.Is(err, ErrChatUnavailable) {
		t.Fatalf("exhaustion error should not masquerade as a real provider error: %v", err)
	}
}
