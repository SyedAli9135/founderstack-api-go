//go:build integration

package practice

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

// fakeProvisioner never calls Clerk — a test must not create real
// organizations in a real Clerk instance.
type fakeProvisioner struct {
	mu        sync.Mutex
	created   []string
	deleted   []string
	createErr error
	// nextID, when set, is returned as the new clerk_org_id instead of a
	// generated one (used to force the local insert to fail).
	nextID string
}

func (f *fakeProvisioner) Create(ctx context.Context, name, createdBy string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	id := f.nextID
	if id == "" {
		id = "org_practice_ws_" + randSuffix()
	}
	f.created = append(f.created, id)
	return id, nil
}

func (f *fakeProvisioner) Delete(ctx context.Context, clerkOrgID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, clerkOrgID)
	return nil
}

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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

var testCfg = &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}

func testRouter(pool *pgxpool.Pool, prov WorkspaceProvisioner) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	h := NewHandler(pool, prov)
	g := r.Group("/api/v1")
	g.Use(middleware.RequireAuth(pool, testCfg))
	h.Register(g)
	idg := r.Group("/api/v1")
	idg.Use(middleware.RequireIdentity(testCfg))
	h.RegisterIdentityOnly(idg)
	return r
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func call(t *testing.T, r *gin.Engine, clerkUserID, clerkOrgID, method, path string, body any) (int, envelope) {
	t.Helper()
	token, err := devtoken.SignForOrg(testCfg.DevTokenSecret.Expose(), clerkUserID, clerkOrgID)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var env envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env
}

// fixture: a standard org (the future practice) with an admin operator and
// a plain-member assistant; plus a second, unrelated practice with its own
// admin, to prove cross-practice isolation.
type fixture struct {
	pool                         *pgxpool.Pool
	practiceID                   pgtype.UUID
	practiceClerkID              string
	operator, assistant          string
	otherPracticeClerkID, outsid string
}

func newFixture(t *testing.T, pool *pgxpool.Pool) *fixture {
	t.Helper()
	s := randSuffix()
	fx := &fixture{
		pool: pool, practiceClerkID: "org_practice_" + s, operator: "user_practice_op_" + s,
		assistant: "user_practice_asst_" + s, otherPracticeClerkID: "org_practice_other_" + s,
		outsid: "user_practice_outsider_" + s,
	}
	fx.practiceID = fx.insertOrg(t, fx.practiceClerkID, "Operator Practice", "practice-"+s)
	fx.insertUser(t, fx.practiceID, fx.operator, "admin")
	fx.insertUser(t, fx.practiceID, fx.assistant, "member")
	other := fx.insertOrg(t, fx.otherPracticeClerkID, "Other Practice", "practice-other-"+s)
	fx.insertUser(t, other, fx.outsid, "admin")

	// Registered first so it runs last: removes every client workspace the
	// test created under either practice, after their seeded runs are gone.
	t.Cleanup(func() {
		ctx := context.Background()
		for _, parent := range []pgtype.UUID{fx.practiceID, other} {
			_, _ = pool.Exec(ctx, "delete from organizations where parent_practice_id = $1", parent)
		}
	})
	return fx
}

func (fx *fixture) insertOrg(t *testing.T, clerkID, name, slug string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := fx.pool.QueryRow(context.Background(),
		"insert into organizations (clerk_org_id, name, slug) values ($1, $2, $3) returning id",
		clerkID, name, slug).Scan(&id); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), "delete from organizations where parent_practice_id = $1", id)
		_, _ = fx.pool.Exec(context.Background(), "delete from organizations where id = $1", id)
	})
	return id
}

