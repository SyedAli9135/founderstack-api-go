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
