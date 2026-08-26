package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withGeminiTestServer(t *testing.T, handler http.HandlerFunc) *GeminiChatClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	orig := geminiGenerateContentBaseURL
	geminiGenerateContentBaseURL = srv.URL
	t.Cleanup(func() { geminiGenerateContentBaseURL = orig })

	return NewGeminiChatClient("AIza-test", "gemini-2.5-pro")
}

func TestGeminiChatClient_SendsKeyAsQueryParamAndSystemInstruction(t *testing.T) {
	var gotReq geminiRequest
	var gotQuery string
	client := withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content:      geminiContent{Role: "model", Parts: []geminiPart{{Text: "hi"}}},
				FinishReason: "STOP",
			}},
		})
	})

	_, err := client.Send(context.Background(), "You are a helpful COO.", []Message{{Role: RoleUser, Content: "hello"}}, nil)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if gotQuery != "key=AIza-test" {
		t.Fatalf("query = %q, want key=AIza-test", gotQuery)
	}
	if gotReq.SystemInstruction == nil || gotReq.SystemInstruction.Parts[0].Text != "You are a helpful COO." {
		t.Fatalf("SystemInstruction = %+v", gotReq.SystemInstruction)
	}
	if len(gotReq.Contents) != 1 || gotReq.Contents[0].Role != "user" {
		t.Fatalf("Contents = %+v", gotReq.Contents)
	}
}

func TestGeminiChatClient_ToolCallRoundTripMatchesByName(t *testing.T) {
	var gotReq geminiRequest
	client := withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content: geminiContent{Role: "model", Parts: []geminiPart{
					{FunctionCall: &geminiFunctionCall{Name: "notion.read_page", Args: json.RawMessage(`{"page_id":"abc"}`)}},
				}},
				FinishReason: "STOP",
			}},
			UsageMetadata: geminiUsageMetadata{PromptTokenCount: 30, CandidatesTokenCount: 5},
		})
	})

	tools := []ToolSchema{{Name: "notion.read_page", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	// Gemini has no per-call ID — the tool result is matched back by
	// Name, not ToolCallID (ToolCallID is still set, for symmetry with
	// the other providers, but this adapter must ignore it).
	messages := []Message{
		{Role: RoleUser, Content: "read the doc"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_0", Name: "notion.search", Args: json.RawMessage(`{}`)}}},
		{Role: RoleTool, Name: "notion.search", ToolCallID: "call_0", Content: `{"results":[]}`},
	}

	resp, err := client.Send(context.Background(), "", messages, tools)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	// Request shape: functionDeclarations present, tool result content
	// carries the function name (not an id).
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].FunctionDeclarations[0].Name != "notion.read_page" {
		t.Fatalf("Tools = %+v", gotReq.Tools)
	}
	toolResultContent := gotReq.Contents[2]
	if toolResultContent.Role != "user" || toolResultContent.Parts[0].FunctionResp == nil ||
		toolResultContent.Parts[0].FunctionResp.Name != "notion.search" {
		t.Fatalf("tool result content = %+v, want a functionResponse part named notion.search", toolResultContent)
	}

	// Response shape: a synthesized call ID was assigned since Gemini's
	// wire format has none, and StopReason correctly resolves to
	// StopReasonToolUse (not just "STOP") because a function call is present.
	if resp.StopReason != StopReasonToolUse {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, StopReasonToolUse)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "notion.read_page" || resp.ToolCalls[0].ID == "" {
		t.Fatalf("ToolCalls = %+v, want one call with a non-empty synthesized ID", resp.ToolCalls)
	}
	if resp.Usage.InputTokens != 30 {
		t.Fatalf("Usage.InputTokens = %d, want 30", resp.Usage.InputTokens)
	}
}

func TestGeminiChatClient_StopWithNoToolCallsIsEndTurn(t *testing.T) {
	client := withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content:      geminiContent{Role: "model", Parts: []geminiPart{{Text: "done"}}},
				FinishReason: "STOP",
			}},
		})
	})

	resp, err := client.Send(context.Background(), "", []Message{{Role: RoleUser, Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if resp.StopReason != StopReasonEndTurn {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, StopReasonEndTurn)
	}
}

func TestGeminiChatClient_ErrorClassification(t *testing.T) {
	// Google returns 400, not 401/403, for a bad key — verify.go's
	// verifyGemini already documents this quirk; classifyChatError must
	// still land it on ErrChatRejected (terminal), not ErrChatUnavailable.
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusBadRequest, ErrChatRejected},
		{http.StatusTooManyRequests, ErrChatUnavailable},
	}
	for _, c := range cases {
		client := withGeminiTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
		})
		_, err := client.Send(context.Background(), "", []Message{{Role: RoleUser, Content: "hi"}}, nil)
		if !errors.Is(err, c.want) {
			t.Errorf("status %d: err = %v, want %v", c.status, err, c.want)
		}
	}
}