func (fx *fixture) insertUser(t *testing.T, orgID pgtype.UUID, clerkUserID, role string) {
	t.Helper()
	if _, err := fx.pool.Exec(context.Background(),
		"insert into users (org_id, clerk_user_id, email, full_name, role) values ($1, $2, $3, 'Test Person', $4)",
		orgID, clerkUserID, clerkUserID+"@example.com", role); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

type createdWorkspace struct {
	ID         string `json:"id"`
	ClerkOrgID string `json:"clerk_org_id"`
	Status     string `json:"status"`
}

func (fx *fixture) create(t *testing.T, r *gin.Engine, name string) createdWorkspace {
	t.Helper()
	code, env := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, "/api/v1/practice/client-workspaces",
		map[string]any{"name": name, "client_contact_email": "ceo@" + strings.ToLower(name) + ".example.com"})
	if code != http.StatusCreated {
		t.Fatalf("create %s: status = %d, code = %s", name, code, env.Error.Code)
	}
	var ws createdWorkspace
	if err := json.Unmarshal(env.Data, &ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

// seedActivity gives a workspace one running run, one pending approval,
// and $1.25 of cost — cleaned up in FK order (workflow_runs/approvals/
// cost_ledger.run_id don't cascade from organizations).
func (fx *fixture) seedActivity(t *testing.T, orgID string, hoursSaved float64) {
	t.Helper()
	ctx := context.Background()
	s := randSuffix()
	var agentID, workflowID, runID pgtype.UUID
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed activity: %v", err)
		}
	}
	must(fx.pool.QueryRow(ctx, `insert into agents (org_id, name, slug, system_prompt) values ($1, 'Practice Agent', $2, 'test') returning id`,
		orgID, "practice-agent-"+s).Scan(&agentID))
	must(fx.pool.QueryRow(ctx, `insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, 'Practice WF', 'manual', '{}') returning id`,
		orgID, agentID).Scan(&workflowID))
	must(fx.pool.QueryRow(ctx, `insert into workflow_runs (workflow_id, org_id, status) values ($1, $2, 'running') returning id`,
		workflowID, orgID).Scan(&runID))
	_, err := fx.pool.Exec(ctx, `insert into approvals (run_id, org_id, status) values ($1, $2, 'pending')`, runID, orgID)
	must(err)
	_, err = fx.pool.Exec(ctx, `insert into cost_ledger (org_id, run_id, cost_type, estimated_cost_usd) values ($1, $2, 'llm', 1.25)`, orgID, runID)
	must(err)
	_, err = fx.pool.Exec(ctx, `update organizations set total_hours_saved = $2 where id = $1`, orgID, hoursSaved)
	must(err)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = fx.pool.Exec(ctx, "delete from cost_ledger where org_id = $1", orgID)
		_, _ = fx.pool.Exec(ctx, "delete from approvals where org_id = $1", orgID)
		_, _ = fx.pool.Exec(ctx, "delete from workflow_runs where org_id = $1", orgID)
		_, _ = fx.pool.Exec(ctx, "delete from workflows where org_id = $1", orgID)
		_, _ = fx.pool.Exec(ctx, "delete from agents where org_id = $1", orgID)
	})
}

type listResponse struct {
	Practice struct {
		OrganizationType       string `json:"organization_type"`
		MaxClientWorkspaces    int    `json:"max_client_workspaces"`
		ActiveClientWorkspaces int    `json:"active_client_workspaces"`
	} `json:"practice"`
	Workspaces []struct {
		ID                 string  `json:"id"`
		Name               string  `json:"name"`
		Status             string  `json:"status"`
		ClientContactEmail *string `json:"client_contact_email"`
		RestorableUntil    *string `json:"restorable_until"`
		Stats              struct {
			HoursSaved       float64 `json:"hours_saved"`
			ActiveRuns       int64   `json:"active_runs"`
			PendingApprovals int64   `json:"pending_approvals"`
			TotalCostUSD     float64 `json:"total_cost_usd"`
		} `json:"stats"`
	} `json:"workspaces"`
}

func list(t *testing.T, r *gin.Engine, clerkUserID, clerkOrgID string) (int, listResponse) {
	t.Helper()
	code, env := call(t, r, clerkUserID, clerkOrgID, http.MethodGet, "/api/v1/practice/client-workspaces", nil)
	var out listResponse
	_ = json.Unmarshal(env.Data, &out)
	return code, out
}

