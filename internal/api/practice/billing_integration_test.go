//go:build integration

package practice

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"

	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/core/billing"
	"github.com/founderstack/api/internal/core/billing/billingtest"
)

func billedRouter(pool *pgxpool.Pool, syncer *billing.Syncer) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	h := NewHandler(pool, nil, &fakeProvisioner{}, fakeTools{})
	h.SetUsageSyncer(syncer)
	g := r.Group("/api/v1")
	g.Use(middleware.RequireAuth(pool, testCfg))
	h.Register(g)
	return r
}

func TestPractice_PerWorkspaceBilling(t *testing.T) {
	pool := testSystemPool(t)
	fx := newFixture(t, pool)
	ctx := context.Background()
	growth, _ := billing.PlanByTier("growth")
	studio, _ := billing.PlanByTier("studio")

	fake := billingtest.New()
	emails := &billingtest.Emails{}
	subID := "sub_practice_" + randSuffix()
	customer := "cus_practice_" + randSuffix()
	fake.Subscription(subID, customer, growth, stripe.SubscriptionStatusActive)
	// The practice as a live Growth subscriber would be after the webhook sync.
	if _, err := pool.Exec(ctx, `update organizations set plan_tier = 'growth', subscription_status = 'active',
		stripe_customer_id = $2, stripe_subscription_id = $3, max_agents = 10, max_workflows = 25,
		included_client_workspaces = 3, max_client_workspaces = 25 where id = $1`,
		fx.practiceID, customer, subID); err != nil {
		t.Fatal(err)
	}
	syncer := billing.NewSyncer(pool, fake, emails, "http://app.test")
	r := billedRouter(pool, syncer)

	terms := func() practiceBilling {
		t.Helper()
		_, env := call(t, r, fx.operator, fx.practiceClerkID, http.MethodGet, "/api/v1/practice/client-workspaces", nil)
		var out struct {
			Practice struct {
				Billing practiceBilling `json:"billing"`
			} `json:"practice"`
		}
		if err := json.Unmarshal(env.Data, &out); err != nil {
			t.Fatal(err)
		}
		return out.Practice.Billing
	}

	var created []createdWorkspace
	for _, name := range []string{"Initech", "Globex", "Vandelay"} {
		created = append(created, fx.create(t, r, name))
	}
	if extra := fake.ExtraItems(subID); len(extra) != 0 {
		t.Fatalf("3 of 3 included should bill nothing extra, got %v", extra)
	}

	fourth := fx.create(t, r, "Wonka")
	if got := fake.ExtraItems(subID)[growth.ExtraWorkspaceLookupKey]; got != 1 {
		t.Fatalf("4th workspace on Growth: extra quantity %d, want 1", got)
	}
	if b := terms(); b.ActiveClientWorkspaces != 4 || b.IncludedClientWorkspaces != 3 || b.BilledExtraWorkspaces != 1 || b.ExtraWorkspaceUSD != 15 {
		t.Fatalf("portfolio billing terms: %+v", b)
	}

	// A client workspace runs on its practice's plan limits.
	var tier string
	var maxAgents int32
	if err := pool.QueryRow(ctx, `select plan_tier, max_agents from organizations where id = $1`, fourth.ID).Scan(&tier, &maxAgents); err != nil {
		t.Fatal(err)
	}
	if tier != "growth" || maxAgents != 10 {
		t.Fatalf("new workspace limits: %s/%d, want growth/10", tier, maxAgents)
	}

	// Removing one stops billing it straight away; restoring bills it again.
	if code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodDelete, "/api/v1/practice/client-workspaces/"+created[0].ID, nil); code != http.StatusOK {
		t.Fatalf("remove: %d", code)
	}
	if extra := fake.ExtraItems(subID); len(extra) != 0 {
		t.Fatalf("after removal the extra item should be gone, got %v", extra)
	}
	if code, _ := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, "/api/v1/practice/client-workspaces/"+created[0].ID+"/restore", nil); code != http.StatusOK {
		t.Fatalf("restore: %d", code)
	}
	if got := fake.ExtraItems(subID)[growth.ExtraWorkspaceLookupKey]; got != 1 {
		t.Fatalf("after restore: extra quantity %d, want 1", got)
	}

	// Re-running the sync with nothing changed doesn't touch Stripe.
	updates := fake.ItemUpdates
	if err := syncer.SyncWorkspaceUsage(ctx, fx.practiceID); err != nil || fake.ItemUpdates != updates {
		t.Fatalf("idempotent sync: err %v, updates %d -> %d", err, updates, fake.ItemUpdates)
	}

	// Ahead of renewal, owners/admins hear about the extra workspace.
	syncer.NotifyUpcomingOverage(ctx, fx.practiceID)
	if emails.Count() != 1 || !strings.HasPrefix(emails.Sent[0], fx.operator) {
		t.Fatalf("upcoming-overage emails: %v", emails.Sent)
	}

	// The plan's cap is a hard stop, with the cap in the message.
	if _, err := pool.Exec(ctx, `update organizations set max_client_workspaces = 4 where id = $1`, fx.practiceID); err != nil {
		t.Fatal(err)
	}
	code, env := call(t, r, fx.operator, fx.practiceClerkID, http.MethodPost, "/api/v1/practice/client-workspaces", map[string]any{"name": "Over"})
	if code != http.StatusPaymentRequired || env.Error.Code != "CLIENT_WORKSPACE_LIMIT_REACHED" {
		t.Fatalf("over the cap: (%d, %s)", code, env.Error.Code)
	}

	// Moving to Studio (10 included) swaps prices and bills nothing extra;
	// a subscription change on Stripe's side drives this through Apply.
	sub, _ := fake.GetSubscription(ctx, subID)
	sub.Items.Data[0].Price = &stripe.Price{ID: "price_studio", LookupKey: studio.LookupKey}
	if _, err := syncer.Apply(ctx, fx.practiceID, sub, false); err != nil {
		t.Fatal(err)
	}
	if extra := fake.ExtraItems(subID); len(extra) != 0 {
		t.Fatalf("on Studio 4 of 10 included should bill nothing, got %v", extra)
	}
	if err := pool.QueryRow(ctx, `select max_agents from organizations where id = $1`, fourth.ID).Scan(&maxAgents); err != nil || maxAgents != 50 {
		t.Fatalf("plan change should reach client workspaces: max_agents %d (%v)", maxAgents, err)
	}
	syncer.NotifyUpcomingOverage(ctx, fx.practiceID)
	if emails.Count() != 1 {
		t.Fatalf("no overage means no warning, got %d emails", emails.Count())
	}
}
