package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

func connectGitHubServer(t *testing.T) *gomcp.ClientSession {
	t.Helper()
	server := NewGitHubServer()
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

func swapGitHubAPIBase(url string) func() {
	original := githubAPIBase
	githubAPIBase = url
	return func() { githubAPIBase = original }
}

func TestGitHub_ReviewPR(t *testing.T) {
	var gotAcceptHeader, gotAPIVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptHeader = r.Header.Get("Accept")
		gotAPIVersion = r.Header.Get("X-GitHub-Api-Version")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/42":
			_, _ = w.Write([]byte(`{"title":"Fix bug","body":"Fixes #1","state":"open"}`))
		case "/repos/acme/widgets/pulls/42/files":
			_, _ = w.Write([]byte(`[{"filename":"main.go","status":"modified","additions":3,"deletions":1,"patch":"@@ -1,1 +1,3 @@"}]`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	defer swapGitHubAPIBase(srv.URL)()

	session := connectGitHubServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "review_pr",
		Arguments: map[string]any{"owner": "acme", "repo": "widgets", "number": 42},
		Meta:      mcp.WithToken("ghp_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if gotAcceptHeader != "application/vnd.github+json" {
		t.Errorf("Accept header = %q, want application/vnd.github+json", gotAcceptHeader)
	}
	if gotAPIVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q, want 2022-11-28", gotAPIVersion)
	}

	var out githubReviewPROutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Title != "Fix bug" || len(out.Files) != 1 || out.Files[0].Filename != "main.go" {
		t.Fatalf("output = %+v, want title=Fix bug with one file main.go", out)
	}
}

func TestGitHub_CreateIssue_MissingFieldsIsToolError(t *testing.T) {
	session := connectGitHubServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "create_issue",
		Arguments: map[string]any{"owner": "acme"},
		Meta:      mcp.WithToken("ghp_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for missing repo/title")
	}
}

func TestGitHub_SearchCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "handleWebhook org:acme" {
			t.Errorf("query = %q, want %q", r.URL.Query().Get("q"), "handleWebhook org:acme")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":1,"items":[{"path":"main.go","repository":{"full_name":"acme/widgets"},"html_url":"https://github.com/acme/widgets/blob/main/main.go"}]}`))
	}))
	defer srv.Close()
	defer swapGitHubAPIBase(srv.URL)()

	session := connectGitHubServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "search_code",
		Arguments: map[string]any{"query": "handleWebhook org:acme"},
		Meta:      mcp.WithToken("ghp_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}

	var out githubSearchCodeOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.TotalCount != 1 || len(out.Items) != 1 || out.Items[0].Repository != "acme/widgets" {
		t.Fatalf("output = %+v, want total_count=1 repository=acme/widgets", out)
	}
}