func TestPractice_CreateListAndSummary(t *testing.T) {
	pool := testSystemPool(t)
	fx := newFixture(t, pool)
	prov := &fakeProvisioner{}
	r := testRouter(pool, prov)

	a := fx.create(t, r, "Acme")
	b := fx.create(t, r, "Globex")
	fx.seedActivity(t, a.ID, 12.5)

	t.Run("creating the first workspace promotes the org to a practice", func(t *testing.T) {
		var typ string
		if err := pool.QueryRow(context.Background(), "select organization_type from organizations where id = $1", fx.practiceID).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		if typ != "practice" {
			t.Fatalf("organization_type = %q, want practice", typ)
		}
	})

	t.Run("each workspace is its own org with the parent set and the operator as admin", func(t *testing.T) {
		for _, ws := range []createdWorkspace{a, b} {
			var typ, role string
			var parent pgtype.UUID
			err := pool.QueryRow(context.Background(),
				`select o.organization_type, o.parent_practice_id, u.role from organizations o
				 join users u on u.org_id = o.id and u.clerk_user_id = $2 where o.id = $1`,
				ws.ID, fx.operator).Scan(&typ, &parent, &role)
			if err != nil {
				t.Fatalf("workspace %s: %v", ws.ID, err)
			}
			if typ != "client_workspace" || parent != fx.practiceID || role != "admin" {
				t.Fatalf("workspace = (%s, %s, %s), want client_workspace under practice with admin operator", typ, parent.String(), role)
			}
		}
	})

	t.Run("the operator can immediately act inside a new workspace", func(t *testing.T) {
		code, env := call(t, r, fx.operator, a.ClerkOrgID, http.MethodGet, "/api/v1/me/workspaces", nil)
		if code != http.StatusOK {
			t.Fatalf("status = %d (%s)", code, env.Error.Code)
		}
		var out struct {
			Workspaces []struct {
				ID               string `json:"id"`
				OrganizationType string `json:"organization_type"`
				IsCurrent        bool   `json:"is_current"`
			} `json:"workspaces"`
		}
		_ = json.Unmarshal(env.Data, &out)
		if len(out.Workspaces) != 3 || out.Workspaces[0].OrganizationType != "practice" {
			t.Fatalf("workspaces = %+v, want practice first, then 2 client workspaces", out.Workspaces)
		}
		for _, w := range out.Workspaces {
			if w.IsCurrent != (w.ID == a.ID) {
				t.Fatalf("is_current wrong on %+v, want only %s current", w, a.ID)
			}
		}
	})

	t.Run("list reports per-workspace stats", func(t *testing.T) {
		code, out := list(t, r, fx.operator, fx.practiceClerkID)
		if code != http.StatusOK || len(out.Workspaces) != 2 {
			t.Fatalf("list = (%d, %+v)", code, out)
		}
		if out.Practice.ActiveClientWorkspaces != 2 || out.Practice.MaxClientWorkspaces != 5 {
			t.Fatalf("practice = %+v", out.Practice)
		}
		acme := out.Workspaces[0]
		if acme.Name != "Acme" || acme.Stats.ActiveRuns != 1 || acme.Stats.PendingApprovals != 1 ||
			acme.Stats.TotalCostUSD != 1.25 || acme.Stats.HoursSaved != 12.5 {
			t.Fatalf("acme = %+v", acme)
		}
		if acme.ClientContactEmail == nil || *acme.ClientContactEmail != "ceo@acme.example.com" {
			t.Fatalf("client_contact_email = %v", acme.ClientContactEmail)
		}
		if g := out.Workspaces[1]; g.Stats.ActiveRuns != 0 || g.Stats.TotalCostUSD != 0 {
			t.Fatalf("globex should have no activity, got %+v", g.Stats)
		}
	})

	t.Run("the practice resolves the same from inside a client workspace", func(t *testing.T) {
		code, out := list(t, r, fx.operator, b.ClerkOrgID)
		if code != http.StatusOK || len(out.Workspaces) != 2 {
			t.Fatalf("list from workspace = (%d, %d workspaces)", code, len(out.Workspaces))
		}
	})

	t.Run("portfolio summary rolls up across active workspaces", func(t *testing.T) {
		code, env := call(t, r, fx.operator, fx.practiceClerkID, http.MethodGet, "/api/v1/practice/portfolio-summary", nil)
		var s map[string]float64
		_ = json.Unmarshal(env.Data, &s)
		if code != http.StatusOK || s["active_workspaces"] != 2 || s["hours_saved"] != 12.5 ||
			s["active_runs"] != 1 || s["pending_approvals"] != 1 || s["total_cost_usd"] != 1.25 {
			t.Fatalf("summary = (%d, %v)", code, s)
		}
	})
}

