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

func connectLinkedInServer(t *testing.T) *gomcp.ClientSession {
	t.Helper()
	server := NewLinkedInServer()
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

func swapLinkedInAPIBase(url string) func() {
	original := linkedinAPIBase
	linkedinAPIBase = url
	return func() { linkedinAPIBase = original }
}

func TestLinkedIn_DraftPost(t *testing.T) {
	var gotVersion, gotRestli string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("LinkedIn-Version")
		gotRestli = r.Header.Get("X-Restli-Protocol-Version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("x-restli-id", "urn:li:share:12345")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	defer swapLinkedInAPIBase(srv.URL)()

	session := connectLinkedInServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "draft_post",
		Arguments: map[string]any{"author_urn": "urn:li:person:abc123", "text": "Hello world"},
		Meta:      mcp.WithToken("linkedin_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if gotVersion != linkedinAPIVersion {
		t.Errorf("LinkedIn-Version = %q, want %q", gotVersion, linkedinAPIVersion)
	}
	if gotRestli != "2.0.0" {
		t.Errorf("X-Restli-Protocol-Version = %q, want 2.0.0", gotRestli)
	}
	if gotBody["author"] != "urn:li:person:abc123" || gotBody["commentary"] != "Hello world" {
		t.Fatalf("request body = %+v, want author/commentary set", gotBody)
	}

	var out linkedinDraftPostOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.PostID != "urn:li:share:12345" {
		t.Fatalf("post_id = %q, want urn:li:share:12345", out.PostID)
	}
}

func TestLinkedIn_DraftPost_MissingFieldsIsToolError(t *testing.T) {
	session := connectLinkedInServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "draft_post",
		Arguments: map[string]any{"text": "no author set"},
		Meta:      mcp.WithToken("linkedin_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for missing author_urn")
	}
}

func TestLinkedIn_DraftPost_APIErrorSurfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"insufficient scope"}`))
	}))
	defer srv.Close()
	defer swapLinkedInAPIBase(srv.URL)()

	session := connectLinkedInServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "draft_post",
		Arguments: map[string]any{"author_urn": "urn:li:person:abc123", "text": "Hello"},
		Meta:      mcp.WithToken("linkedin_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for a 403 from LinkedIn")
	}
}

func TestLinkedIn_DraftPost_RetriesOnExplicitServerErrorThenSucceeds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("x-restli-id", "urn:li:share:retry-ok")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	defer swapLinkedInAPIBase(srv.URL)()

	session := connectLinkedInServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "draft_post",
		Arguments: map[string]any{"author_urn": "urn:li:person:abc123", "text": "Hello"},
		Meta:      mcp.WithToken("linkedin_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, want the retry to have recovered: %+v", result.Content)
	}
	if calls != 2 {
		t.Fatalf("server received %d requests, want 2 (1 retry after the 503)", calls)
	}
}

func TestLinkedIn_DraftPost_DoesNotRetryTerminalStatus(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	defer swapLinkedInAPIBase(srv.URL)()

	session := connectLinkedInServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "draft_post",
		Arguments: map[string]any{"author_urn": "urn:li:person:abc123", "text": "Hello"},
		Meta:      mcp.WithToken("linkedin_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for a 403")
	}
	if calls != 1 {
		t.Fatalf("server received %d requests, want exactly 1 (a 403 must not retry)", calls)
	}
}
