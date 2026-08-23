package mcp

import (
	"context"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoExtraServer reports whatever Extra map it received via _meta —
// proves ExtraFromRequest correctly converts the map[string]any shape a
// real (JSON-serializing) MCP round trip produces, not just the
// map[string]string shape a same-process call would.
func echoExtraServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "echo-extra", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "echo_extra"}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, map[string]string, error) {
		extra, _ := ExtraFromRequest(req)
		return nil, extra, nil
	})
	return server
}

func TestWithExtra_RoundTripsThroughRealMCPSession(t *testing.T) {
	server := echoExtraServer()
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

	meta := WithToken("some-token")
	extra := map[string]string{"webhook_url": "https://discord.com/api/webhooks/1/abc"}
	for k, v := range WithExtra(extra) {
		meta[k] = v
	}

	result, err := session.CallTool(ctx, &gomcp.CallToolParams{Name: "echo_extra", Meta: meta})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}

	got, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %+v (%T), want map[string]any", result.StructuredContent, result.StructuredContent)
	}
	if got["webhook_url"] != extra["webhook_url"] {
		t.Fatalf("webhook_url = %v, want %v", got["webhook_url"], extra["webhook_url"])
	}
}

func TestExtraFromRequest_MissingIsFalse(t *testing.T) {
	if _, ok := ExtraFromRequest(nil); ok {
		t.Fatal("ExtraFromRequest(nil) ok = true, want false")
	}
	req := &gomcp.CallToolRequest{Params: &gomcp.CallToolParamsRaw{}}
	if _, ok := ExtraFromRequest(req); ok {
		t.Fatal("ExtraFromRequest with no _meta ok = true, want false")
	}
}
