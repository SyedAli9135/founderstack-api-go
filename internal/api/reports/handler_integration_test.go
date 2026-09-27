//go:build integration

package reports

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

var testCfg = &config.Config{AppEnv: "development", DevTokenSecret: "test-dev-token-secret"}

func pool(t *testing.T, env string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skip(env + " not set; skipping integration test")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func suffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type allowAll struct{}

func (allowAll) Allow(context.Context, string) bool { return true }

type denyAll struct{}

func (denyAll) Allow(context.Context, string) bool { return false }

func router(appPool, sysPool *pgxpool.Pool, limiter RateLimiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Mirrors cmd/api: trust no proxy, so X-Forwarded-For can't be spoofed.
	_ = r.SetTrustedProxies(nil)
	r.Use(middleware.RequestID())
	h := NewHandler(appPool, sysPool, limiter)
	g := r.Group("/api/v1")
	g.Use(middleware.RequireAuth(sysPool, testCfg))
	h.Register(g)
	h.RegisterPublic(r.Group("/api/public"))
	return r
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func do(t *testing.T, r *gin.Engine, clerkUserID, method, path string, body any, headers ...string) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if clerkUserID != "" {
		tok, err := devtoken.Sign(testCfg.DevTokenSecret.Expose(), clerkUserID)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var env envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec, env
}

type fixture struct {
	orgID, otherOrgID string
	admin, member     string
	outsider          string
}

// newFixture: an org (Asia/Karachi timezone, UTC+5, no DST) with an admin
// and a member, and runs + costs placed around the September boundary; plus
// an unrelated org with its own run that must never appear.
func newFixture(t *testing.T, sys *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	s := suffix()
	fx := fixture{admin: "user_rep_admin_" + s, member: "user_rep_member_" + s, outsider: "user_rep_out_" + s}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := sys.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q[:40], err)
		}
	}
	newOrg := func(name string) string {
		var id string
		if err := sys.QueryRow(ctx, `insert into organizations (clerk_org_id, name, slug, digest_timezone) values ($1, $2, $3, 'Asia/Karachi') returning id`,
			"org_rep_"+name+"_"+s, "Report "+name, "rep-"+name+"-"+s).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c := context.Background()
			for _, q := range []string{
				"delete from client_reports where org_id = $1", "delete from audit_logs where org_id = $1",
				"delete from cost_ledger where org_id = $1", "delete from workflow_runs where org_id = $1",
				"delete from workflows where org_id = $1", "delete from agents where org_id = $1",
				"delete from organizations where id = $1",
			} {
				_, _ = sys.Exec(c, q, id)
			}
		})
		return id
	}
	fx.orgID = newOrg("client")
	fx.otherOrgID = newOrg("other")
	mustExec(`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'a@example.com', 'admin')`, fx.orgID, fx.admin)
	mustExec(`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'm@example.com', 'member')`, fx.orgID, fx.member)
	mustExec(`insert into users (org_id, clerk_user_id, email, role) values ($1, $2, 'o@example.com', 'admin')`, fx.otherOrgID, fx.outsider)

	seed := func(org, workflowName string, runs []struct {
		at     string
		status string
		hours  float64
		cost   float64
	}) {
		var agentID, wfID string
		if err := sys.QueryRow(ctx, `insert into agents (org_id, name, slug, system_prompt) values ($1, $2, $3, 'x') returning id`,
			org, workflowName+" Agent", "agent-"+suffix()).Scan(&agentID); err != nil {
			t.Fatal(err)
		}
		if err := sys.QueryRow(ctx, `insert into workflows (org_id, agent_id, name, trigger_type, graph_definition) values ($1, $2, $3, 'manual', '{}') returning id`,
			org, agentID, workflowName).Scan(&wfID); err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			var runID string
			if err := sys.QueryRow(ctx, `insert into workflow_runs (workflow_id, org_id, status, created_at, started_at, completed_at, hours_saved)
				values ($1, $2, $3, $4, $4, $4, $5) returning id`, wfID, org, r.status, r.at, r.hours).Scan(&runID); err != nil {
				t.Fatal(err)
			}
			mustExec(`insert into cost_ledger (org_id, run_id, agent_id, cost_type, input_tokens, output_tokens, estimated_cost_usd, created_at)
				values ($1, $2, $3, 'llm', 1000, 200, $4, $5)`, org, runID, agentID, r.cost, r.at)
		}
	}
	type run = struct {
		at     string
		status string
		hours  float64
		cost   float64
	}
	seed(fx.orgID, "Weekly Close", []run{
		{"2026-09-03T10:00:00Z", "completed", 1.5, 0.40},
		{"2026-09-17T10:00:00Z", "completed", 1.5, 0.60},
		{"2026-09-18T10:00:00Z", "failed", 0, 0.10},
		// 20:00 UTC Sep 30 is 01:00 Oct 1 in Karachi: outside a September report.
		{"2026-09-30T20:00:00Z", "completed", 9, 9.00},
		// 22:00 UTC Aug 31 is 03:00 Sep 1 in Karachi: inside it.
		{"2026-08-31T22:00:00Z", "completed", 0.5, 0.05},
	})
	seed(fx.otherOrgID, "Other Client Secret Workflow", []run{{"2026-09-10T10:00:00Z", "completed", 50, 99}})
	return fx
}

