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

func connectNotionServer(t *testing.T) *gomcp.ClientSession {
	t.Helper()
	server := NewNotionServer()
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

func swapNotionAPIBase(url string) func() {
	original := notionAPIBase
	notionAPIBase = url
	return func() { notionAPIBase = original }
}

func TestNotion_ReadPage(t *testing.T) {
	var gotVersionHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersionHeader = r.Header.Get("Notion-Version")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/pages/page123":
			_, _ = w.Write([]byte(`{"properties":{"Name":{"type":"title","title":[{"plain_text":"My SOP"}]}}}`))
		case "/blocks/page123/children":
			_, _ = w.Write([]byte(`{"results":[
				{"type":"paragraph","paragraph":{"rich_text":[{"plain_text":"First line."}]}},
				{"type":"divider","divider":{}},
				{"type":"heading_1","heading_1":{"rich_text":[{"plain_text":"A heading"}]}}
			]}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	defer swapNotionAPIBase(srv.URL)()

	session := connectNotionServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "read_page",
		Arguments: map[string]any{"page_id": "page123"},
		Meta:      mcp.WithToken("secret_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if gotVersionHeader != notionAPIVersion {
		t.Errorf("Notion-Version = %q, want %q", gotVersionHeader, notionAPIVersion)
	}

	var out notionReadPageOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Title != "My SOP" {
		t.Errorf("title = %q, want My SOP", out.Title)
	}
	// The divider block has no rich_text and must be silently skipped,
	// not error the whole read.
	if len(out.Content) != 2 || out.Content[0] != "First line." || out.Content[1] != "A heading" {
		t.Fatalf("content = %+v, want [First line., A heading] (divider skipped)", out.Content)
	}
}

func TestNotion_WritePage_MissingFieldsIsToolError(t *testing.T) {
	session := connectNotionServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "write_page",
		Arguments: map[string]any{"title": "Untitled"},
		Meta:      mcp.WithToken("secret_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for missing parent_page_id")
	}
}

func TestNotion_WritePage(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"newpage1","url":"https://notion.so/newpage1"}`))
	}))
	defer srv.Close()
	defer swapNotionAPIBase(srv.URL)()

	session := connectNotionServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "write_page",
		Arguments: map[string]any{
			"parent_page_id": "parent1",
			"title":          "New SOP",
			"body":           "Paragraph one.\n\nParagraph two.",
		},
		Meta: mcp.WithToken("secret_test_token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}

	var out notionWritePageOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.PageID != "newpage1" {
		t.Fatalf("page_id = %q, want newpage1", out.PageID)
	}
	children, _ := gotBody["children"].([]any)
	if len(children) != 2 {
		t.Fatalf("sent %d children blocks, want 2 (one per paragraph)", len(children))
	}
}
