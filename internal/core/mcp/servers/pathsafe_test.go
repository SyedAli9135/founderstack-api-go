package servers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

func TestPathSegment(t *testing.T) {
	for _, ok := range []string{"octocat", "my-repo.v2", "a_b", "3f2a9c1e-8b7d-4e5f-9a10-1c2d3e4f5a6b"} {
		if err := pathSegment("x", ok); err != nil {
			t.Errorf("pathSegment(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../user", "a?b=1", "a#b", "a%2Fb", "a b", "a\nb", "x\\y"} {
		if err := pathSegment("x", bad); err == nil {
			t.Errorf("pathSegment(%q) = nil, want an error", bad)
		}
	}
}

func TestCheckDiscordWebhookURL(t *testing.T) {
	for _, ok := range []string{
		"https://discord.com/api/webhooks/123/abc",
		"https://canary.discord.com/api/webhooks/123/abc",
		"https://discordapp.com/api/webhooks/123/abc",
	} {
		if err := checkDiscordWebhookURL(ok); err != nil {
			t.Errorf("checkDiscordWebhookURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"http://discord.com/api/webhooks/1/a",
		"https://evil.example/api/webhooks/1/a",
		"https://discord.com.evil.example/api/webhooks/1/a",
		"https://evildiscord.com/api/webhooks/1/a",
		"https://discord.com@evil.example/api/webhooks/1/a",
		"https://discord.com/other/path",
		"http://169.254.169.254/latest/meta-data/",
		"not a url",
	} {
		if err := checkDiscordWebhookURL(bad); err == nil {
			t.Errorf("checkDiscordWebhookURL(%q) = nil, want an error", bad)
		}
	}
}

// A path-escaping argument must be refused before any request is made.
func TestToolsRefusePathTraversalArguments(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	origGH, origNotion, origDrive := githubAPIBase, notionAPIBase, driveAPIBase
	githubAPIBase, notionAPIBase, driveAPIBase = srv.URL, srv.URL, srv.URL
	t.Cleanup(func() { githubAPIBase, notionAPIBase, driveAPIBase = origGH, origNotion, origDrive })

	cases := []struct {
		name   string
		server *gomcp.Server
		tool   string
		args   map[string]any
	}{
		{"github owner", NewGitHubServer(), "review_pr", map[string]any{"owner": "../user", "repo": "r", "number": 1}},
		{"github repo", NewGitHubServer(), "create_issue", map[string]any{"owner": "o", "repo": "r/../../x", "title": "t"}},
		{"notion page", NewNotionServer(), "read_page", map[string]any{"page_id": "../users/me"}},
		{"drive file", NewGoogleDriveServer(), "read_file", map[string]any{"file_id": "x/../../about"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ct := gomcp.NewInMemoryTransports()
			ctx := context.Background()
			ss, err := tc.server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ss.Close() })
			cs, err := gomcp.NewClient(&gomcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cs.Close() })
			res, err := cs.CallTool(ctx, &gomcp.CallToolParams{Name: tc.tool, Arguments: tc.args, Meta: mcp.WithToken("tok")})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !res.IsError {
				t.Fatal("IsError = false, want a refusal")
			}
		})
	}
	if hit {
		t.Fatal("a request reached the API with a traversal-style identifier")
	}
}
