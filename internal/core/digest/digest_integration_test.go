//go:build integration

package digest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
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

// testOrgAgentWorkflow builds a fresh org + one owner user + one agent +
// one workflow, everything BuildPayload/ListOrgsDueForDigest need a
// workflow_run to hang off of.
func testOrgAgentWorkflow(t *testing.T, systemPool *pgxpool.Pool) (orgID, workflowID pgtype.UUID, orgName string) {
	t.Helper()
	suffix := randSuffix(t)
	ctx := context.Background()
	orgName = "Digest Test Org " + suffix

	if err := systemPool.QueryRow(ctx,
		`insert into organizations (clerk_org_id, name, slug) values ($1, $2, $3) returning id`,
		"org_digest_test_"+suffix, orgName, "digest-test-"+suffix,
	).Scan(&orgID); err != nil {
		t.Fatalf("insert test org: %v", err)
	}
	t.Cleanup(func() { _, _ = systemPool.Exec(context.Background(), "delete from organizations where id = $1", orgID) })

	if _, err := systemPool.Exec(ctx,
		`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'digest-owner-test@example.com', 'owner')`,
		orgID, "user_digest_test_"+suffix,
	); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	var agentID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into agents (org_id, name, slug, system_prompt) values ($1, 'Digest Test Agent', $2, 'test') returning id`,
		orgID, "digest-test-agent-"+suffix,
	).Scan(&agentID); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	if err := systemPool.QueryRow(ctx,
		`insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, 'Digest Test Workflow', 'manual', '{}'::jsonb) returning id`,
		orgID, agentID,
	).Scan(&workflowID); err != nil {
		t.Fatalf("insert test workflow: %v", err)
	}

	return orgID, workflowID, orgName
}

func insertRun(t *testing.T, systemPool *pgxpool.Pool, orgID, workflowID pgtype.UUID, status string, hoursSaved float64, createdAt time.Time) pgtype.UUID {
	t.Helper()
	var runID pgtype.UUID
	if err := systemPool.QueryRow(context.Background(),
		`insert into workflow_runs (workflow_id, org_id, status, hours_saved, created_at) values ($1, $2, $3, $4, $5) returning id`,
		workflowID, orgID, status, hoursSaved, createdAt,
	).Scan(&runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return runID
}

func insertCostLedgerEntry(t *testing.T, systemPool *pgxpool.Pool, orgID, runID pgtype.UUID, costUSD float64, createdAt time.Time) {
	t.Helper()
	if _, err := systemPool.Exec(context.Background(),
		`insert into cost_ledger (org_id, run_id, cost_type, estimated_cost_usd, created_at) values ($1, $2, 'llm_call', $3, $4)`,
		orgID, runID, costUSD, createdAt,
	); err != nil {
		t.Fatalf("insert cost_ledger entry: %v", err)
	}
}

// TestBuildPayload_AggregatesYesterdayInOrgTimezone is the load-bearing
// check on the whole feature's core promise: "yesterday" means the
// previous calendar day in the org's own digest_timezone, not a naive
// UTC-minus-24h window, and specialist sub-runs (parent_run_id set) don't
// get double-counted into the top-level run stats.
func TestBuildPayload_AggregatesYesterdayInOrgTimezone(t *testing.T) {
	systemPool := testSystemPool(t)
	q := dbgen.New(systemPool)
	orgID, workflowID, orgName := testOrgAgentWorkflow(t, systemPool)

	const tz = "America/New_York"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	// "Yesterday" in America/New_York, at a wall-clock hour that's a
	// different UTC calendar day -- 11pm ET is 3/4am UTC the *next* day,
	// so a naive UTC-day query would miss this run entirely.
	now := time.Now().In(loc)
	yesterdayLateEvening := time.Date(now.Year(), now.Month(), now.Day()-1, 23, 0, 0, 0, loc)

	run1 := insertRun(t, systemPool, orgID, workflowID, "completed", 1.5, yesterdayLateEvening)
	insertRun(t, systemPool, orgID, workflowID, "failed", 0, yesterdayLateEvening.Add(10*time.Minute))
	insertCostLedgerEntry(t, systemPool, orgID, run1, 2.25, yesterdayLateEvening)

	// A run from 2 days ago must NOT be counted.
	insertRun(t, systemPool, orgID, workflowID, "completed", 99, yesterdayLateEvening.AddDate(0, 0, -1))
	// A specialist sub-run (parent_run_id set) must NOT inflate the
	// top-level count -- insert directly since insertRun has no
	// parent_run_id param.
	if _, err := systemPool.Exec(context.Background(),
		`insert into workflow_runs (workflow_id, org_id, status, hours_saved, created_at, parent_run_id)
		 values ($1, $2, 'completed', 5, $3, $4)`,
		workflowID, orgID, yesterdayLateEvening, run1,
	); err != nil {
		t.Fatalf("insert specialist sub-run: %v", err)
	}

	payload, err := BuildPayload(context.Background(), q, orgID, orgName, tz)
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}

	if payload.TotalRuns != 2 {
		t.Errorf("TotalRuns = %d, want 2 (top-level runs only, excluding the 2-days-ago run and the sub-run)", payload.TotalRuns)
	}
	if payload.SuccessfulRuns != 1 || payload.FailedRuns != 1 {
		t.Errorf("SuccessfulRuns/FailedRuns = %d/%d, want 1/1", payload.SuccessfulRuns, payload.FailedRuns)
	}
	if payload.HoursSaved != 1.5 {
		t.Errorf("HoursSaved = %v, want 1.5 (sub-run's 5h must not be included)", payload.HoursSaved)
	}
	if payload.CostUSD != 2.25 {
		t.Errorf("CostUSD = %v, want 2.25", payload.CostUSD)
	}
	if !payload.HadActivity {
		t.Error("HadActivity = false, want true")
	}
	if payload.TopAgentName != "Digest Test Agent" || payload.TopAgentRuns != 2 {
		t.Errorf("TopAgent = %s (%d runs), want Digest Test Agent (2 runs)", payload.TopAgentName, payload.TopAgentRuns)
	}
}

func TestBuildPayload_NoActivityYesterday(t *testing.T) {
	systemPool := testSystemPool(t)
	q := dbgen.New(systemPool)
	orgID, _, orgName := testOrgAgentWorkflow(t, systemPool)

	payload, err := BuildPayload(context.Background(), q, orgID, orgName, "UTC")
	if err != nil {
		t.Fatalf("BuildPayload: %v", err)
	}
	if payload.HadActivity {
		t.Error("HadActivity = true, want false for an org with no runs at all")
	}
	if payload.TotalRuns != 0 || payload.TopAgentName != "" {
		t.Errorf("expected zero-value stats, got TotalRuns=%d TopAgentName=%q", payload.TotalRuns, payload.TopAgentName)
	}

	subject, textBody, htmlBody, err := RenderEmail(payload, "https://example.com/unsub")
	if err != nil {
		t.Fatalf("RenderEmail: %v", err)
	}
	if !strings.Contains(textBody, "No runs yesterday") {
		t.Errorf("text body missing the no-activity message: %s", textBody)
	}
	if !strings.Contains(htmlBody, "No runs yesterday") {
		t.Error("html body missing the no-activity message")
	}
	if !strings.Contains(subject, payload.Date) {
		t.Errorf("subject = %q, want it to mention %q", subject, payload.Date)
	}
}

// TestListOrgsDueForDigest_GuardsAgainstDoubleSendAndGhostEmails exercises
// the 3 conditions ListOrgsDueForDigest's WHERE clause encodes: hour
// match, not-already-sent-today, and 7-day activity.
func TestListOrgsDueForDigest_GuardsAgainstDoubleSendAndGhostEmails(t *testing.T) {
	systemPool := testSystemPool(t)
	q := dbgen.New(systemPool)
	ctx := context.Background()

	orgID, workflowID, _ := testOrgAgentWorkflow(t, systemPool)
	// Recent activity so the 7-day-quiet guard doesn't exclude it.
	insertRun(t, systemPool, orgID, workflowID, "completed", 1, time.Now())

	currentHourUTC := time.Now().UTC().Hour()

	findsOrg := func(t *testing.T) bool {
		t.Helper()
		due, err := q.ListOrgsDueForDigest(ctx)
		if err != nil {
			t.Fatalf("ListOrgsDueForDigest: %v", err)
		}
		for _, o := range due {
			if o.ID == orgID {
				return true
			}
		}
		return false
	}

	if _, err := systemPool.Exec(ctx,
		`update organizations set digest_enabled = true, digest_send_hour = $2, digest_timezone = 'UTC', digest_last_sent_at = NULL where id = $1`,
		orgID, currentHourUTC,
	); err != nil {
		t.Fatal(err)
	}
	if !findsOrg(t) {
		t.Error("org with matching hour, never sent, recent activity: expected to be due, was not")
	}

	if err := q.MarkDigestSent(ctx, orgID); err != nil {
		t.Fatalf("MarkDigestSent: %v", err)
	}
	if findsOrg(t) {
		t.Error("org already sent today: expected NOT due, was found")
	}

	// Reset the send marker, but now mismatch the hour.
	if _, err := systemPool.Exec(ctx,
		`update organizations set digest_last_sent_at = NULL, digest_send_hour = $2 where id = $1`,
		orgID, (currentHourUTC+5)%24,
	); err != nil {
		t.Fatal(err)
	}
	if findsOrg(t) {
		t.Error("org whose digest_send_hour doesn't match the current hour: expected NOT due, was found")
	}

	// Hour matches again, but the org has been enabled+disabled to prove
	// digest_enabled=false excludes it outright.
	if _, err := systemPool.Exec(ctx,
		`update organizations set digest_send_hour = $2, digest_enabled = false where id = $1`,
		orgID, currentHourUTC,
	); err != nil {
		t.Fatal(err)
	}
	if findsOrg(t) {
		t.Error("org with digest_enabled=false: expected NOT due, was found")
	}
}
