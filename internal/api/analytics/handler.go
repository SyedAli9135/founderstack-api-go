package analytics

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// equivalentSalaryPerHourUSD is a fixed reference rate for the "equivalent
// salary" figure
const equivalentSalaryPerHourUSD = 50.0

type Handler struct {
	appPool *pgxpool.Pool
}

func NewHandler(appPool *pgxpool.Pool) *Handler {
	return &Handler{appPool: appPool}
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/analytics/hours-saved", h.HoursSaved)
	rg.GET("/analytics/agent-performance", h.AgentPerformance)
	rg.GET("/analytics/rag-quality", h.RagQuality)
}

type hoursSavedResponse struct {
	TotalHoursSaved     float64 `json:"total_hours_saved"`
	ThisMonthHoursSaved float64 `json:"this_month_hours_saved"`
	ThisWeekHoursSaved  float64 `json:"this_week_hours_saved"`
	EquivalentSalaryUSD float64 `json:"equivalent_salary_usd"`
}

func (h *Handler) HoursSaved(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	now := time.Now().UTC()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	// ISO week: Monday start. time.Weekday's Sunday=0 needs remapping so
	// Monday is the 0-offset day, not Sunday.
	weekday := (int(now.Weekday()) + 6) % 7
	startOfWeek := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -weekday)

	var total, thisMonth, thisWeek float64
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		total, err = q.GetOrgTotalHoursSaved(ctx, user.OrgID)
		if err != nil {
			return err
		}
		thisMonth, err = q.GetHoursSavedSince(ctx, dbgen.GetHoursSavedSinceParams{OrgID: user.OrgID, CompletedAt: pgtype.Timestamptz{Time: startOfMonth, Valid: true}})
		if err != nil {
			return err
		}
		thisWeek, err = q.GetHoursSavedSince(ctx, dbgen.GetHoursSavedSinceParams{OrgID: user.OrgID, CompletedAt: pgtype.Timestamptz{Time: startOfWeek, Valid: true}})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch hours saved")
		return
	}

	response.OK(c, http.StatusOK, "Hours saved fetched", hoursSavedResponse{
		TotalHoursSaved: total, ThisMonthHoursSaved: thisMonth, ThisWeekHoursSaved: thisWeek,
		EquivalentSalaryUSD: total * equivalentSalaryPerHourUSD,
	})
}

type agentPerformanceItem struct {
	AgentID       string  `json:"agent_id"`
	AgentName     string  `json:"agent_name"`
	TotalRuns     int64   `json:"total_runs"`
	SuccessRate   float64 `json:"success_rate"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	AvgCostUsd    float64 `json:"avg_cost_usd"`
	FailureCount  int64   `json:"failure_count"`
}

// AgentPerformance reports every agent that has at least one workflow run —
// an agent with a workflow but zero runs yet doesn't appear (there's
// nothing to rate it on), matching GetAgentPerformance's INNER JOINs.
func (h *Handler) AgentPerformance(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	items := []agentPerformanceItem{}
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		rows, err := q.GetAgentPerformance(ctx, user.OrgID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			var successRate float64
			if r.TotalRuns > 0 {
				successRate = float64(r.SuccessCount) / float64(r.TotalRuns)
			}
			items = append(items, agentPerformanceItem{
				AgentID: r.AgentID.String(), AgentName: r.AgentName, TotalRuns: r.TotalRuns,
				SuccessRate: successRate, AvgDurationMs: r.AvgDurationMs, AvgCostUsd: r.AvgCostUsd,
				FailureCount: r.FailureCount,
			})
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch agent performance")
		return
	}

	response.OK(c, http.StatusOK, "", items)
}

type ragQualityResponse struct {
	AvgRerankScore     float64 `json:"avg_rerank_score"`
	AvgChunksRetrieved float64 `json:"avg_chunks_retrieved"`
	CacheHitRate       float64 `json:"cache_hit_rate"`
	TotalSearches      int64   `json:"total_searches"`
}

// RagQuality covers the last 30 days — a rolling quality signal, not an
// all-time one, since a founder cares whether search is working well
// *now* (matching the daily-trend window every other analytics endpoint
// here uses).
func (h *Handler) RagQuality(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	since := time.Now().UTC().AddDate(0, 0, -30)
	var stats dbgen.GetRagQualityStatsRow
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		stats, err = q.GetRagQualityStats(ctx, dbgen.GetRagQualityStatsParams{
			OrgID: user.OrgID, CreatedAt: pgtype.Timestamptz{Time: since, Valid: true},
		})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch RAG quality stats")
		return
	}

	response.OK(c, http.StatusOK, "", ragQualityResponse{
		AvgRerankScore: stats.AvgRerankScore, AvgChunksRetrieved: stats.AvgChunksRetrieved,
		CacheHitRate: stats.CacheHitRate, TotalSearches: stats.TotalSearches,
	})
}
