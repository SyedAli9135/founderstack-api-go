package servers

import (
	"context"
	"fmt"
	"net/url"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// githubAPIBase is a var so github_test.go can point it at a fake server.
var githubAPIBase = "https://api.github.com"

// NewGitHubServer builds the GitHub MCP tool server (WORKFLOW_PLAN_GO.md
// workflow 5) — review_pr, search_code, create_issue. Auth is a
// Bearer-prefixed PAT, matching the token stored by workflow 4's GitHub
// PAT connect flow.
func NewGitHubServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "github", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "review_pr",
		Description: "Fetch a pull request's metadata and per-file diffs, for the agent to review.",
		Annotations: mcp.ReadOnly(),
	}, githubReviewPR)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "search_code",
		Description: "Search code across GitHub (or a specific repo) using GitHub's code search syntax.",
		Annotations: mcp.ReadOnly(),
	}, githubSearchCode)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "create_issue",
		Description: "Create an issue in a GitHub repository.",
		Annotations: mcp.ReversibleWrite(),
	}, githubCreateIssue)

	return server
}

// doGitHub is doJSON with GitHub's recommended Accept/API-Version headers
// layered on — GitHub's REST API tolerates plain "application/json" but
// the documented, versioned contract is "application/vnd.github+json"
// plus X-GitHub-Api-Version, so tool responses don't silently start
// differing shape on a future API default-version bump.
func doGitHub(ctx context.Context, method, endpoint, token string, body, out any) error {
	req, err := newRequestWithBody(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return doAndDecode(req, out)
}

type githubReviewPRInput struct {
	Owner  string `json:"owner" jsonschema:"Repository owner (user or org)"`
	Repo   string `json:"repo" jsonschema:"Repository name"`
	Number int    `json:"number" jsonschema:"Pull request number"`
}

type githubFileDiff struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"`
}

type githubReviewPROutput struct {
	Title string           `json:"title"`
	Body  string           `json:"body"`
	State string           `json:"state"`
	Files []githubFileDiff `json:"files"`
}

type githubPRResponse struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	State string `json:"state"`
}

func githubReviewPR(ctx context.Context, req *gomcp.CallToolRequest, in githubReviewPRInput) (*gomcp.CallToolResult, githubReviewPROutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, githubReviewPROutput{}, fmt.Errorf("github: no token in request metadata")
	}
	if in.Owner == "" || in.Repo == "" || in.Number == 0 {
		return nil, githubReviewPROutput{}, fmt.Errorf("github: owner, repo, and number are required")
	}

	var pr githubPRResponse
	prURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", githubAPIBase, in.Owner, in.Repo, in.Number)
	if err := doGitHub(ctx, "GET", prURL, token, nil, &pr); err != nil {
		return nil, githubReviewPROutput{}, fmt.Errorf("github: fetch PR: %w", err)
	}

	var files []githubFileDiff
	filesURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files?per_page=100", githubAPIBase, in.Owner, in.Repo, in.Number)
	if err := doGitHub(ctx, "GET", filesURL, token, nil, &files); err != nil {
		return nil, githubReviewPROutput{}, fmt.Errorf("github: fetch PR files: %w", err)
	}

	return nil, githubReviewPROutput{Title: pr.Title, Body: pr.Body, State: pr.State, Files: files}, nil
}

type githubSearchCodeInput struct {
	Query string `json:"query" jsonschema:"GitHub code search query, e.g. 'org:acme handleWebhook language:go'"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum results to return (default 10, max 50)"`
}

type githubCodeMatch struct {
	Path       string `json:"path"`
	Repository string `json:"repository"`
	HTMLURL    string `json:"html_url"`
}

type githubSearchCodeOutput struct {
	TotalCount int               `json:"total_count"`
	Items      []githubCodeMatch `json:"items"`
}

type githubSearchCodeResponse struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		Path       string `json:"path"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HTMLURL string `json:"html_url"`
	} `json:"items"`
}

func githubSearchCode(ctx context.Context, req *gomcp.CallToolRequest, in githubSearchCodeInput) (*gomcp.CallToolResult, githubSearchCodeOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, githubSearchCodeOutput{}, fmt.Errorf("github: no token in request metadata")
	}
	if in.Query == "" {
		return nil, githubSearchCodeOutput{}, fmt.Errorf("github: query is required")
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	searchURL := fmt.Sprintf("%s/search/code?q=%s&per_page=%d", githubAPIBase, url.QueryEscape(in.Query), limit)
	var resp githubSearchCodeResponse
	if err := doGitHub(ctx, "GET", searchURL, token, nil, &resp); err != nil {
		return nil, githubSearchCodeOutput{}, fmt.Errorf("github: search code: %w", err)
	}

	out := githubSearchCodeOutput{TotalCount: resp.TotalCount, Items: make([]githubCodeMatch, 0, len(resp.Items))}
	for _, item := range resp.Items {
		out.Items = append(out.Items, githubCodeMatch{Path: item.Path, Repository: item.Repository.FullName, HTMLURL: item.HTMLURL})
	}
	return nil, out, nil
}

type githubCreateIssueInput struct {
	Owner string `json:"owner" jsonschema:"Repository owner (user or org)"`
	Repo  string `json:"repo" jsonschema:"Repository name"`
	Title string `json:"title" jsonschema:"Issue title"`
	Body  string `json:"body,omitempty" jsonschema:"Issue body (markdown)"`
}

type githubCreateIssueOutput struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

type githubIssueResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

func githubCreateIssue(ctx context.Context, req *gomcp.CallToolRequest, in githubCreateIssueInput) (*gomcp.CallToolResult, githubCreateIssueOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, githubCreateIssueOutput{}, fmt.Errorf("github: no token in request metadata")
	}
	if in.Owner == "" || in.Repo == "" || in.Title == "" {
		return nil, githubCreateIssueOutput{}, fmt.Errorf("github: owner, repo, and title are required")
	}

	body := map[string]string{"title": in.Title}
	if in.Body != "" {
		body["body"] = in.Body
	}

	issuesURL := fmt.Sprintf("%s/repos/%s/%s/issues", githubAPIBase, in.Owner, in.Repo)
	var issue githubIssueResponse
	if err := doGitHub(ctx, "POST", issuesURL, token, body, &issue); err != nil {
		return nil, githubCreateIssueOutput{}, fmt.Errorf("github: create issue: %w", err)
	}

	return nil, githubCreateIssueOutput{Number: issue.Number, HTMLURL: issue.HTMLURL}, nil
}
