//go:build integration

package workflows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// testOrgAgentWorkflow sets up a full chain (org, user, agent, workflow)
// directly via systemPool (BYPASSRLS) — the scheduler runs on app_system,
// not app_user, so this test builds its fixtures the same way.
func testOrgAgentWorkflow(t *testing.T, systemPool *pgxpool.Pool, cronExpr string, nextRunAt time.Time) pgtype.UUID {
	t.Helper()
	suffix := randSuffix(t)
	ctx := context.Background()

	var orgID, userID, agentID, workflowID pgtype.UUID
	err := systemPool.QueryRow(ctx,
		"insert into organizations (clerk_org_id, name, slug) values ($1, 'Scheduler Test Org', $2) returning id",
		"org_scheduler_test_"+suffix, "scheduler-test-"+suffix,
	).Scan(&orgID)
	if err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID)
	})

	err = systemPool.QueryRow(ctx,
		"insert into users (org_id, clerk_user_id, email) values ($1, $2, 'scheduler-test@example.com') returning id",
		orgID, "user_scheduler_test_"+suffix,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	err = systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt, created_by) values ($1, $2, $3, $4, $5) returning id`,
		orgID, "Scheduler Test Agent", "scheduler-test-agent-"+suffix,
		"A test agent used only to exercise the workflow 8 background scheduler.", userID,
	).Scan(&agentID)
	if err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	err = systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition, cron_expression, next_run_at, created_by)
		 values ($1, $2, $3, 'scheduled', '{}'::jsonb, $4, $5, $6) returning id`,
		orgID, agentID, "Scheduler Test Workflow", cronExpr, pgtype.Timestamptz{Time: nextRunAt, Valid: true}, userID,
	).Scan(&workflowID)
	if err != nil {
		t.Fatalf("insert test workflow: %v", err)
	}

	return workflowID
}

func TestTick_FiresDueWorkflowAndAdvancesNextRunAt(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()

	// next_run_at 1 hour in the past — due now.
	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))

	tick(ctx, systemPool)

	var runCount int
	err := systemPool.QueryRow(ctx, "select count(*) from workflow_runs where workflow_id = $1", workflowID).Scan(&runCount)
	if err != nil {
		t.Fatal(err)
	}
	if runCount != 1 {
		t.Fatalf("workflow_runs rows for the due workflow = %d, want 1", runCount)
	}

	var status string
	err = systemPool.QueryRow(ctx, "select status from workflow_runs where workflow_id = $1", workflowID).Scan(&status)
	if err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("run status = %q, want pending", status)
	}

	var nextRunAt pgtype.Timestamptz
	err = systemPool.QueryRow(ctx, "select next_run_at from workflows where id = $1", workflowID).Scan(&nextRunAt)
	if err != nil {
		t.Fatal(err)
	}
	if !nextRunAt.Valid || !nextRunAt.Time.After(time.Now()) {
		t.Fatalf("next_run_at = %v, want a time after now (advanced past the fired run)", nextRunAt)
	}
}

func TestTick_DoesNotFireNotYetDueWorkflow(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()

	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(24*time.Hour))

	tick(ctx, systemPool)

	var runCount int
	err := systemPool.QueryRow(ctx, "select count(*) from workflow_runs where workflow_id = $1", workflowID).Scan(&runCount)
	if err != nil {
		t.Fatal(err)
	}
	if runCount != 0 {
		t.Fatalf("workflow_runs rows for a not-yet-due workflow = %d, want 0", runCount)
	}
}

func TestTick_DoesNotFirePausedWorkflow(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()

	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))
	if _, err := systemPool.Exec(ctx, "update workflows set is_active = false where id = $1", workflowID); err != nil {
		t.Fatal(err)
	}

	tick(ctx, systemPool)

	var runCount int
	err := systemPool.QueryRow(ctx, "select count(*) from workflow_runs where workflow_id = $1", workflowID).Scan(&runCount)
	if err != nil {
		t.Fatal(err)
	}
	if runCount != 0 {
		t.Fatalf("workflow_runs rows for a paused workflow = %d, want 0", runCount)
	}
}
