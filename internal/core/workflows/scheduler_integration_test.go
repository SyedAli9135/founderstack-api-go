//go:build integration

package workflows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/db/dbgen"
)

type launchCall struct {
	orgID, agentID, workflowID, runID uuid.UUID
	input                             string
}

// fakeLauncher records Launch calls instead of executing a run; the real
// execution path behind Launch is covered by internal/core/graph's tests.
type fakeLauncher struct {
	mu           sync.Mutex
	preflightErr error
	launches     []launchCall
}

func (f *fakeLauncher) Preflight(ctx context.Context, orgID pgtype.UUID) error {
	return f.preflightErr
}

func (f *fakeLauncher) Launch(orgID, agentID, workflowID, runID uuid.UUID, input string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, launchCall{orgID, agentID, workflowID, runID, input})
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
	// Runs first: workflow_runs references organizations without ON DELETE
	// CASCADE, so the org cleanup above silently fails while any remain.
	t.Cleanup(func() {
		_, _ = systemPool.Exec(context.Background(), "delete from workflow_runs where org_id = $1", orgID)
	})

	return workflowID
}

func TestTick_FiresDueWorkflowAndAdvancesNextRunAt(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()

	// next_run_at 1 hour in the past — due now.
	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))

	tick(ctx, systemPool, &fakeLauncher{})

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

	tick(ctx, systemPool, &fakeLauncher{})

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

	tick(ctx, systemPool, &fakeLauncher{})

	var runCount int
	err := systemPool.QueryRow(ctx, "select count(*) from workflow_runs where workflow_id = $1", workflowID).Scan(&runCount)
	if err != nil {
		t.Fatal(err)
	}
	if runCount != 0 {
		t.Fatalf("workflow_runs rows for a paused workflow = %d, want 0", runCount)
	}
}

func TestTick_DoesNotFireWorkflowInDeactivatedOrg(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()

	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))
	if _, err := systemPool.Exec(ctx,
		"update organizations set is_active = false where id = (select org_id from workflows where id = $1)", workflowID,
	); err != nil {
		t.Fatal(err)
	}

	tick(ctx, systemPool, &fakeLauncher{})

	var runCount int
	if err := systemPool.QueryRow(ctx, "select count(*) from workflow_runs where workflow_id = $1", workflowID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 0 {
		t.Fatalf("workflow_runs rows for a deactivated org = %d, want 0", runCount)
	}
}

func TestTick_LaunchesTheRunItCreates(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()

	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))
	if _, err := systemPool.Exec(ctx, "update workflows set task_input_template = 'Summarize last week' where id = $1", workflowID); err != nil {
		t.Fatal(err)
	}
	var orgID, agentID pgtype.UUID
	if err := systemPool.QueryRow(ctx, "select org_id, agent_id from workflows where id = $1", workflowID).Scan(&orgID, &agentID); err != nil {
		t.Fatal(err)
	}

	launcher := &fakeLauncher{}
	tick(ctx, systemPool, launcher)

	var runID pgtype.UUID
	if err := systemPool.QueryRow(ctx, "select id from workflow_runs where workflow_id = $1", workflowID).Scan(&runID); err != nil {
		t.Fatalf("run row: %v", err)
	}
	if len(launcher.launches) != 1 {
		t.Fatalf("Launch called %d times, want 1 — a scheduled run must be handed to the launcher, not left pending", len(launcher.launches))
	}
	got := launcher.launches[0]
	want := launchCall{uuid.UUID(orgID.Bytes), uuid.UUID(agentID.Bytes), uuid.UUID(workflowID.Bytes), uuid.UUID(runID.Bytes), "Summarize last week"}
	if got != want {
		t.Fatalf("Launch(%+v), want %+v", got, want)
	}

	// Advanced past this slot, so the next tick doesn't fire it again.
	tick(ctx, systemPool, launcher)
	if len(launcher.launches) != 1 {
		t.Fatalf("Launch called %d times after a second tick, want still 1", len(launcher.launches))
	}
}

