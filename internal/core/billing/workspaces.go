package billing

import (
	"context"
	"fmt"
	"github.com/founderstack/api/internal/pkg/safego"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"

	"github.com/founderstack/api/internal/db/dbgen"
)

// SyncWorkspaceUsage makes a practice's subscription carry one "additional
// client workspace" unit for each active client workspace beyond its
// plan's included count (workflow 24). It's a reconcile, not an increment:
// it compares what Stripe has against the current count and only calls
// Stripe when they differ, so it's safe to run after every change, from the
// webhook, and from the daily job — and the subscription.updated event its
// own update causes finds nothing left to do.
func (s *Syncer) SyncWorkspaceUsage(ctx context.Context, orgID pgtype.UUID) error {
	org, err := dbgen.New(s.systemPool).GetOrgBilling(ctx, orgID)
	if err != nil {
		return err
	}
	subID := deref(org.StripeSubscriptionID)
	if org.OrganizationType != "practice" || subID == "" || !IsLive(deref(org.SubscriptionStatus)) {
		return nil
	}
	plan, ok := PlanByTier(deref(org.PlanTier))
	if !ok {
		return nil
	}
	sub, err := s.api.GetSubscription(ctx, subID)
	if err != nil {
		return fmt.Errorf("billing: fetching subscription: %w", err)
	}
	changes := workspaceItemChanges(sub, plan, plan.ExtraWorkspaces(org.ActiveClientWorkspaces))
	if len(changes) == 0 {
		return nil
	}
	if err := s.api.UpdateItems(ctx, subID, changes); err != nil {
		return fmt.Errorf("billing: updating workspace items: %w", err)
	}
	slog.Info("billing: client workspace usage synced", "org", org.Name,
		"active", org.ActiveClientWorkspaces, "billed_extra", plan.ExtraWorkspaces(org.ActiveClientWorkspaces))
	return nil
}

// workspaceItemChanges is what turns sub's extra-workspace items into
// exactly `want` units of plan's extra price: other tiers' extra items (left
// from a plan change) are removed, and a zero count leaves no item at all.
func workspaceItemChanges(sub *stripe.Subscription, plan Plan, want int64) []ItemChange {
	var changes []ItemChange
	found := false
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			if it.Price == nil || !IsExtraWorkspaceLookupKey(it.Price.LookupKey) {
				continue
			}
			switch {
			case it.Price.LookupKey != plan.ExtraWorkspaceLookupKey || want == 0 || found:
				changes = append(changes, ItemChange{ItemID: it.ID, Delete: true})
			case it.Quantity != want:
				found = true
				changes = append(changes, ItemChange{ItemID: it.ID, Quantity: want})
			default:
				found = true
			}
		}
	}
	if !found && want > 0 {
		changes = append(changes, ItemChange{LookupKey: plan.ExtraWorkspaceLookupKey, Quantity: want})
	}
	return changes
}

// RunWorkspaceUsageJob re-syncs every practice's workspace count once a
// day — a safety net for a sync that failed at the moment of a change
// (Stripe briefly unreachable, the process restarted mid-request).
func RunWorkspaceUsageJob(ctx context.Context, systemPool *pgxpool.Pool, s *Syncer) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		_ = safego.Do("billing: workspace usage job", func() {
			ids, err := dbgen.New(systemPool).ListPracticesWithLiveSubscriptions(ctx)
			if err != nil {
				slog.Error("billing: workspace usage job: listing practices", "err", err)
			}
			for _, id := range ids {
				if err := s.SyncWorkspaceUsage(ctx, id); err != nil {
					slog.Error("billing: workspace usage job", "org_id", id.String(), "err", err)
				}
			}
		})
		timer.Reset(24 * time.Hour)
	}
}
