//go:build integration

package llm

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/pkg/vault"
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

func TestGetClient_DecryptsAndBuildsClient(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	ctx := context.Background()

	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatal(err)
	}
	const plaintextKey = "sk-ant-test-genuine-fixture-key"
	encrypted, err := vault.Encrypt(plaintextKey, encKey)
	if err != nil {
		t.Fatal(err)
	}

	var orgID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		"insert into organizations (name, slug, clerk_org_id) values ('LLM Test Org', 'llm-test-org', 'clerk_llm_test_org') returning id",
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})
	if _, err := systemPool.Exec(ctx,
		`insert into api_key_registry (org_id, provider, key_prefix, encrypted_key, kms_key_id, is_valid)
		 values ($1, 'anthropic', 'sk-ant-...', $2, 'local-aes-gcm', true)`,
		orgID, encrypted,
	); err != nil {
		t.Fatalf("insert test key: %v", err)
	}

	client, err := GetClient(ctx, appPool, encKey, orgID)
	if err != nil {
		t.Fatalf("GetClient() error = %v, want nil", err)
	}
	if client.Options == nil {
		t.Fatal("GetClient() returned a zero-value client")
	}
}

func TestGetClient_NoKeyReturnsErrNoRows(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	ctx := context.Background()

	var orgID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		"insert into organizations (name, slug, clerk_org_id) values ('No Key Org', 'no-key-org', 'clerk_no_key_org') returning id",
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})

	_, err := GetClient(ctx, appPool, make([]byte, 32), orgID)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetClient() error = %v, want pgx.ErrNoRows (unwrapped)", err)
	}
}

func TestGetClient_CrossOrgIsolation(t *testing.T) {
	systemPool := testSystemPool(t)
	appPool := testAppPool(t)
	ctx := context.Background()

	encKey := make([]byte, 32)
	_, _ = rand.Read(encKey)
	encrypted, err := vault.Encrypt("sk-ant-test-org-a-only", encKey)
	if err != nil {
		t.Fatal(err)
	}

	var orgA, orgB pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		"insert into organizations (name, slug, clerk_org_id) values ('Org A', 'llm-iso-org-a', 'clerk_llm_iso_a') returning id",
	).Scan(&orgA); err != nil {
		t.Fatal(err)
	}
	if err := systemPool.QueryRow(ctx,
		"insert into organizations (name, slug, clerk_org_id) values ('Org B', 'llm-iso-org-b', 'clerk_llm_iso_b') returning id",
	).Scan(&orgB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = systemPool.Exec(ctx, "delete from organizations where id in ($1, $2)", orgA, orgB)
	})
	if _, err := systemPool.Exec(ctx,
		`insert into api_key_registry (org_id, provider, key_prefix, encrypted_key, kms_key_id, is_valid)
		 values ($1, 'anthropic', 'sk-ant-...', $2, 'local-aes-gcm', true)`,
		orgA, encrypted,
	); err != nil {
		t.Fatal(err)
	}

	// Org B has no key of its own and must not be able to resolve org A's.
	_, err = GetClient(ctx, appPool, encKey, orgB)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetClient() for org B = %v, want pgx.ErrNoRows — must not see org A's key", err)
	}
}
