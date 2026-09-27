package billing

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/db/dbgen"
)

// WorkflowLimitReached reports whether the org already has as many active
// workflows as its plan allows. It reads organizations.max_workflows,
// which the subscription sync keeps in step with the plan.
func WorkflowLimitReached(ctx context.Context, q *dbgen.Queries, orgID pgtype.UUID) (bool, error) {
	limit, err := q.GetOrganizationMaxWorkflows(ctx, orgID)
	if err != nil || limit == nil {
		return false, err
	}
	count, err := q.CountActiveWorkflows(ctx, orgID)
	if err != nil {
		return false, err
	}
	return count >= int64(*limit), nil
}

// StorageLimitReached reports whether adding addBytes would take the org's
// knowledge base past its plan's storage allowance.
func StorageLimitReached(ctx context.Context, q *dbgen.Queries, orgID pgtype.UUID, addBytes int64) (bool, error) {
	a, err := q.GetOrgStorageAllowance(ctx, orgID)
	if err != nil || a.MaxRagStorageGb == nil {
		return false, err
	}
	return a.UsedBytes+addBytes > int64(*a.MaxRagStorageGb)<<30, nil
}
