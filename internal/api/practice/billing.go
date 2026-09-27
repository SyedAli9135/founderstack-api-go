package practice

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	corebilling "github.com/founderstack/api/internal/core/billing"
	"github.com/founderstack/api/internal/db/dbgen"
)

// practiceBilling is what the portfolio and new-workspace pages need to
// say "4 of 3 included" and "this adds $15 to your next invoice" before a
// charge happens. active_client_workspaces here is the real
// count — the portfolio list's own count only covers workspaces the viewer
// belongs to.
type practiceBilling struct {
	PlanTier                 string `json:"plan_tier"`
	PlanName                 string `json:"plan_name"`
	IncludedClientWorkspaces int32  `json:"included_client_workspaces"`
	MaxClientWorkspaces      int32  `json:"max_client_workspaces"`
	ActiveClientWorkspaces   int64  `json:"active_client_workspaces"`
	ExtraWorkspaceUSD        int64  `json:"extra_workspace_usd"`
	BilledExtraWorkspaces    int64  `json:"billed_extra_workspaces"`
}

func billingTerms(ctx context.Context, q *dbgen.Queries, practiceID pgtype.UUID) (practiceBilling, error) {
	org, err := q.GetOrgBilling(ctx, practiceID)
	if err != nil {
		return practiceBilling{}, err
	}
	plan, ok := corebilling.PlanByTier(deref(org.PlanTier))
	if !ok {
		plan = corebilling.DefaultPlan
	}
	return practiceBilling{
		PlanTier: plan.Tier, PlanName: plan.Name,
		IncludedClientWorkspaces: org.IncludedClientWorkspaces, MaxClientWorkspaces: org.MaxClientWorkspaces,
		ActiveClientWorkspaces: org.ActiveClientWorkspaces, ExtraWorkspaceUSD: plan.ExtraWorkspaceUSD,
		BilledExtraWorkspaces: plan.ExtraWorkspaces(org.ActiveClientWorkspaces),
	}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
