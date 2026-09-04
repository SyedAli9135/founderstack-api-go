package llm

import (
	"context"
	"fmt"
)

// MockChatCall records one Send invocation's arguments — lets a test
// assert what the engine actually sent, not just what it got back.
type MockChatCall struct {
	SystemPrompt string
	Messages     []Message
	Tools        []ToolSchema
}

// MockChatClient returns a pre-programmed sequence of responses, one per
// Send call — no network, no live provider. The default test harness for
// internal/core/graph: exercises checkpointing, guardrails, approval gate,
// and SSE events deterministically.
type MockChatClient struct {
	Responses []ChatResponse
	Calls     []MockChatCall

	calls int
}

// NewMockChatClient builds a MockChatClient that returns responses in
// order, one per Send call.
func NewMockChatClient(responses ...ChatResponse) *MockChatClient {
	return &MockChatClient{Responses: responses}
}

// Send implements ChatClient.
func (m *MockChatClient) Send(ctx context.Context, systemPrompt string, messages []Message, tools []ToolSchema) (ChatResponse, error) {
	m.Calls = append(m.Calls, MockChatCall{SystemPrompt: systemPrompt, Messages: messages, Tools: tools})

	if m.calls >= len(m.Responses) {
		return ChatResponse{}, fmt.Errorf("llm: MockChatClient exhausted its %d canned response(s) on call %d", len(m.Responses), m.calls+1)
	}
	resp := m.Responses[m.calls]
	m.calls++
	return resp, nil
}
