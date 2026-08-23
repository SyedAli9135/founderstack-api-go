package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

func connectSlackServer(t *testing.T) *gomcp.ClientSession {
	t.Helper()
	server := NewSlackServer()
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

func swapSlackAPIBase(url string) func() {
	original := slackAPIBase
	slackAPIBase = url
	return func() { slackAPIBase = original }
}

func TestSlack_SendMessage(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C123","ts":"1234.5678"}`))
	}))
	defer srv.Close()
	defer swapSlackAPIBase(srv.URL)()

	session := connectSlackServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{"channel": "#general", "text": "hello"},
		Meta:      mcp.WithToken("xoxb-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if gotAuth != "Bearer xoxb-test-token" {
		t.Fatalf("Authorization = %q, want Bearer xoxb-test-token", gotAuth)
	}

	var out slackSendMessageOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Channel != "C123" || out.Timestamp != "1234.5678" {
		t.Fatalf("output = %+v, want channel=C123 timestamp=1234.5678", out)
	}
}

// TestSlack_SendMessage_OKFalseIsToolError proves the ok:false handling:
// Slack returns HTTP 200 even for a failed call, so a naive "check the
// status code" implementation would report success here.
func TestSlack_SendMessage_OKFalseIsToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // Slack's real behavior on failure
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}))
	defer srv.Close()
	defer swapSlackAPIBase(srv.URL)()

	session := connectSlackServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{"channel": "#nonexistent", "text": "hello"},
		Meta:      mcp.WithToken("xoxb-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for Slack's ok:false response")
	}
}

func TestSlack_ListChannels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/conversations.list" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C1","name":"general","is_private":false}]}`))
	}))
	defer srv.Close()
	defer swapSlackAPIBase(srv.URL)()

	session := connectSlackServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "list_channels",
		Meta: mcp.WithToken("xoxb-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}

	var out slackListChannelsOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Channels) != 1 || out.Channels[0].Name != "general" {
		t.Fatalf("channels = %+v, want one 'general'", out.Channels)
	}
}
