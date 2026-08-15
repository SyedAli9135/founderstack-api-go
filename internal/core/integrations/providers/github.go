package providers

import "context"

// GitHub is a KeyProvider — the founder pastes a Personal Access Token
// rather than authorizing an OAuth app. ValidatePAT hits GET /user, which
// works for both classic and fine-grained PATs and requires no specific
// scope beyond "identify yourself."
type GitHub struct{}

func NewGitHub() *GitHub { return &GitHub{} }

func (g *GitHub) Name() string { return "github" }

func (g *GitHub) ValidateKey(ctx context.Context, key string) error {
	return bearerRequest(ctx, "GET", "https://api.github.com/user", key)
}
