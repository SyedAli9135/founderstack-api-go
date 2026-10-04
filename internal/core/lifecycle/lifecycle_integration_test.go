//go:build integration

package lifecycle

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/core/integrations"
)

func systemPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_SYSTEM_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_SYSTEM_DATABASE_URL not set")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_OWNER_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_OWNER_DATABASE_URL not set")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

type fakeDocs struct {
	pool   *pgxpool.Pool
	purged []uuid.UUID
}

func (f *fakeDocs) Purge(ctx context.Context, org, doc pgtype.UUID) error {
	f.purged = append(f.purged, uuid.UUID(doc.Bytes))
	_, err := f.pool.Exec(ctx, `DELETE FROM documents WHERE id = $1`, doc)
	return err
}

// seedOrg builds a workspace with run history, an approval, audit and cost rows and a document.
func seedOrg(t *testing.T, p *pgxpool.Pool, active bool, deactivatedDaysAgo int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var org, user, agent, wf, run, appr uuid.UUID
	sfx := uuid.NewString()[:8]
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(p.QueryRow(ctx, `INSERT INTO organizations (clerk_org_id, name, slug, is_active, deactivated_at)
		VALUES ($1,'Lifecycle Test',$2,$3, CASE WHEN $3 THEN NULL ELSE now() - make_interval(days => $4) END) RETURNING id`,
		"org_lc_"+sfx, "lc-"+sfx, active, deactivatedDaysAgo).Scan(&org))
	must(p.QueryRow(ctx, `INSERT INTO users (org_id, clerk_user_id, email, role) VALUES ($1,$2,'lc@example.invalid','admin') RETURNING id`, org, "user_lc_"+sfx).Scan(&user))
	must(p.QueryRow(ctx, `INSERT INTO agents (org_id, name, slug, system_prompt, model) VALUES ($1,'A','a','prompt prompt prompt prompt prompt prompt prompt prompt','mock:happy') RETURNING id`, org).Scan(&agent))
	must(p.QueryRow(ctx, `INSERT INTO workflows (org_id, agent_id, name, trigger_type, graph_definition) VALUES ($1,$2,'W','manual','{}'::jsonb) RETURNING id`, org, agent).Scan(&wf))
	must(p.QueryRow(ctx, `INSERT INTO workflow_runs (org_id, workflow_id, status) VALUES ($1,$2,'completed') RETURNING id`, org, wf).Scan(&run))
	_, err := p.Exec(ctx, `INSERT INTO workflow_steps (run_id, node_name, step_type, status) VALUES ($1,'n','planning','completed')`, run)
	must(err)
	must(p.QueryRow(ctx, `INSERT INTO approvals (org_id, run_id, status, expires_at) VALUES ($1,$2,'approved', now()) RETURNING id`, org, run).Scan(&appr))
	_, err = p.Exec(ctx, `INSERT INTO approval_decisions (approval_id, user_id, decision) VALUES ($1,$2,'approve')`, appr, user)
	must(err)
	_, err = p.Exec(ctx, `INSERT INTO audit_logs (org_id, actor_type, action) VALUES ($1,'user','x.y')`, org)
	must(err)
	_, err = p.Exec(ctx, `INSERT INTO cost_ledger (org_id, run_id, cost_type) VALUES ($1,$2,'llm_inference')`, org, run)
	must(err)
	_, err = p.Exec(ctx, `INSERT INTO documents (org_id, filename, s3_path) VALUES ($1,'f.txt','k/f.txt')`, org)
	must(err)
	t.Cleanup(func() { // best effort for leftovers of a failed test
		if active {
			_, _ = p.Exec(ctx, `UPDATE organizations SET is_active=false WHERE id=$1`, org)
		}
		_, _ = p.Exec(ctx, `SELECT purge_organization($1)`, org)
	})
	return org
}

