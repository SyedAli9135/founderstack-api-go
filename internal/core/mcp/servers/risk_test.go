package servers

import (
	"context"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	coremcp "github.com/founderstack/api/internal/core/mcp"
)

// TestAllServers_EveryToolHasRiskAnnotations is a regression guard, not
// just a one-off check: every tool this codebase registers must carry
// ToolAnnotations, because mcp.RiskLevelFor fails closed (treats a
// missing annotation as the most restrictive tier) specifically so a
// forgotten annotation can never silently skip the approval gate — but
// "fails closed" only helps if something actually notices the mistake.
// This connects through a real MCP client/server session
// (mcp.NewInMemoryTransports), the same way every other tool test in this
// package does, so it's also proving the annotation survives real
// protocol serialization, not just present on the local Go struct.
func TestAllServers_EveryToolHasRiskAnnotations(t *testing.T) {
	ctx := context.Background()

	for service, server := range AllServers() {
		serverTransport, clientTransport := gomcp.NewInMemoryTransports()
		if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
			t.Fatalf("%s: connect server: %v", service, err)
		}
		client := gomcp.NewClient(&gomcp.Implementation{Name: "risk-test", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, clientTransport, nil)
		if err != nil {
			t.Fatalf("%s: connect client: %v", service, err)
		}
		t.Cleanup(func() { _ = session.Close() })

		page, err := session.ListTools(ctx, &gomcp.ListToolsParams{})
		if err != nil {
			t.Fatalf("%s: list tools: %v", service, err)
		}
		if len(page.Tools) == 0 {
			t.Fatalf("%s: registered zero tools", service)
		}

		for _, tool := range page.Tools {
			if tool.Annotations == nil {
				t.Errorf("%s.%s has no ToolAnnotations set — RiskLevelFor will fail closed to %q, but every tool must classify itself explicitly, not rely on the fallback",
					service, tool.Name, coremcp.RiskWriteDestructiveOrFinancial)
				continue
			}
			// Every tool must land on exactly one of the 3 defined tiers
			// — RiskLevelFor can't return an unrecognized value by
			// construction, but assert it here too so this test documents
			// the invariant directly.
			level := coremcp.RiskLevelFor(tool.Annotations)
			switch level {
			case coremcp.RiskRead, coremcp.RiskWriteReversible, coremcp.RiskWriteDestructiveOrFinancial:
			default:
				t.Errorf("%s.%s: RiskLevelFor returned unrecognized level %q", service, tool.Name, level)
			}
		}
	}
}
