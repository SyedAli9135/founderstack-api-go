package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

func connectDiscordServer(t *testing.T) *gomcp.ClientSession {
	t.Helper()
	server := NewDiscordServer()
	serverTransport, clientTransport := gomcp.NewInMemoryTransports()

	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestDiscord_SendMessage(t *testing.T) {
	var gotAuthHeader, gotQuery string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg123"}`))
	}))
	defer srv.Close()

	session := connectDiscordServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{"content": "hello discord"},
		Meta:      mcp.WithExtra(map[string]string{"webhook_url": srv.URL}),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if gotAuthHeader != "" {
		t.Errorf("Authorization header = %q, want empty — webhook URL is the credential", gotAuthHeader)
	}
	if gotQuery != "wait=true" {
		t.Errorf("query = %q, want wait=true", gotQuery)
	}
	if gotBody["content"] != "hello discord" {
		t.Errorf("body = %+v, want content=hello discord", gotBody)
	}

	var out discordSendMessageOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.MessageID != "msg123" {
		t.Fatalf("message_id = %q, want msg123", out.MessageID)
	}
}

func TestDiscord_SendMessage_NoWebhookIsToolError(t *testing.T) {
	session := connectDiscordServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{"content": "hello"},
		// No Meta at all — simulates a connection with no webhook Extra.
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true when there's no webhook_url")
	}
}

func TestDiscord_SendMessage_MissingContentIsToolError(t *testing.T) {
	session := connectDiscordServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{},
		Meta:      mcp.WithExtra(map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/abc"}),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for missing content")
	}
}