func TestPractice_AccessControl(t *testing.T) {
	pool := testSystemPool(t)
	fx := newFixture(t, pool)
	prov := &fakeProvisioner{}
	r := testRouter(pool, prov)
	a := fx.create(t, r, "Initech")

	t.Run("a practice member who isn't owner/admin cannot create", func(t *testing.T) {
		code, env := call(t, r, fx.assistant, fx.practiceClerkID, http.MethodPost, "/api/v1/practice/client-workspaces", map[string]any{"name": "Nope"})
		if code != http.StatusForbidden || env.Error.Code != "NOT_AUTHORIZED" {
			t.Fatalf("got (%d, %s), want 403 NOT_AUTHORIZED", code, env.Error.Code)
		}
	})

	t.Run("a practice member only sees workspaces they belong to", func(t *testing.T) {
		code, out := list(t, r, fx.assistant, fx.practiceClerkID)
		if code != http.StatusOK || len(out.Workspaces) != 0 {
			t.Fatalf("assistant list = (%d, %d workspaces), want 200 with none", code, len(out.Workspaces))
		}
	})

	t.Run("client-side staff inside one workspace get nothing over the practice", func(t *testing.T) {
		clientStaff := "user_practice_client_staff_" + randSuffix()
		var wsID pgtype.UUID
		_ = wsID.Scan(a.ID)
		fx.insertUser(t, wsID, clientStaff, "admin")
		for _, path := range []string{"/api/v1/practice/client-workspaces", "/api/v1/practice/portfolio-summary"} {
			code, env := call(t, r, clientStaff, a.ClerkOrgID, http.MethodGet, path, nil)
			if code != http.StatusForbidden || env.Error.Code != "NOT_A_PRACTICE_MEMBER" {
				t.Fatalf("%s: got (%d, %s), want 403 NOT_A_PRACTICE_MEMBER", path, code, env.Error.Code)
			}
		}
	})

	t.Run("another practice's admin can't touch or see this practice's workspace", func(t *testing.T) {
		code, env := call(t, r, fx.outsid, fx.otherPracticeClerkID, http.MethodDelete, "/api/v1/practice/client-workspaces/"+a.ID, nil)
		if code != http.StatusNotFound {
			t.Fatalf("delete by outsider: got (%d, %s), want 404", code, env.Error.Code)
		}
		code, out := list(t, r, fx.outsid, fx.otherPracticeClerkID)
		if code != http.StatusOK || len(out.Workspaces) != 0 {
			t.Fatalf("outsider list = (%d, %d workspaces), want none", code, len(out.Workspaces))
		}
		code, _ = call(t, r, fx.outsid, a.ClerkOrgID, http.MethodGet, "/api/v1/practice/portfolio-summary", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("outsider claiming the workspace as active org: status = %d, want 401", code)
		}
	})

	t.Run("a client workspace can't itself create workspaces under it", func(t *testing.T) {
		code, _ := call(t, r, fx.operator, a.ClerkOrgID, http.MethodPost, "/api/v1/practice/client-workspaces", map[string]any{"name": "Nested"})
		if code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (created under the parent practice)", code)
		}
		var nested int
		_ = pool.QueryRow(context.Background(), "select count(*) from organizations where parent_practice_id = $1", a.ID).Scan(&nested)
		if nested != 0 {
			t.Fatalf("%d workspaces nested under a client workspace, want 0", nested)
		}
	})

	t.Run("invalid input is rejected before anything is provisioned", func(t *testing.T) {
		before := len(prov.created)
		for _, body := range []map[string]any{{"name": "  "}, {"name": "Ok", "client_contact_email": "not-an-email"}} {
			code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, "/api/v1/practice/client-workspaces", body)
			if code != http.StatusBadRequest {
				t.Fatalf("body %v: status = %d, want 400", body, code)
			}
		}
		if len(prov.created) != before {
			t.Fatal("a Clerk org was provisioned for an invalid request")
		}
	})
}