type created struct {
	ID         string          `json:"id"`
	ShareToken string          `json:"share_token"`
	SharePath  string          `json:"share_path"`
	Status     string          `json:"status"`
	Snapshot   json.RawMessage `json:"snapshot"`
}

func TestReports_EndToEnd(t *testing.T) {
	app, sys := pool(t, "TEST_APP_DATABASE_URL"), pool(t, "TEST_SYSTEM_DATABASE_URL")
	fx := newFixture(t, sys)
	r := router(app, sys, allowAll{})
	ctx := context.Background()
	september := map[string]any{"org_id": fx.orgID, "date_from": "2026-09-01", "date_to": "2026-09-30"}

	var outcomesOnly, full created
	t.Run("default report shows outcomes only, and stores nothing hidden", func(t *testing.T) {
		start := time.Now()
		rec, env := do(t, r, fx.admin, http.MethodPost, "/api/v1/reports", september)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, env.Error.Code)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("generation took %v, want < 5s", d)
		}
		_ = json.Unmarshal(env.Data, &outcomesOnly)
		var snap Snapshot
		_ = json.Unmarshal(outcomesOnly.Snapshot, &snap)
		// Sep 1 (Karachi) through Sep 30: 4 runs (3 completed, 1 failed); the Oct 1 one excluded.
		if snap.Summary.RunsTotal != 4 || snap.Summary.RunsCompleted != 3 || snap.Summary.RunsFailed != 1 || snap.Summary.HoursSaved != 3.5 {
			t.Fatalf("summary = %+v, want 4 total / 3 completed / 1 failed / 3.5h", snap.Summary)
		}
		if snap.Timezone != "Asia/Karachi" || len(snap.Workflows) != 1 || snap.Workflows[0].Name != "Weekly Close" {
			t.Fatalf("snapshot = %+v", snap)
		}
		var stored string
		_ = sys.QueryRow(ctx, "select snapshot::text from client_reports where id = $1", outcomesOnly.ID).Scan(&stored)
		for _, hidden := range []string{`"cost"`, `"tokens"`, `"runs"`, "total_usd", "cost_usd"} {
			if strings.Contains(stored, hidden) {
				t.Fatalf("stored snapshot contains hidden section %s: %s", hidden, stored)
			}
		}
		if strings.Contains(stored, "Other Client Secret Workflow") {
			t.Fatal("another tenant's data leaked into the report")
		}
		if len(outcomesOnly.ShareToken) != 32 || !strings.HasPrefix(outcomesOnly.SharePath, "/reports/") {
			t.Fatalf("share token/path = %q %q", outcomesOnly.ShareToken, outcomesOnly.SharePath)
		}
	})

	t.Run("opted-in sections carry cost, tokens and runs", func(t *testing.T) {
		body := map[string]any{"org_id": fx.orgID, "date_from": "2026-09-01", "date_to": "2026-09-30",
			"visible_sections": map[string]bool{"cost": true, "tokens": true, "runs": true}, "expires_in_days": 7, "title": "September for Acme"}
		rec, env := do(t, r, fx.admin, http.MethodPost, "/api/v1/reports", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, env.Error.Code)
		}
		_ = json.Unmarshal(env.Data, &full)
		var snap Snapshot
		_ = json.Unmarshal(full.Snapshot, &snap)
		if snap.Cost == nil || snap.Cost.TotalUSD < 1.149 || snap.Cost.TotalUSD > 1.151 {
			t.Fatalf("cost = %+v, want $1.15 (0.05+0.40+0.60+0.10)", snap.Cost)
		}
		if snap.Tokens == nil || snap.Tokens.Input != 4000 || snap.Tokens.Output != 800 {
			t.Fatalf("tokens = %+v", snap.Tokens)
		}
		if len(snap.Runs) != 4 || snap.Runs[0].CostUSD == nil {
			t.Fatalf("runs = %+v", snap.Runs)
		}
		var expires time.Time
		_ = sys.QueryRow(ctx, "select expires_at from client_reports where id = $1", full.ID).Scan(&expires)
		if d := time.Until(expires); d < 6*24*time.Hour || d > 8*24*time.Hour {
			t.Fatalf("expires in %v, want ~7 days", d)
		}
	})

	t.Run("the public link works with no login and counts views", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			rec, env := do(t, r, "", http.MethodGet, "/api/public/reports/"+outcomesOnly.ShareToken, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("public view: %d %s", rec.Code, env.Error.Code)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
			}
			// Neither hidden data nor the fact that something was hidden.
			for _, leak := range []string{"total_usd", "cost_usd", `"tokens"`, "visible_sections", `"cost"`} {
				if strings.Contains(string(env.Data), leak) {
					t.Fatalf("public payload contains %s: %s", leak, env.Data)
				}
			}
		}
		var views int
		var last pgtype.Timestamptz
		_ = sys.QueryRow(ctx, "select view_count, last_viewed_at from client_reports where id = $1", outcomesOnly.ID).Scan(&views, &last)
		if views != 2 || !last.Valid {
			t.Fatalf("views = %d, last viewed %v; want 2 and set", views, last)
		}
		// The operator's own preview is not a client view.
		if rec, _ := do(t, r, fx.admin, http.MethodGet, "/api/v1/reports/"+outcomesOnly.ID, nil); rec.Code != http.StatusOK {
			t.Fatalf("preview: %d", rec.Code)
		}
		_ = sys.QueryRow(ctx, "select view_count from client_reports where id = $1", outcomesOnly.ID).Scan(&views)
		if views != 2 {
			t.Fatalf("operator preview counted as a view: %d", views)
		}
	})

	notAvailable := func(t *testing.T, token string) {
		t.Helper()
		rec, env := do(t, r, "", http.MethodGet, "/api/public/reports/"+token, nil)
		if rec.Code != http.StatusNotFound || env.Error.Code != "REPORT_NOT_AVAILABLE" || env.Error.Message != "This report is no longer available" {
			t.Fatalf("got (%d, %s, %q), want the uniform not-available response", rec.Code, env.Error.Code, env.Error.Message)
		}
	}

	t.Run("revoking takes effect immediately", func(t *testing.T) {
		rec, env := do(t, r, fx.admin, http.MethodDelete, "/api/v1/reports/"+full.ID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("revoke: %d %s", rec.Code, env.Error.Code)
		}
		notAvailable(t, full.ShareToken)
	})

	t.Run("expired, unknown and malformed tokens look identical to revoked", func(t *testing.T) {
		if _, err := sys.Exec(ctx, "update client_reports set expires_at = now() - interval '1 second' where id = $1", outcomesOnly.ID); err != nil {
			t.Fatal(err)
		}
		notAvailable(t, outcomesOnly.ShareToken)
		notAvailable(t, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		notAvailable(t, "short")
	})

	t.Run("list shows each report's status and views", func(t *testing.T) {
		rec, env := do(t, r, fx.admin, http.MethodGet, "/api/v1/reports?org_id="+fx.orgID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d", rec.Code)
		}
		var out struct {
			Reports []struct {
				ID        string `json:"id"`
				Status    string `json:"status"`
				ViewCount int    `json:"view_count"`
			} `json:"reports"`
		}
		_ = json.Unmarshal(env.Data, &out)
		got := map[string]string{}
		for _, rep := range out.Reports {
			got[rep.ID] = rep.Status
		}
		if got[outcomesOnly.ID] != "expired" || got[full.ID] != "revoked" {
			t.Fatalf("statuses = %v", got)
		}
	})

	t.Run("create and revoke are audit-logged", func(t *testing.T) {
		var n int
		_ = sys.QueryRow(ctx, "select count(*) from audit_logs where org_id = $1 and action in ('report.created','report.revoked')", fx.orgID).Scan(&n)
		if n != 3 {
			t.Fatalf("audit rows = %d, want 3 (2 created + 1 revoked)", n)
		}
	})
}

