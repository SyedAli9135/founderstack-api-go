package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"context"
)

// These test the request/response wire-shape logic against fake httptest
// servers, never the real Anthropic API — same reasoning as verify_test.go.

func withAnthropicTestServer(t *testing.T, handler http.HandlerFunc) *AnthropicChatClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	orig := anthropicMessagesURL
	anthropicMessagesURL = srv.URL
	t.Cleanup(func() { anthropicMessagesURL = orig })

	return NewAnthropicChatClient("sk-ant-test", "claude-sonnet-4-6")
}

func TestAnthropicChatClient_SendsSystemPromptWithCacheControl(t *testing.T) {
	var gotReq anthropicRequest
	client := withAnthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			t.Errorf("x-api-key header = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != anthropicAPIVersion {
			t.Errorf("anthropic-version header = %q", r.Header.Get("anthropic-version"))
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content:    []anthropicContentBlock{{Type: "text", Text: "hi"}},
			StopReason: "end_turn",
		})
	})

	_, err := client.Send(context.Background(), "You are a helpful COO.", []Message{{Role: RoleUser, Content: "hello"}}, nil)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if len(gotReq.System) != 1 || gotReq.System[0].Text != "You are a helpful COO." {
		t.Fatalf("System = %+v, want one block with the system prompt", gotReq.System)
	}
	if gotReq.System[0].CacheControl == nil || gotReq.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("System[0].CacheControl = %+v, want ephemeral cache_control set", gotReq.System[0].CacheControl)
	}
}

func TestAnthropicChatClient_ToolCallRoundTrip(t *testing.T) {
	var gotReq anthropicRequest
	client := withAnthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content: []anthropicContentBlock{
				{Type: "tool_use", ID: "toolu_1", Name: "stripe.list_invoices", Input: json.RawMessage(`{"status":"overdue"}`)},
			},
			StopReason: "tool_use",
			Usage:      anthropicUsage{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 50},
		})
	})

	tools := []ToolSchema{{Name: "stripe.list_invoices", Description: "list invoices", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	messages := []Message{
		{Role: RoleUser, Content: "find overdue invoices"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "toolu_0", Name: "stripe.list_customers", Args: json.RawMessage(`{}`)}}},
		{Role: RoleTool, ToolCallID: "toolu_0", Content: `{"customers":[]}`},
	}

	resp, err := client.Send(context.Background(), "", messages, tools)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	// Request shape: the outbound tool_use block and the tool_result block
	// both made it onto the wire correctly.
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Name != "stripe.list_invoices" {
		t.Fatalf("Tools = %+v", gotReq.Tools)
	}
	if len(gotReq.Messages) != 3 {
		t.Fatalf("Messages = %d, want 3", len(gotReq.Messages))
	}
	toolResultMsg := gotReq.Messages[2]
	if toolResultMsg.Role != "user" || toolResultMsg.Content[0].Type != "tool_result" || toolResultMsg.Content[0].ToolUseID != "toolu_0" {
		t.Fatalf("tool result message = %+v, want a user-role tool_result block", toolResultMsg)
	}

	// Response shape: the tool call came back correctly, and StopReason
	// normalized to StopReasonToolUse.
	if resp.StopReason != StopReasonToolUse {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, StopReasonToolUse)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "stripe.list_invoices" {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	if resp.Usage.CachedTokens != 50 {
		t.Fatalf("Usage.CachedTokens = %d, want 50", resp.Usage.CachedTokens)
	}
}

func TestAnthropicChatClient_ErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrChatRejected},
		{http.StatusBadRequest, ErrChatRejected},
		{http.StatusTooManyRequests, ErrChatUnavailable},
		{http.StatusInternalServerError, ErrChatUnavailable},
	}
	for _, c := range cases {
		client := withAnthropicTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		})
		_, err := client.Send(context.Background(), "", []Message{{Role: RoleUser, Content: "hi"}}, nil)
		if !errors.Is(err, c.want) {
			t.Errorf("status %d: err = %v, want %v", c.status, err, c.want)
		}
	}
}