func exists(t *testing.T, p *pgxpool.Pool, table string, org uuid.UUID) bool {
	t.Helper()
	col := "org_id"
	if table == "organizations" {
		col = "id"
	}
	var n int
	if err := p.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func newPurger(p *pgxpool.Pool, docs DocPurger) *Purger {
	return &Purger{System: p, App: p, Docs: docs, Registry: integrations.NewRegistry(), Retain: Retention{730, 730, 180, 30, 90}}
}

func has(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// Selection is tested through dueOrgs and the purge itself through PurgeOrg on
// the seeded workspace only: PurgeDue would also delete any other expired
// workspace sitting in a shared dev database.
func TestDueOrgs_OnlyWorkspacesPastTheRestoreWindow(t *testing.T) {
	p := systemPool(t)
	old := seedOrg(t, p, false, 31)
	recent := seedOrg(t, p, false, 5)
	live := seedOrg(t, p, true, 0)
	pr := newPurger(p, &fakeDocs{pool: p})

	due, err := pr.dueOrgs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !has(due, old) || has(due, recent) || has(due, live) {
		t.Fatalf("due = %v; want the 31-day-old workspace only", due)
	}

	docs := &fakeDocs{pool: p}
	if err := newPurger(p, docs).PurgeOrg(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"organizations", "users", "agents", "workflows", "workflow_runs", "approvals", "audit_logs", "cost_ledger", "documents"} {
		if exists(t, p, table, old) {
			t.Errorf("%s still has rows of the purged workspace", table)
		}
		if !exists(t, p, table, recent) || !exists(t, p, table, live) {
			t.Errorf("%s: rows of another workspace were deleted", table)
		}
	}
	if len(docs.purged) != 1 {
		t.Errorf("documents purged = %d, want 1 (vectors/files go before rows)", len(docs.purged))
	}
}

func TestPurgeOrg_RefusesAnActiveWorkspace(t *testing.T) {
	p := systemPool(t)
	live := seedOrg(t, p, true, 0)
	if err := newPurger(p, &fakeDocs{pool: p}).PurgeOrg(context.Background(), live); err == nil {
		t.Fatal("PurgeOrg deleted an active workspace")
	}
	if !exists(t, p, "organizations", live) {
		t.Fatal("active workspace is gone")
	}
}

func TestPurgeDue_APracticeWaitsForItsClientWorkspaces(t *testing.T) {
	p := systemPool(t)
	ctx := context.Background()
	practice := seedOrg(t, p, false, 40)
	client := seedOrg(t, p, false, 5)
	if _, err := p.Exec(ctx, `UPDATE organizations SET parent_practice_id=$1, organization_type='client_workspace' WHERE id=$2`, practice, client); err != nil {
		t.Fatal(err)
	}
	due, err := newPurger(p, &fakeDocs{pool: p}).dueOrgs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if has(due, practice) {
		t.Fatal("a practice came due while it still had client workspaces")
	}
}

func TestExpireRecords_DeletesOnlyWhatIsPastRetention(t *testing.T) {
	p := systemPool(t)
	ctx := context.Background()
	org := seedOrg(t, p, true, 0)
	own := ownerPool(t) // audit_logs is immutable to the app roles; back-dating needs the owner
	if _, err := own.Exec(ctx, `UPDATE audit_logs SET created_at = now() - interval '800 days' WHERE org_id=$1`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO audit_logs (org_id, actor_type, action) VALUES ($1,'user','recent.row')`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE workflow_steps SET created_at = now() - interval '200 days' WHERE run_id IN (SELECT id FROM workflow_runs WHERE org_id=$1)`, org); err != nil {
		t.Fatal(err)
	}
	newPurger(p, &fakeDocs{pool: p}).ExpireRecords(ctx)

	var old, recent, steps, costs int
	_ = p.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action='x.y'`, org).Scan(&old)
	_ = p.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action='recent.row'`, org).Scan(&recent)
	_ = p.QueryRow(ctx, `SELECT count(*) FROM workflow_steps WHERE run_id IN (SELECT id FROM workflow_runs WHERE org_id=$1)`, org).Scan(&steps)
	_ = p.QueryRow(ctx, `SELECT count(*) FROM cost_ledger WHERE org_id=$1`, org).Scan(&costs)
	if old != 0 || recent != 1 || steps != 0 || costs != 1 {
		t.Fatalf("old audit=%d (want 0) recent audit=%d (want 1) old steps=%d (want 0) fresh cost=%d (want 1)", old, recent, steps, costs)
	}
}
