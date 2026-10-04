//go:build integration

package db

import (
	"context"
	"os"
	"strings"
	"testing"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_SYSTEM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_SYSTEM_DATABASE_URL not set; skipping integration test")
	}
	return dsn
}

func TestNewPool_AppliesTheConnectionCap(t *testing.T) {
	pool, err := NewPool(context.Background(), testDSN(t), 7)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 7 {
		t.Fatalf("MaxConns = %d, want 7", got)
	}
}

// The local dev database is created from the migrations, so its roles do
// accept the repository's dev passwords — exactly what production must catch.
func TestProbeDevCredentials_FindsTheLocalDevRoles(t *testing.T) {
	open, err := ProbeDevCredentials(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("open roles = %v, want both app_user and app_system on a dev database", open)
	}
	if err := RefuseDevCredentials(context.Background(), testDSN(t)); err == nil || !strings.Contains(err.Error(), "ALTER ROLE") {
		t.Fatalf("RefuseDevCredentials = %v, want an error naming the fix", err)
	}
}

func TestProbeDevCredentials_AnUnreachableServerIsNotMistakenForSafe(t *testing.T) {
	// Nothing listens here, so no login succeeds: the probe finds no open role
	// (it can't tell a wrong password from no server — both are "not open").
	open, err := ProbeDevCredentials(context.Background(), "postgresql://x:y@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil || len(open) != 0 {
		t.Fatalf("got (%v, %v), want (none, nil)", open, err)
	}
}

// The roles carry database-enforced timeouts (migration 000026); a new session
// as each role must actually see them.
func TestRolesCarryStatementAndIdleTransactionTimeouts(t *testing.T) {
	for _, tc := range []struct{ envKey, statement string }{
		{"TEST_APP_DATABASE_URL", "30s"},
		{"TEST_SYSTEM_DATABASE_URL", "2min"},
	} {
		dsn := os.Getenv(tc.envKey)
		if dsn == "" {
			t.Skipf("%s not set", tc.envKey)
		}
		pool, err := NewPool(context.Background(), dsn, 2)
		if err != nil {
			t.Fatal(err)
		}
		var statement, idle string
		if err := pool.QueryRow(context.Background(), "show statement_timeout").Scan(&statement); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(context.Background(), "show idle_in_transaction_session_timeout").Scan(&idle); err != nil {
			t.Fatal(err)
		}
		pool.Close()
		if statement != tc.statement {
			t.Errorf("%s statement_timeout = %q, want %q", tc.envKey, statement, tc.statement)
		}
		if idle == "0" {
			t.Errorf("%s idle_in_transaction_session_timeout is unset", tc.envKey)
		}
	}
}
