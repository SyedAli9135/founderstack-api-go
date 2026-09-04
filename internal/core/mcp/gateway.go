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

// Gateway is what the rest of the backend calls to run a tool. Nothing
// outside this package touches a *gomcp.ClientSession directly.
type Gateway struct {
	appPool       *pgxpool.Pool
	encryptionKey []byte
	registry      *Registry
	rdb           *redis.Client
}

// NewGateway builds a Gateway. appPool must be the app_user (RLS-enforced) pool.
func NewGateway(appPool *pgxpool.Pool, encryptionKey []byte, registry *Registry, rdb *redis.Client) *Gateway {
	return &Gateway{appPool: appPool, encryptionKey: encryptionKey, registry: registry, rdb: rdb}
}

// ExecuteTool runs one tool call for orgID against service's MCP server.
// The org's decrypted token is passed via CallToolParams.Meta (WithToken),
// never as a context.Value (doesn't cross the real MCP session boundary)
// or a tool argument (would be visible to the LLM).
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
