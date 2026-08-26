package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withOpenAICompatibleTestServer(t *testing.T, handler http.HandlerFunc) *OpenAICompatibleChatClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewOpenAICompatibleChatClient(srv.URL, "sk-test", "gpt-5")
}

func TestOpenAICompatibleChatClient_SendsSystemPromptAsFirstMessage(t *testing.T) {
	var gotReq openAIRequest
	var gotAuth string
	client := withOpenAICompatibleTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{{Message: openAIMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop"}},
		})
	})

	_, err := client.Send(context.Background(), "You are a helpful COO.", []Message{{Role: RoleUser, Content: "hello"}}, nil)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" || gotReq.Messages[0].Content != "You are a helpful COO." {
		t.Fatalf("Messages = %+v, want system message first", gotReq.Messages)
	}
}

func TestOpenAICompatibleChatClient_ToolCallRoundTrip(t *testing.T) {
	var gotReq openAIRequest
	client := withOpenAICompatibleTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIResponse{
			Choices: []openAIChoice{{
				Message: openAIMessage{
					Role: "assistant",
					ToolCalls: []openAIToolCall{{
						ID: "call_1", Type: "function",
						Function: openAIToolCallFunc{Name: "slack.send_message", Arguments: `{"channel":"#general","text":"hi"}`},
					}},
				},
				FinishReason: "tool_calls",
			}},
			Usage: openAIUsage{PromptTokens: 40, CompletionTokens: 10},
		})
	})

	tools := []ToolSchema{{Name: "slack.send_message", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	messages := []Message{
		{Role: RoleUser, Content: "notify the team"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_0", Name: "slack.list_channels", Args: json.RawMessage(`{}`)}}},
		{Role: RoleTool, ToolCallID: "call_0", Content: `{"channels":["#general"]}`},
	}

	resp, err := client.Send(context.Background(), "", messages, tools)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	// Request shape.
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "slack.send_message" {
		t.Fatalf("Tools = %+v", gotReq.Tools)
	}
	toolMsg := gotReq.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_0" {
		t.Fatalf("tool result message = %+v, want role=tool tool_call_id=call_0", toolMsg)
	}

	// Response shape: arguments string got re-parsed into json.RawMessage.
	if resp.StopReason != StopReasonToolUse {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, StopReasonToolUse)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "slack.send_message" {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	var args map[string]string
	if err := json.Unmarshal(resp.ToolCalls[0].Args, &args); err != nil || args["channel"] != "#general" {
		t.Fatalf("ToolCalls[0].Args = %s, want valid JSON with channel=#general", resp.ToolCalls[0].Args)
	}
}

func TestOpenAICompatibleChatClient_ErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrChatRejected},
		{http.StatusTooManyRequests, ErrChatUnavailable},
		{http.StatusServiceUnavailable, ErrChatUnavailable},
	}
	for _, c := range cases {
		client := withOpenAICompatibleTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
		})
		_, err := client.Send(context.Background(), "", []Message{{Role: RoleUser, Content: "hi"}}, nil)
		if !errors.Is(err, c.want) {
			t.Errorf("status %d: err = %v, want %v", c.status, err, c.want)
		}
	}
}
