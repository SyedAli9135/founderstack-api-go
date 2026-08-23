package mcp

import (
	"context"
	"crypto/rand"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/founderstack/api/internal/core/integrations"
)

func testAppPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_APP_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_APP_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to app test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testSystemPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_SYSTEM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_SYSTEM_DATABASE_URL not set; skipping integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to system test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// echoTokenServer is a fake tool server (not a real Stripe/Slack call)
// whose one tool reports whatever token it received via _meta — this
// tests Gateway.ExecuteTool's actual job (fetch + decrypt + deliver a
// real org's token from Postgres) without depending on any third-party
// API being reachable or a fake token being accepted by one.
func echoTokenServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "echo", Version: "1.0.0"}, nil)
	gomcp.AddTool(server, &gomcp.Tool{Name: "echo_token"}, func(ctx context.Context, req *gomcp.CallToolRequest, _ struct{}) (*gomcp.CallToolResult, struct {
		Token string `json:"token"`
	}, error) {
		tok, _ := TokenFromRequest(req)
		return nil, struct {
			Token string `json:"token"`
		}{Token: tok}, nil
	})
	return server
}

func TestGateway_ExecuteTool_FetchesAndDeliversRealToken(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	ctx := context.Background()

	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatal(err)
	}

	var orgID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		"insert into organizations (name, slug, clerk_org_id) values ('MCP Gateway Test Org', 'mcp-gateway-test-org', 'clerk_mcp_gw_test') returning id",
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})

	const plaintextToken = "echo-service-real-token-xyz"
	if err := integrations.SaveConnection(ctx, appPool, encKey, orgID, "echo", "Echo", "manual", "connected",
		integrations.Token{AccessToken: plaintextToken},
	); err != nil {
		t.Fatalf("save test connection: %v", err)
	}

	registry, err := NewRegistry(ctx, map[string]*gomcp.Server{"echo": echoTokenServer()})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	gateway := NewGateway(appPool, encKey, registry)

	result, err := gateway.ExecuteTool(ctx, orgID, "echo", "echo_token", nil)
	if err != nil {
		t.Fatalf("ExecuteTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}

	got, ok := result.StructuredContent.(map[string]any)
	if !ok || got["token"] != plaintextToken {
		t.Fatalf("StructuredContent = %+v, want token=%q", result.StructuredContent, plaintextToken)
	}
}

func TestGateway_ExecuteTool_UnknownServiceIsError(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	ctx := context.Background()

	registry, err := NewRegistry(ctx, map[string]*gomcp.Server{"echo": echoTokenServer()})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	gateway := NewGateway(appPool, make([]byte, 32), registry)

	var orgID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		"insert into organizations (name, slug, clerk_org_id) values ('MCP Gateway Test Org 2', 'mcp-gateway-test-org-2', 'clerk_mcp_gw_test_2') returning id",
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})

	if _, err := gateway.ExecuteTool(ctx, orgID, "not-a-real-service", "whatever", nil); err == nil {
		t.Fatal("ExecuteTool() error = nil, want ErrUnknownTool")
	}
}
