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

const linkedinAPIVersion = "202601" // YYYYMM, no content-negotiation fallback

// reply_to_mention from the original plan isn't implemented: LinkedIn's
// OAuth here is scoped to w_member_social only, and mentions/comments
// need the paid Marketing Developer Platform. "draft_post" is a misnomer
// kept from the plan — this scope has no saved-draft state, so the tool
// composes and publishes in one call (its description says so).
func NewLinkedInServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "linkedin", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "draft_post",
		Description: "Compose and publish a post to LinkedIn. LinkedIn has no API-accessible draft state — this publishes immediately.",
		// Destructive-tier despite not moving money: public, permanent, no
		// undo step.
		Annotations: mcp.DestructiveOrFinancial(),
	}, linkedinDraftPost)

	return server
}

type linkedinDraftPostInput struct {
	// Explicit input, not auto-resolved: w_member_social alone doesn't
	// reliably grant the profile-read scope a resolve call would need.
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

// The Posts API returns 201 with an empty body and the new post's URN in
// the x-restli-id header, not a JSON body like every other provider here.
// Always a POST, so — same reasoning as doAndDecode's isWrite guard — a
// network-level failure never auto-retries (the post may have already
// gone live); only an explicit 429/5xx response retries.
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