func TestPractice_LimitAndProvisioningRollback(t *testing.T) {
	pool := testSystemPool(t)
	fx := newFixture(t, pool)
	prov := &fakeProvisioner{}
	r := testRouter(pool, prov)
	if _, err := pool.Exec(context.Background(), "update organizations set max_client_workspaces = 1 where id = $1", fx.practiceID); err != nil {
		t.Fatal(err)
	}
	fx.create(t, r, "Hooli")

	t.Run("at the limit returns 402 without provisioning a Clerk org", func(t *testing.T) {
		before := len(prov.created)
		code, env := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, "/api/v1/practice/client-workspaces", map[string]any{"name": "Pied Piper"})
		if code != http.StatusPaymentRequired || env.Error.Code != "CLIENT_WORKSPACE_LIMIT_REACHED" {
			t.Fatalf("got (%d, %s), want 402", code, env.Error.Code)
		}
		if len(prov.created) != before {
			t.Fatal("Clerk org provisioned despite the limit")
		}
	})

	t.Run("a failed local insert deletes the Clerk org it just created", func(t *testing.T) {
		_, _ = pool.Exec(context.Background(), "update organizations set max_client_workspaces = 5 where id = $1", fx.practiceID)
		prov.nextID = "org_" + strings.Repeat("x", 300) // exceeds varchar(255)
		defer func() { prov.nextID = "" }()
		code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, "/api/v1/practice/client-workspaces", map[string]any{"name": "Doomed"})
		if code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", code)
		}
		if len(prov.deleted) != 1 || prov.deleted[0] != prov.nextID {
			t.Fatalf("deleted = %v, want the orphaned Clerk org rolled back", prov.deleted)
		}
	})

	t.Run("a Clerk failure surfaces as 502 and writes nothing", func(t *testing.T) {
		prov.createErr = errors.New("clerk down")
		defer func() { prov.createErr = nil }()
		var before int
		_ = pool.QueryRow(context.Background(), "select count(*) from organizations where parent_practice_id = $1", fx.practiceID).Scan(&before)
		code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, "/api/v1/practice/client-workspaces", map[string]any{"name": "Nope"})
		var after int
		_ = pool.QueryRow(context.Background(), "select count(*) from organizations where parent_practice_id = $1", fx.practiceID).Scan(&after)
		if code != http.StatusBadGateway || after != before {
			t.Fatalf("got (%d, %d->%d rows), want 502 and no new row", code, before, after)
		}
	})
}

