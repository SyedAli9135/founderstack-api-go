// Package digest builds and sends the daily email digest (workflow 20):
// yesterday's run counts, hours saved, cost, and pending approvals for an
// org, mailed via the same Brevo sender internal/core/notify already
// built for approval-gate notifications.
package digest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/db/dbgen"
)

// Payload is everything a digest email is rendered from, built fresh
// from the DB on every send rather than cached -- a digest must never
// show a founder something the dashboard itself would disagree with.
type Payload struct {
	OrgName          string
	Date             string // yesterday, in the org's own timezone, formatted for humans
	HadActivity      bool   // false drives the "No runs yesterday — all quiet" branch
	TotalRuns        int64
	SuccessfulRuns   int64
	FailedRuns       int64
	HoursSaved       float64
	CostUSD          float64
	PendingApprovals int64
	TopAgentName     string // "" when nobody ran anything yesterday
	TopAgentRuns     int64
}

// BuildPayload takes a *dbgen.Queries directly (not a pool) so it works
// identically whether the caller is the cross-org scheduler (system
// pool, no RLS) or the authenticated "send test email" handler (app
// pool, tenant.WithTx) -- both just need one org's worth of numbers.
func BuildPayload(ctx context.Context, q *dbgen.Queries, orgID pgtype.UUID, orgName, tz string) (*Payload, error) {
	stats, err := q.GetDigestRunStats(ctx, dbgen.GetDigestRunStatsParams{OrgID: orgID, Timezone: tz})
	if err != nil {
		return nil, fmt.Errorf("digest: run stats: %w", err)
	}
	costUSD, err := q.GetDigestCostUSD(ctx, dbgen.GetDigestCostUSDParams{OrgID: orgID, Timezone: tz})
	if err != nil {
		return nil, fmt.Errorf("digest: cost: %w", err)
	}
	pending, err := q.GetDigestPendingApprovalsCount(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("digest: pending approvals: %w", err)
	}

	var topAgentName string
	var topAgentRuns int64
	top, err := q.GetDigestTopAgent(ctx, dbgen.GetDigestTopAgentParams{OrgID: orgID, Timezone: tz})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("digest: top agent: %w", err)
	}
	if err == nil {
		topAgentName, topAgentRuns = top.AgentName, top.RunCount
	}

	return &Payload{
		OrgName:          orgName,
		Date:             yesterdayLabel(tz),
		HadActivity:      stats.TotalRuns > 0,
		TotalRuns:        stats.TotalRuns,
		SuccessfulRuns:   stats.SuccessfulRuns,
		FailedRuns:       stats.FailedRuns,
		HoursSaved:       stats.HoursSaved,
		CostUSD:          costUSD,
		PendingApprovals: pending,
		TopAgentName:     topAgentName,
		TopAgentRuns:     topAgentRuns,
	}, nil
}

// yesterdayLabel falls back to UTC for a malformed/unknown tz string
// rather than failing the whole digest over a cosmetic date label -- the
// SQL side's `AT TIME ZONE` already tolerates the same input the same way.
func yesterdayLabel(tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	return time.Now().In(loc).AddDate(0, 0, -1).Format("Monday, Jan 2")
}
