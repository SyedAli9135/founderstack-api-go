package billing

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

const usageWindowDays = 30

type Handler struct {
	appPool *pgxpool.Pool
}

func NewHandler(appPool *pgxpool.Pool) *Handler {
	return &Handler{appPool: appPool}
}

// rg must already have middleware.RequireAuth applied.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/billing/usage", h.Usage)
	rg.GET("/billing/ledger", h.Ledger)
}

type dailyUsagePoint struct {
	Day              string  `json:"day"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	EstimatedCostUsd float64 `json:"estimated_cost_usd"`
}

type agentCostShareItem struct {
	AgentName    string  `json:"agent_name"`
	TotalCostUsd float64 `json:"total_cost_usd"`
}

type usageResponse struct {
	InputTokens       int64                `json:"input_tokens"`
	OutputTokens      int64                `json:"output_tokens"`
	CachedTokens      int64                `json:"cached_tokens"`
	ThinkingTokens    int64                `json:"thinking_tokens"`
	TotalEstimatedUsd float64              `json:"total_estimated_usd"`
	CacheHitRate      float64              `json:"cache_hit_rate"`
	DailyUsage        []dailyUsagePoint    `json:"daily_usage"`
	AgentCostShare    []agentCostShareItem `json:"agent_cost_share"`
}

// Usage is the same headline aggregate as settings.APIKeyUsage, over a
// rolling 30-day window instead of calendar-month, plus the daily trend and
// per-agent breakdown the plan's usage page chart needs — one request
// covers everything that page renders.
func (h *Handler) Usage(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	since := time.Now().UTC().AddDate(0, 0, -usageWindowDays)
	sinceParam := pgtype.Timestamptz{Time: since, Valid: true}

	var totals dbgen.GetCostUsageSinceRow
	var dailyRows []dbgen.GetDailyCostUsageRow
	var agentRows []dbgen.GetAgentCostShareRow
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		totals, err = q.GetCostUsageSince(ctx, dbgen.GetCostUsageSinceParams{OrgID: user.OrgID, CreatedAt: sinceParam})
		if err != nil {
			return err
		}
		dailyRows, err = q.GetDailyCostUsage(ctx, dbgen.GetDailyCostUsageParams{OrgID: user.OrgID, CreatedAt: sinceParam})
		if err != nil {
			return err
		}
		agentRows, err = q.GetAgentCostShare(ctx, dbgen.GetAgentCostShareParams{OrgID: user.OrgID, CreatedAt: sinceParam})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch usage")
		return
	}

	var cacheHitRate float64
	if promptTokens := totals.InputTokens + totals.CachedTokens; promptTokens > 0 {
		cacheHitRate = float64(totals.CachedTokens) / float64(promptTokens)
	}

	daily := make([]dailyUsagePoint, 0, len(dailyRows))
	for _, r := range dailyRows {
		daily = append(daily, dailyUsagePoint{
			Day: r.Day.Time.Format("2006-01-02"), InputTokens: r.InputTokens,
			OutputTokens: r.OutputTokens, CachedTokens: r.CachedTokens, EstimatedCostUsd: r.EstimatedCostUsd,
		})
	}
	agentShare := make([]agentCostShareItem, 0, len(agentRows))
	for _, r := range agentRows {
		agentShare = append(agentShare, agentCostShareItem{AgentName: r.AgentName, TotalCostUsd: r.TotalCostUsd})
	}

	response.OK(c, http.StatusOK, "", usageResponse{
		InputTokens: totals.InputTokens, OutputTokens: totals.OutputTokens,
		CachedTokens: totals.CachedTokens, ThinkingTokens: totals.ThinkingTokens,
		TotalEstimatedUsd: totals.TotalEstimatedUsd, CacheHitRate: cacheHitRate,
		DailyUsage: daily, AgentCostShare: agentShare,
	})
}

type ledgerEntry struct {
	ID               string  `json:"id"`
	CreatedAt        string  `json:"created_at"`
	CostType         string  `json:"cost_type"`
	Provider         *string `json:"provider,omitempty"`
	Model            *string `json:"model,omitempty"`
	InputTokens      int32   `json:"input_tokens"`
	OutputTokens     int32   `json:"output_tokens"`
	CachedTokens     int32   `json:"cached_tokens"`
	ThinkingTokens   int32   `json:"thinking_tokens"`
	EstimatedCostUsd float64 `json:"estimated_cost_usd"`
}

type ledgerResponse struct {
	Entries []ledgerEntry `json:"entries"`
	Total   int64         `json:"total"`
}

// Ledger is the raw, itemized cost_ledger feed behind Usage's aggregates —
// same limit/offset shape internal/api/runs and internal/api/approvals
// already use.
func (h *Handler) Ledger(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	limit := int32(50)
	if raw := c.Query("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 200 {
			limit = int32(v)
		}
	}
	offset := int32(0)
	if raw := c.Query("offset"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 {
			offset = int32(v)
		}
	}

	var rows []dbgen.ListCostLedgerPageRow
	var total int64
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListCostLedgerPage(ctx, dbgen.ListCostLedgerPageParams{OrgID: user.OrgID, Limit: limit, Offset: offset})
		if err != nil {
			return err
		}
		total, err = q.CountCostLedger(ctx, user.OrgID)
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch cost ledger")
		return
	}

	entries := make([]ledgerEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, ledgerEntry{
			ID: r.ID.String(), CreatedAt: r.CreatedAt.Time.Format(time.RFC3339), CostType: r.CostType,
			Provider: r.Provider, Model: r.Model, InputTokens: derefInt32(r.InputTokens),
			OutputTokens: derefInt32(r.OutputTokens), CachedTokens: derefInt32(r.CachedTokens),
			ThinkingTokens: derefInt32(r.ThinkingTokens), EstimatedCostUsd: r.EstimatedCostUsd,
		})
	}

	response.OK(c, http.StatusOK, "", ledgerResponse{Entries: entries, Total: total})
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}
