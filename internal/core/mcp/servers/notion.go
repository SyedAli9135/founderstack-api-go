package servers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// notionAPIBase is a var so notion_test.go can point it at a fake server.
var notionAPIBase = "https://api.notion.com/v1"

// Notion has no content-negotiation fallback: omitting this header is
// itself a request error, not just a risk of drift.
const notionAPIVersion = "2022-06-28"

func NewNotionServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "notion", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "read_page",
		Description: "Read a Notion page's title and text content.",
		Annotations: mcp.ReadOnly(),
	}, notionReadPage)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "write_page",
		Description: "Create a new Notion page (e.g. an SOP or blog draft) under a parent page.",
		Annotations: mcp.ReversibleWrite(),
	}, notionWritePage)

	return server
}

// Wraps newRequestWithBody rather than teaching doJSON about one
// provider's Notion-Version header.
func doNotion(ctx context.Context, method, endpoint, token string, body, out any) error {
	req, err := newRequestWithBody(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Notion-Version", notionAPIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return doAndDecode(req, out)
}

type notionReadPageInput struct {
	PageID string `json:"page_id" jsonschema:"Notion page ID"`
}

type notionReadPageOutput struct {
	Title   string   `json:"title"`
	Content []string `json:"content"`
}

type notionRichText struct {
	PlainText string `json:"plain_text"`
}

type notionPageResponse struct {
	Properties map[string]struct {
		Type  string           `json:"type"`
		Title []notionRichText `json:"title"`
	} `json:"properties"`
}

type notionBlockChildrenResponse struct {
	Results []json.RawMessage `json:"results"`
}

func notionReadPage(ctx context.Context, req *gomcp.CallToolRequest, in notionReadPageInput) (*gomcp.CallToolResult, notionReadPageOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, notionReadPageOutput{}, fmt.Errorf("notion: no token in request metadata")
	}
	if in.PageID == "" {
		return nil, notionReadPageOutput{}, fmt.Errorf("notion: page_id is required")
	}

	var page notionPageResponse
	if err := doNotion(ctx, "GET", notionAPIBase+"/pages/"+in.PageID, token, nil, &page); err != nil {
		return nil, notionReadPageOutput{}, fmt.Errorf("notion: fetch page: %w", err)
	}
	title := notionExtractTitle(page)

	var children notionBlockChildrenResponse
	childrenURL := notionAPIBase + "/blocks/" + in.PageID + "/children?page_size=100"
	if err := doNotion(ctx, "GET", childrenURL, token, nil, &children); err != nil {
		return nil, notionReadPageOutput{}, fmt.Errorf("notion: fetch page content: %w", err)
	}

	return nil, notionReadPageOutput{Title: title, Content: notionExtractBlockText(children.Results)}, nil
}

// notionExtractTitle finds the page's title property. A page's title
// property key is always literally "title" when its parent is another
// page, but can be any name when its parent is a database — so this
// scans by the property's declared type instead of assuming a key name.
func notionExtractTitle(page notionPageResponse) string {
	for _, prop := range page.Properties {
		if prop.Type != "title" {
			continue
		}
		var sb strings.Builder
		for _, rt := range prop.Title {
			sb.WriteString(rt.PlainText)
		}
		return sb.String()
	}
	return ""
}

// notionExtractBlockText pulls plain text out of a page's top-level
// blocks. Every Notion block type that carries text (paragraph,
// heading_1/2/3, bulleted_list_item, numbered_list_item, quote, to_do)
// stores it the same way — {"<type>": {"rich_text": [...]}} — so one
// generic decode covers all of them without a case per type. Other block
// types (images, tables, embeds, ...) and nested children are skipped,
// not errored: this is a text-content reader, not a full Notion renderer.
func notionExtractBlockText(blocks []json.RawMessage) []string {
	var lines []string
	for _, raw := range blocks {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			continue
		}
		inner, ok := fields[envelope.Type]
		if !ok {
			continue
		}
		var holder struct {
			RichText []notionRichText `json:"rich_text"`
		}
		if err := json.Unmarshal(inner, &holder); err != nil || len(holder.RichText) == 0 {
			continue
		}
		var sb strings.Builder
		for _, rt := range holder.RichText {
			sb.WriteString(rt.PlainText)
		}
		if sb.Len() > 0 {
			lines = append(lines, sb.String())
		}
	}
	return lines
}

type notionWritePageInput struct {
	ParentPageID string `json:"parent_page_id" jsonschema:"Notion page ID to create the new page under"`
	Title        string `json:"title" jsonschema:"Title of the new page"`
	// Body is split on blank lines into separate paragraph blocks — a
	// deliberately simple markdown-free convention, not full Notion
	// block-type support (no headings, lists, etc. from this tool).
	Body string `json:"body,omitempty" jsonschema:"Page body text; blank lines separate paragraphs"`
}

type notionWritePageOutput struct {
	PageID string `json:"page_id"`
	URL    string `json:"url"`
}

type notionCreatePageResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func notionWritePage(ctx context.Context, req *gomcp.CallToolRequest, in notionWritePageInput) (*gomcp.CallToolResult, notionWritePageOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, notionWritePageOutput{}, fmt.Errorf("notion: no token in request metadata")
	}
	if in.ParentPageID == "" || in.Title == "" {
		return nil, notionWritePageOutput{}, fmt.Errorf("notion: parent_page_id and title are required")
	}

	payload := map[string]any{
		"parent": map[string]string{"page_id": in.ParentPageID},
		"properties": map[string]any{
			"title": map[string]any{
				"title": []map[string]any{
					{"text": map[string]string{"content": in.Title}},
				},
			},
		},
	}
	if children := notionParagraphBlocks(in.Body); len(children) > 0 {
		payload["children"] = children
	}

	var created notionCreatePageResponse
	if err := doNotion(ctx, "POST", notionAPIBase+"/pages", token, payload, &created); err != nil {
		return nil, notionWritePageOutput{}, fmt.Errorf("notion: create page: %w", err)
	}

	return nil, notionWritePageOutput{PageID: created.ID, URL: created.URL}, nil
}

func notionParagraphBlocks(body string) []map[string]any {
	if body == "" {
		return nil
	}
	var blocks []map[string]any
	for _, para := range strings.Split(body, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		blocks = append(blocks, map[string]any{
			"object": "block",
			"type":   "paragraph",
			"paragraph": map[string]any{
				"rich_text": []map[string]any{
					{"type": "text", "text": map[string]string{"content": para}},
				},
			},
		})
	}
	return blocks
}
