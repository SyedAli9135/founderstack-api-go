package servers

import (
	"context"
	"errors"
	"fmt"
	"io"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// linkedinAPIBase is a var so linkedin_test.go can point it at a fake
// server.
var linkedinAPIBase = "https://api.linkedin.com/rest"

// linkedinAPIVersion is LinkedIn's required versioned-API header
// (YYYYMM), same "no content-negotiation fallback" story as Notion's
// Notion-Version.
const linkedinAPIVersion = "202601"

// NewLinkedInServer builds the LinkedIn MCP tool server (WORKFLOW_PLAN_GO.md
// workflow 5) — draft_post only. reply_to_mention from the original plan
// is deliberately not implemented: workflow 4 scoped LinkedIn's OAuth to
// just the free "Share on LinkedIn" product (w_member_social) rather than
// the paid, partner-gated Marketing Developer Platform (see
// founderstack-api-go/CLAUDE.md's Third-Party Integrations section) —
// reading/replying to mentions and comments lives behind that gated
// platform, not w_member_social, so there's no real API this tool could
// call. Re-add it if LinkedIn's OAuth scope is ever widened.
//
// "draft_post" is a slight misnomer carried over from the original plan:
// LinkedIn's API has no saved-draft state reachable with this scope —
// this tool composes and publishes the post in one call. The tool's
// description says so explicitly rather than implying a review step that
// doesn't exist.
func NewLinkedInServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "linkedin", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "draft_post",
		Description: "Compose and publish a post to LinkedIn. LinkedIn has no API-accessible draft state — this publishes immediately.",
		// Destructive-tier despite not moving money: a public, permanent,
		// brand-visible post published with no draft/undo step is exactly
		// the kind of irreversible external action the risk model treats
		// the same as a financial one — see internal/core/mcp/risk.go.
		Annotations: mcp.DestructiveOrFinancial(),
	}, linkedinDraftPost)

	return server
}

type linkedinDraftPostInput struct {
	// AuthorURN identifies who the post is published as, e.g.
	// "urn:li:person:abc123". Accepted as an explicit input rather than
	// resolved automatically: w_member_social alone doesn't reliably
	// grant profile-read access (that's typically bundled with a
	// separate openid/profile scope this integration doesn't request),
	// so guessing at it here would assume a permission workflow 4 never
	// actually granted.
	AuthorURN string `json:"author_urn" jsonschema:"LinkedIn person URN to post as, e.g. urn:li:person:abc123"`
	Text      string `json:"text" jsonschema:"Post text"`
}

type linkedinDraftPostOutput struct {
	PostID string `json:"post_id"`
}

func linkedinDraftPost(ctx context.Context, req *gomcp.CallToolRequest, in linkedinDraftPostInput) (*gomcp.CallToolResult, linkedinDraftPostOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, linkedinDraftPostOutput{}, fmt.Errorf("linkedin: no token in request metadata")
	}
	if in.AuthorURN == "" || in.Text == "" {
		return nil, linkedinDraftPostOutput{}, fmt.Errorf("linkedin: author_urn and text are required")
	}

	payload := map[string]any{
		"author":     in.AuthorURN,
		"commentary": in.Text,
		"visibility": "PUBLIC",
		"distribution": map[string]any{
			"feedDistribution":               "MAIN_FEED",
			"targetEntities":                 []any{},
			"thirdPartyDistributionChannels": []any{},
		},
		"lifecycleState":            "PUBLISHED",
		"isReshareDisabledByAuthor": false,
	}

	postID, err := doLinkedInCreate(ctx, linkedinAPIBase+"/posts", token, payload)
	if err != nil {
		return nil, linkedinDraftPostOutput{}, fmt.Errorf("linkedin: create post: %w", err)
	}

	return nil, linkedinDraftPostOutput{PostID: postID}, nil
}

// doLinkedInCreate posts payload to endpoint and returns the created
// resource's ID from the LinkedIn-convention x-restli-id response
// header — the Posts API returns 201 with an empty body and the new
// post's URN in that header, not in a JSON body like every other
// provider here.
// doLinkedInCreate is always a POST (publishing a post has no read
// variant), so — same reasoning as doAndDecode's isWrite guard — a
// network-level failure never auto-retries here: the post may have
// already gone live before the response was lost, and retrying could
// double-publish. Only an explicit 429/5xx response (the server
// rejected the request outright, without applying it) retries.
func doLinkedInCreate(ctx context.Context, endpoint, token string, payload any) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= toolCallMaxAttempts; attempt++ {
		req, err := newRequestWithBody(ctx, "POST", endpoint, payload)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("LinkedIn-Version", linkedinAPIVersion)
		req.Header.Set("X-Restli-Protocol-Version", "2.0.0")
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %w", mcp.ErrToolRetryable, req.URL, err)
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			classified := mcp.ClassifyToolHTTPError(resp.StatusCode, fmt.Sprintf("%s returned: %s", req.URL, truncate(body, 500)))
			if errors.Is(classified, mcp.ErrToolRetryable) && attempt < toolCallMaxAttempts && sleepBackoff(ctx, attempt) {
				lastErr = classified
				continue
			}
			return "", classified
		}
		return resp.Header.Get("x-restli-id"), nil
	}
	return "", lastErr
}
