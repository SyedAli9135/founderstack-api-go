package mcp

import (
	"context"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Registry holds one connected MCP client session per tool server, keyed
// by the same service name internal/core/integrations uses so
// Gateway.ExecuteTool can look up credentials and dispatch the call with
// one name.
type Registry struct {
	sessions map[string]*gomcp.ClientSession
}

// NewRegistry connects one in-process client/server pair per entry in
// servers, once at startup rather than lazily per call.
func NewRegistry(ctx context.Context, servers map[string]*gomcp.Server) (*Registry, error) {
	sessions := make(map[string]*gomcp.ClientSession, len(servers))
	for service, server := range servers {
		serverTransport, clientTransport := gomcp.NewInMemoryTransports()

		if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
			return nil, fmt.Errorf("mcp: connect %s server: %w", service, err)
		}

		client := gomcp.NewClient(&gomcp.Implementation{
			Name:    "founderstack-gateway",
			Version: "1.0.0",
		}, nil)
		session, err := client.Connect(ctx, clientTransport, nil)
		if err != nil {
			return nil, fmt.Errorf("mcp: connect %s client: %w", service, err)
		}
		sessions[service] = session
	}
	return &Registry{sessions: sessions}, nil
}

// ErrUnknownTool means service has no registered MCP server — a
// programming error (a caller asking for a tool server that was never
// added to the registry), not a founder-facing condition.
var ErrUnknownTool = fmt.Errorf("mcp: unknown tool server")

func (r *Registry) sessionFor(service string) (*gomcp.ClientSession, error) {
	session, ok := r.sessions[service]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTool, service)
	}
	return session, nil
}

// ListTools enumerates every tool on every registered service, keyed by
// service name. Used by cmd/seedtools to build Pinecone embeddings for
// semantic tool discovery
func (r *Registry) ListTools(ctx context.Context) (map[string][]*gomcp.Tool, error) {
	result := make(map[string][]*gomcp.Tool, len(r.sessions))
	for service, session := range r.sessions {
		var tools []*gomcp.Tool
		cursor := ""
		for {
			page, err := session.ListTools(ctx, &gomcp.ListToolsParams{Cursor: cursor})
			if err != nil {
				return nil, fmt.Errorf("mcp: list tools for %s: %w", service, err)
			}
			tools = append(tools, page.Tools...)
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		result[service] = tools
	}
	return result, nil
}