func TestPractice_RemoveAndRestore(t *testing.T) {
	pool := testSystemPool(t)
	fx := newFixture(t, pool)
	r := testRouter(pool, &fakeProvisioner{})
	a := fx.create(t, r, "Umbrella")
	b := fx.create(t, r, "Soylent")
	fx.seedActivity(t, a.ID, 3)
	path := "/api/v1/practice/client-workspaces/" + a.ID

	t.Run("a non-admin practice member cannot remove", func(t *testing.T) {
		code, _ := call(t, r, fx.assistant, fx.practiceClerkID, http.MethodDelete, path, nil)
		if code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", code)
		}
	})

	t.Run("remove soft-deactivates and keeps the data", func(t *testing.T) {
		code, env := call(t, r, fx.operator, fx.practiceClerkID, http.MethodDelete, path, nil)
		if code != http.StatusOK {
			t.Fatalf("status = %d (%s)", code, env.Error.Code)
		}
		var active bool
		var runs int
		_ = pool.QueryRow(context.Background(), "select is_active from organizations where id = $1", a.ID).Scan(&active)
		_ = pool.QueryRow(context.Background(), "select count(*) from workflow_runs where org_id = $1", a.ID).Scan(&runs)
		if active || runs != 1 {
			t.Fatalf("after remove: is_active=%v runs=%d, want inactive with data retained", active, runs)
		}
	})

	t.Run("a removed workspace can no longer be used as the active org", func(t *testing.T) {
		code, env := call(t, r, fx.operator, a.ClerkOrgID, http.MethodGet, "/api/v1/practice/portfolio-summary", nil)
		if code != http.StatusNotFound || env.Error.Code != "ORGANIZATION_NOT_FOUND" {
			t.Fatalf("got (%d, %s), want 404 ORGANIZATION_NOT_FOUND", code, env.Error.Code)
		}
	})

	t.Run("with the removed workspace still active in the session, the switcher list still works", func(t *testing.T) {
		code, env := call(t, r, fx.operator, a.ClerkOrgID, http.MethodGet, "/api/v1/me/workspaces", nil)
		if code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200 so the client can recover", code, env.Error.Code)
		}
		var out struct {
			Workspaces []struct {
				ID        string `json:"id"`
				IsCurrent bool   `json:"is_current"`
			} `json:"workspaces"`
		}
		_ = json.Unmarshal(env.Data, &out)
		if len(out.Workspaces) != 2 {
			t.Fatalf("workspaces = %+v, want the practice and the remaining workspace", out.Workspaces)
		}
		for _, w := range out.Workspaces {
			if w.ID == a.ID || w.IsCurrent {
				t.Fatalf("workspace %+v listed as usable/current, want the removed one excluded and none current", w)
			}
		}
	})

	t.Run("list shows it as restorable; summary excludes it", func(t *testing.T) {
		_, out := list(t, r, fx.operator, fx.practiceClerkID)
		var found bool
		for _, w := range out.Workspaces {
			if w.ID == a.ID {
				found = true
				if w.Status != "deactivated" || w.RestorableUntil == nil {
					t.Fatalf("removed workspace = %+v, want deactivated with restorable_until", w)
				}
			}
		}
		if !found || out.Practice.ActiveClientWorkspaces != 1 {
			t.Fatalf("list = %+v", out)
		}
		_, env := call(t, r, fx.operator, fx.practiceClerkID, http.MethodGet, "/api/v1/practice/portfolio-summary", nil)
		var s map[string]float64
		_ = json.Unmarshal(env.Data, &s)
		if s["active_workspaces"] != 1 || s["active_runs"] != 0 || s["total_cost_usd"] != 0 {
			t.Fatalf("summary = %v, want only the remaining active workspace counted", s)
		}
	})

	t.Run("removing again is idempotent", func(t *testing.T) {
		if code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodDelete, path, nil); code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	})

	t.Run("restore inside the window reactivates it", func(t *testing.T) {
		code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, path+"/restore", nil)
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if code, _ := call(t, r, fx.operator, a.ClerkOrgID, http.MethodGet, "/api/v1/practice/portfolio-summary", nil); code != http.StatusOK {
			t.Fatalf("restored workspace still unusable: status = %d", code)
		}
	})

	t.Run("restore past 30 days is refused", func(t *testing.T) {
		bPath := "/api/v1/practice/client-workspaces/" + b.ID
		if code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodDelete, bPath, nil); code != http.StatusOK {
			t.Fatalf("remove: %d", code)
		}
		if _, err := pool.Exec(context.Background(), "update organizations set deactivated_at = now() - interval '31 days' where id = $1", b.ID); err != nil {
			t.Fatal(err)
		}
		code, env := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, bPath+"/restore", nil)
		if code != http.StatusGone || env.Error.Code != "RESTORE_WINDOW_EXPIRED" {
			t.Fatalf("got (%d, %s), want 410 RESTORE_WINDOW_EXPIRED", code, env.Error.Code)
		}
		// Even at the limit, expiry is the reason reported — it's the permanent one.
		_, _ = pool.Exec(context.Background(), "update organizations set max_client_workspaces = 1 where id = $1", fx.practiceID)
		code, env = call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, bPath+"/restore", nil)
		if code != http.StatusGone {
			t.Fatalf("expired restore at the limit: got (%d, %s), want 410", code, env.Error.Code)
		}
		_, out := list(t, r, fx.operator, fx.practiceClerkID)
		for _, w := range out.Workspaces {
			if w.ID == b.ID && w.Status != "expired" {
				t.Fatalf("status = %s, want expired", w.Status)
			}
		}
	})

	t.Run("an unknown id is a 404", func(t *testing.T) {
		code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodDelete,
			fmt.Sprintf("/api/v1/practice/client-workspaces/%s", "00000000-0000-0000-0000-000000000000"), nil)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	})
}