func TestReports_AccessAndValidation(t *testing.T) {
	app, sys := pool(t, "TEST_APP_DATABASE_URL"), pool(t, "TEST_SYSTEM_DATABASE_URL")
	fx := newFixture(t, sys)
	r := router(app, sys, allowAll{})
	valid := map[string]any{"org_id": fx.orgID, "date_from": "2026-09-01", "date_to": "2026-09-30"}

	rec, env := do(t, r, fx.admin, http.MethodPost, "/api/v1/reports", valid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, env.Error.Code)
	}
	var rep created
	_ = json.Unmarshal(env.Data, &rep)

	if rec, env := do(t, r, fx.member, http.MethodPost, "/api/v1/reports", valid); rec.Code != http.StatusForbidden || env.Error.Code != "NOT_AUTHORIZED" {
		t.Fatalf("member create: (%d, %s)", rec.Code, env.Error.Code)
	}
	if rec, env := do(t, r, fx.outsider, http.MethodPost, "/api/v1/reports", valid); rec.Code != http.StatusForbidden {
		t.Fatalf("outsider create for another org: (%d, %s)", rec.Code, env.Error.Code)
	}
	for _, u := range []string{fx.member, fx.outsider} {
		if rec, _ := do(t, r, u, http.MethodGet, "/api/v1/reports/"+rep.ID, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s get: %d, want 404", u, rec.Code)
		}
		if rec, _ := do(t, r, u, http.MethodDelete, "/api/v1/reports/"+rep.ID, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s revoke: %d, want 404", u, rec.Code)
		}
	}

	for name, body := range map[string]map[string]any{
		"reversed range": {"org_id": fx.orgID, "date_from": "2026-09-30", "date_to": "2026-09-01"},
		"bad date":       {"org_id": fx.orgID, "date_from": "Sept 1", "date_to": "2026-09-30"},
		"over a year":    {"org_id": fx.orgID, "date_from": "2025-01-01", "date_to": "2026-09-30"},
		"future start":   {"org_id": fx.orgID, "date_from": "2099-01-01", "date_to": "2099-01-31"},
		"expiry 0 days":  {"org_id": fx.orgID, "date_from": "2026-09-01", "date_to": "2026-09-30", "expires_in_days": 0},
	} {
		if rec, env := do(t, r, fx.admin, http.MethodPost, "/api/v1/reports", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: (%d, %s), want 400", name, rec.Code, env.Error.Code)
		}
	}

	t.Run("a rate-limited caller gets 429", func(t *testing.T) {
		rr := router(app, sys, denyAll{})
		if rec, env := do(t, rr, "", http.MethodGet, "/api/public/reports/"+rep.ShareToken, nil); rec.Code != http.StatusTooManyRequests || env.Error.Code != "RATE_LIMITED" {
			t.Fatalf("got (%d, %s), want 429", rec.Code, env.Error.Code)
		}
	})
}

func TestRedisLimiter_LimitsPerIPAndIgnoresSpoofedForwardedFor(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skip("redis not reachable: ", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	app, sys := pool(t, "TEST_APP_DATABASE_URL"), pool(t, "TEST_SYSTEM_DATABASE_URL")
	// A fresh limit window per run: the key includes this unique window size.
	limiter := NewRedisLimiter(rdb, 3, time.Minute+time.Duration(time.Now().UnixNano()%1000)*time.Millisecond)
	r := router(app, sys, limiter)

	// Same connection, a different forged X-Forwarded-For each time: with no
	// trusted proxies they all count against the one real client IP.
	codes := []int{}
	for i := 0; i < 5; i++ {
		rec, _ := do(t, r, "", http.MethodGet, "/api/public/reports/unknown-token-value-xxxxxxxx", nil,
			"X-Forwarded-For", "203.0.113."+string(rune('1'+i)))
		codes = append(codes, rec.Code)
	}
	want := []int{404, 404, 404, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("codes = %v, want %v (spoofed X-Forwarded-For must not reset the limit)", codes, want)
		}
	}
}
