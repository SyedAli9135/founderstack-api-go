//go:build integration

package approvals

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/core/notify"
	"github.com/founderstack/api/internal/pkg/secret"
)

// pendingApproval inserts a run waiting on approval plus its pending approval
// row directly, so these tests exercise only the decision endpoint's auth.
func pendingApproval(t *testing.T, systemPool *pgxpool.Pool, fx approvalFixture) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	var runID, approvalID pgtype.UUID
	if err := systemPool.QueryRow(ctx,
		`insert into workflow_runs (workflow_id, org_id, status) values ($1, $2, 'awaiting_approval') returning id`,
		fx.workflowID, fx.orgID).Scan(&runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := systemPool.QueryRow(ctx,
		`insert into approvals (run_id, org_id, status, expires_at) values ($1, $2, 'pending', now() + interval '1 hour') returning id`,
		runID, fx.orgID).Scan(&approvalID); err != nil {
		t.Fatalf("insert approval: %v", err)
	}
	return approvalID
}

func approvalStatus(t *testing.T, systemPool *pgxpool.Pool, id pgtype.UUID) string {
	t.Helper()
	var s string
	if err := systemPool.QueryRow(context.Background(), "select status from approvals where id = $1", id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestApprovalsHandler_ActionTokenIsScopedToItsApprovalAndActiveUser(t *testing.T) {
	appPool, systemPool := testAppPool(t), testSystemPool(t)
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: secret.Value("test-dev-token-secret"), PushActionTokenSecret: secret.Value("test-push-secret")}
	tokens := notify.NewActionTokenSigner(cfg.PushActionTokenSecret)
	router := testRouter(t, systemPool, appPool, cfg, nil, tokens) // a refused request never reaches the launcher

	fx := newApprovalFixture(t, systemPool, appPool)
	approvalA, approvalB := pendingApproval(t, systemPool, fx), pendingApproval(t, systemPool, fx)

	decide := func(approvalID pgtype.UUID, token string) int {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, jsonRequest(http.MethodPost, "/api/v1/approvals/"+idString(approvalID)+"/approve?action_token="+token, nil))
		return rec.Code
	}

	tokenForA := tokens.Sign(uuidFromPg(approvalA), uuidFromPg(fx.approverUserID), time.Now().Add(time.Hour))
	if got := decide(approvalB, tokenForA); got != http.StatusUnauthorized {
		t.Errorf("token for approval A used on approval B: status = %d, want 401", got)
	}
	if got := approvalStatus(t, systemPool, approvalB); got != "pending" {
		t.Errorf("approval B = %q after a refused request, want pending", got)
	}

	if _, err := systemPool.Exec(context.Background(), "update users set is_active = false where id = $1", fx.approverUserID); err != nil {
		t.Fatal(err)
	}
	if got := decide(approvalA, tokenForA); got == http.StatusOK {
		t.Errorf("token of a removed user: status = %d, want a refusal", got)
	}
	if got := approvalStatus(t, systemPool, approvalA); got != "pending" {
		t.Errorf("approval A = %q after a removed user's token, want pending", got)
	}
}

func TestApprovalsHandler_RejectReasonIsBounded(t *testing.T) {
	appPool, systemPool := testAppPool(t), testSystemPool(t)
	cfg := &config.Config{AppEnv: "development", DevTokenSecret: secret.Value("test-dev-token-secret"), PushActionTokenSecret: secret.Value("test-push-secret")}
	router := testRouter(t, systemPool, appPool, cfg, nil, notify.NewActionTokenSigner(cfg.PushActionTokenSecret))
	fx := newApprovalFixture(t, systemPool, appPool)
	approvalID := pendingApproval(t, systemPool, fx)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authedRequest(t, cfg, fx.approverClerkID, http.MethodPost,
		"/api/v1/approvals/"+idString(approvalID)+"/reject", map[string]string{"reason": strings.Repeat("r", maxReasonLen+1)}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "REASON_TOO_LONG") {
		t.Fatalf("oversized reason: got (%d, %s), want 400 REASON_TOO_LONG", rec.Code, rec.Body.String())
	}
	if got := approvalStatus(t, systemPool, approvalID); got != "pending" {
		t.Errorf("approval = %q after a refused reject, want pending", got)
	}
}
