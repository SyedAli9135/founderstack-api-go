package mcp

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/core/integrations"
)

// Gateway is what the rest of the backend calls to actually run a tool. Nothing outside this
// package touches a *gomcp.ClientSession directly.
type Gateway struct {
	appPool       *pgxpool.Pool
	encryptionKey []byte
	registry      *Registry
	rdb           *redis.Client
}

// NewGateway builds a Gateway. appPool must be the app_user (RLS-enforced)
// pool — GetIntegrationToken's own query is tenant-scoped the same way
// every other BYOK/integration read in this codebase is. rdb may be nil
// (rate limiting fails open — see checkRateLimit's doc comment); pass the
// real client in production, nil only where a test genuinely doesn't
// need rate limiting exercised.
func NewGateway(appPool *pgxpool.Pool, encryptionKey []byte, registry *Registry, rdb *redis.Client) *Gateway {
	return &Gateway{appPool: appPool, encryptionKey: encryptionKey, registry: registry, rdb: rdb}
}

// ExecuteTool runs one tool call for orgID against service's MCP server.
// This is the single call site (per call) that fetches and decrypts the
// org's stored integration token (internal/core/integrations.GetIntegrationToken)
// and makes it available to the tool handler — via CallToolParams.Meta
// (see WithToken/TokenFromRequest), not a context.Value. Never pass a
// credential as a tool argument: it would then be part of the JSON
// schema the LLM sees and could plan around, echo back, or log.
//
// Token.Extra is merged in via WithExtra whenever it's non-empty —
// most connections have none, but some (Discord's webhook.incoming
// grant, e.g.) carry their real usable credential there instead of, or
// alongside, AccessToken.
//
// idempotencyKey is attached via WithIdempotencyKey whenever non-empty —
// only a handful of write handlers (Stripe's create_invoice/refund_payment)
// actually use it; every other tool handler ignores it harmlessly. Pass
// "" for tool calls where idempotency doesn't apply (reads, calls
// graph.executeOneToolCall doesn't classify as financial).
func (g *Gateway) ExecuteTool(ctx context.Context, orgID pgtype.UUID, service, toolName string, args map[string]any, idempotencyKey string) (*gomcp.CallToolResult, error) {
	if err := checkRateLimit(ctx, g.rdb, orgID.String(), service); err != nil {
		return nil, err
	}

	session, err := g.registry.sessionFor(service)
	if err != nil {
		return nil, err
	}

	tok, err := integrations.GetIntegrationToken(ctx, g.appPool, g.encryptionKey, orgID, service)
	if err != nil {
		return nil, fmt.Errorf("mcp: fetch %s token: %w", service, err)
	}

	meta := WithToken(tok.AccessToken)
	if len(tok.Extra) > 0 {
		for k, v := range WithExtra(tok.Extra) {
			meta[k] = v
		}
	}
	if idempotencyKey != "" {
		for k, v := range WithIdempotencyKey(idempotencyKey) {
			meta[k] = v
		}
	}

	result, err := session.CallTool(ctx, &gomcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
		Meta:      meta,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: call %s.%s: %w", service, toolName, err)
	}
	return result, nil
}
