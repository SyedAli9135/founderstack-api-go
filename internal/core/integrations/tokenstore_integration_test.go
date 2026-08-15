//go:build integration

package integrations

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/response"
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

func testEncryptionKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// testOrg inserts a fresh org directly via systemPool (BYPASSRLS), same
// as testOrgAndUser in settings/apikey_integration_test.go, and returns
// its ID as a pgtype.UUID ready for tenant.WithTx.
func testOrg(t *testing.T, systemPool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	suffix := response.NewID()[:12]
	clerkOrgID := "org_integrations_test_" + suffix
	ctx := context.Background()

	var id pgtype.UUID
	err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Integrations Test Org', $2) returning id",
		clerkOrgID, "integrations-test-"+suffix,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where clerk_org_id = $1", clerkOrgID)
	})
	return id
}

func TestTokenstore_SaveGetConnection_RoundTrip(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	encKey := testEncryptionKey(t)
	orgID := testOrg(t, systemPool)
	ctx := context.Background()

	expiresAt := time.Now().Add(time.Hour).UTC().Round(time.Second)
	tok := Token{
		AccessToken:  "xoxb-fake-slack-token",
		RefreshToken: "refresh-abc",
		ExpiresAt:    expiresAt,
		Scopes:       []string{"chat:write", "channels:read"},
		Extra:        map[string]string{"team_id": "T123"},
	}

	if err := SaveConnection(ctx, appPool, encKey, orgID, "slack", "Slack", "oauth", "connected", tok); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}

	conn, err := GetConnection(ctx, appPool, encKey, orgID, "slack")
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if conn.Token.AccessToken != tok.AccessToken || conn.Token.RefreshToken != tok.RefreshToken {
		t.Fatalf("got token %+v, want access/refresh to match %+v", conn.Token, tok)
	}
	if !conn.Token.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("got expires_at %v, want %v", conn.Token.ExpiresAt, expiresAt)
	}
	if conn.Token.Extra["team_id"] != "T123" {
		t.Fatalf("got extra %+v, want team_id=T123", conn.Token.Extra)
	}
	if conn.OAuthStatus != "connected" || !conn.IsActive {
		t.Fatalf("got status=%s active=%v, want connected/true", conn.OAuthStatus, conn.IsActive)
	}

	// Verify the raw column actually holds ciphertext, not the plaintext token.
	var raw string
	if err := systemPool.QueryRow(ctx,
		"select encrypted_credentials from mcp_connections where org_id = $1 and service_name = 'slack'", orgID,
	).Scan(&raw); err != nil {
		t.Fatalf("query encrypted_credentials: %v", err)
	}
	if raw == "" {
		t.Fatal("encrypted_credentials is empty")
	}
	for _, secret := range []string{tok.AccessToken, tok.RefreshToken} {
		if strings.Contains(raw, secret) {
			t.Fatalf("plaintext %q found in encrypted_credentials column", secret)
		}
	}
}

func TestTokenstore_SaveConnection_UpsertsNotDuplicates(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	encKey := testEncryptionKey(t)
	orgID := testOrg(t, systemPool)
	ctx := context.Background()

	first := Token{AccessToken: "token-v1"}
	second := Token{AccessToken: "token-v2"}

	if err := SaveConnection(ctx, appPool, encKey, orgID, "notion", "Notion", "oauth", "connected", first); err != nil {
		t.Fatalf("first SaveConnection: %v", err)
	}
	if err := SaveConnection(ctx, appPool, encKey, orgID, "notion", "Notion", "oauth", "connected", second); err != nil {
		t.Fatalf("second SaveConnection: %v", err)
	}

	var count int
	if err := systemPool.QueryRow(ctx,
		"select count(*) from mcp_connections where org_id = $1 and service_name = 'notion'", orgID,
	).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("got %d rows for (org, notion), want exactly 1 — upsert should replace, not duplicate", count)
	}

	conn, err := GetConnection(ctx, appPool, encKey, orgID, "notion")
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	if conn.Token.AccessToken != "token-v2" {
		t.Fatalf("got access token %q, want the second (latest) value token-v2", conn.Token.AccessToken)
	}
}

func TestTokenstore_GetConnection_NotFound(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	encKey := testEncryptionKey(t)
	orgID := testOrg(t, systemPool)

	_, err := GetConnection(context.Background(), appPool, encKey, orgID, "github")
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("got err %v, want pgx.ErrNoRows", err)
	}

	_, err = GetIntegrationToken(context.Background(), appPool, encKey, orgID, "github")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("GetIntegrationToken: got err %v, want ErrNotConnected", err)
	}
}

func TestTokenstore_RevokeConnection(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	encKey := testEncryptionKey(t)
	orgID := testOrg(t, systemPool)
	ctx := context.Background()

	if err := SaveConnection(ctx, appPool, encKey, orgID, "github", "GitHub", "manual", "connected", Token{AccessToken: "ghp_fake"}); err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
	if err := RevokeConnection(ctx, appPool, orgID, "github"); err != nil {
		t.Fatalf("RevokeConnection: %v", err)
	}

	conn, err := GetConnection(ctx, appPool, encKey, orgID, "github")
	if err != nil {
		t.Fatalf("GetConnection after revoke: %v", err)
	}
	if conn.IsActive || conn.OAuthStatus != "revoked" {
		t.Fatalf("got active=%v status=%s, want active=false status=revoked", conn.IsActive, conn.OAuthStatus)
	}

	_, err = GetIntegrationToken(ctx, appPool, encKey, orgID, "github")
	if !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("GetIntegrationToken after revoke: got err %v, want ErrTokenUnavailable", err)
	}
}

func TestTokenstore_ListConnections_CrossOrgIsolation(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	encKey := testEncryptionKey(t)
	orgA := testOrg(t, systemPool)
	orgB := testOrg(t, systemPool)
	ctx := context.Background()

	if err := SaveConnection(ctx, appPool, encKey, orgA, "stripe", "Stripe", "manual", "connected", Token{AccessToken: "sk_test_a"}); err != nil {
		t.Fatalf("SaveConnection org A: %v", err)
	}

	listA, err := ListConnections(ctx, appPool, orgA)
	if err != nil {
		t.Fatalf("ListConnections org A: %v", err)
	}
	if len(listA) != 1 || listA[0].ServiceName != "stripe" {
		t.Fatalf("org A connections = %+v, want exactly one stripe row", listA)
	}

	listB, err := ListConnections(ctx, appPool, orgB)
	if err != nil {
		t.Fatalf("ListConnections org B: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("org B saw %d connections, want 0 — cross-tenant leak", len(listB))
	}
}