func TestTick_PreflightFailureSkipsRunButAdvancesSchedule(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()

	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))
	launcher := &fakeLauncher{preflightErr: graph.ErrNoBYOKKey}
	tick(ctx, systemPool, launcher)

	var runCount int
	var next pgtype.Timestamptz
	if err := systemPool.QueryRow(ctx, "select count(*) from workflow_runs where workflow_id = $1", workflowID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := systemPool.QueryRow(ctx, "select next_run_at from workflows where id = $1", workflowID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if runCount != 0 || len(launcher.launches) != 0 {
		t.Fatalf("runs=%d launches=%d, want no run when preflight fails (same as Run now refusing)", runCount, len(launcher.launches))
	}
	if !next.Valid || !next.Time.After(time.Now()) {
		t.Fatalf("next_run_at = %v, want advanced so a skipped slot isn't retried every tick", next)
	}
}

func TestTick_UnexpectedPreflightErrorRetriesNextTick(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()

	due := time.Now().Add(-time.Hour)
	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", due)
	tick(ctx, systemPool, &fakeLauncher{preflightErr: errors.New("db unavailable")})

	var next pgtype.Timestamptz
	if err := systemPool.QueryRow(ctx, "select next_run_at from workflows where id = $1", workflowID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next.Time.After(time.Now()) {
		t.Fatalf("next_run_at advanced to %v on a transient error, want it left due so the next tick retries", next.Time)
	}
}

// insertRun seeds a run with an explicit updated_at; the BEFORE UPDATE
// trigger would overwrite it on an UPDATE, but not on an INSERT.
func insertRun(t *testing.T, systemPool *pgxpool.Pool, workflowID pgtype.UUID, status string, updatedAt time.Time) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := systemPool.QueryRow(context.Background(),
		`insert into workflow_runs (workflow_id, org_id, status, updated_at)
		 select id, org_id, $2, $3 from workflows where id = $1 returning id`,
		workflowID, status, updatedAt).Scan(&id); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return id
}

func runStatus(t *testing.T, systemPool *pgxpool.Pool, runID pgtype.UUID) string {
	t.Helper()
	var s string
	if err := systemPool.QueryRow(context.Background(), "select status from workflow_runs where id = $1", runID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTick_SkipsWhilePreviousRunInProgress(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()
	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))
	insertRun(t, systemPool, workflowID, "running", time.Now())

	launcher := &fakeLauncher{}
	tick(ctx, systemPool, launcher)

	var runCount int
	var next pgtype.Timestamptz
	_ = systemPool.QueryRow(ctx, "select count(*) from workflow_runs where workflow_id = $1", workflowID).Scan(&runCount)
	_ = systemPool.QueryRow(ctx, "select next_run_at from workflows where id = $1", workflowID).Scan(&next)
	if runCount != 1 || len(launcher.launches) != 0 {
		t.Fatalf("runs=%d launches=%d, want the overlapping firing skipped", runCount, len(launcher.launches))
	}
	if !next.Time.After(time.Now()) {
		t.Fatalf("next_run_at = %v, want advanced past the skipped slot", next.Time)
	}
}

func TestTick_ReapsDeadRunThenFires(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()
	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))
	dead := insertRun(t, systemPool, workflowID, "running", time.Now().Add(-2*time.Hour))

	launcher := &fakeLauncher{}
	tick(ctx, systemPool, launcher)

	if s := runStatus(t, systemPool, dead); s != "failed" {
		t.Fatalf("dead run status = %q, want failed", s)
	}
	if len(launcher.launches) != 1 {
		t.Fatalf("launches = %d, want 1 — a dead run must not block the schedule", len(launcher.launches))
	}
}

func TestTick_AwaitingApprovalIsNeverReapedAndBlocks(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()
	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))
	waiting := insertRun(t, systemPool, workflowID, "awaiting_approval", time.Now().Add(-48*time.Hour))

	launcher := &fakeLauncher{}
	tick(ctx, systemPool, launcher)

	if s := runStatus(t, systemPool, waiting); s != "awaiting_approval" {
		t.Fatalf("waiting run status = %q, want untouched awaiting_approval", s)
	}
	if len(launcher.launches) != 0 {
		t.Fatalf("launches = %d, want 0 while a run waits on approval", len(launcher.launches))
	}
}

func TestTick_ConcurrentTicksFireASlotExactlyOnce(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()
	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))

	launcher := &fakeLauncher{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tick(ctx, systemPool, launcher)
		}()
	}
	close(start)
	wg.Wait()

	var runCount int
	_ = systemPool.QueryRow(ctx, "select count(*) from workflow_runs where workflow_id = $1", workflowID).Scan(&runCount)
	if runCount != 1 || len(launcher.launches) != 1 {
		t.Fatalf("runs=%d launches=%d across 5 concurrent ticks, want exactly 1", runCount, len(launcher.launches))
	}
}

// The claim is what stops a slot firing twice even when the first run has
// already finished (so the in-flight guard can't catch it): a second claimer
// either skips the locked row or, once the first commits, finds it no longer due.
func TestClaimDueScheduledWorkflow_ExclusiveAndRechecksDue(t *testing.T) {
	systemPool := testSystemPool(t)
	ctx := context.Background()
	workflowID := testOrgAgentWorkflow(t, systemPool, "0 9 * * 1", time.Now().Add(-time.Hour))

	txA, err := systemPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback(ctx)
	if _, err := dbgen.New(txA).ClaimDueScheduledWorkflow(ctx, workflowID); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	claimInOwnTx := func() error {
		tx, err := systemPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		// Bounded so a missing SKIP LOCKED shows up as a failure, not a hang.
		if _, err := tx.Exec(ctx, "set local lock_timeout = '2s'"); err != nil {
			t.Fatal(err)
		}
		_, err = dbgen.New(tx).ClaimDueScheduledWorkflow(ctx, workflowID)
		return err
	}

	if err := claimInOwnTx(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim while the first holds it: err = %v, want ErrNoRows (skipped, not blocked)", err)
	}

	next := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if err := dbgen.New(txA).UpdateWorkflowNextRunAt(ctx, dbgen.UpdateWorkflowNextRunAtParams{ID: workflowID, NextRunAt: next}); err != nil {
		t.Fatal(err)
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := claimInOwnTx(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim after the slot was taken: err = %v, want ErrNoRows (no longer due)", err)
	}
}
