package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

type Handler struct {
	appPool *pgxpool.Pool
	engine  *graph.Engine
}

func NewHandler(appPool *pgxpool.Pool, engine *graph.Engine) *Handler {
	return &Handler{appPool: appPool, engine: engine}
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/runs", h.List)
	rg.GET("/runs/:id", h.Get)
	rg.POST("/runs/:id/cancel", h.Cancel)
	rg.GET("/runs/:id/stream", h.Stream)
}

func parseRunID(c *gin.Context) (pgtype.UUID, bool) {
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid run id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

type runSummary struct {
	ID           string  `json:"id"`
	WorkflowID   string  `json:"workflow_id"`
	Status       string  `json:"status"`
	Output       *string `json:"output,omitempty"`
	CostSoFarUSD float64 `json:"cost_so_far_usd"`
	StartedAt    *string `json:"started_at,omitempty"`
	CompletedAt  *string `json:"completed_at,omitempty"`
	DurationMs   *int32  `json:"duration_ms,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

func (h *Handler) List(c *gin.Context) {
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

	var statusFilter *string
	if s := c.Query("status"); s != "" {
		statusFilter = &s
	}
	var workflowFilter pgtype.UUID
	if w := c.Query("workflow_id"); w != "" {
		id, err := uuid.Parse(w)
		if err != nil {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid workflow_id")
			return
		}
		workflowFilter = pgtype.UUID{Bytes: id, Valid: true}
	}

	var rows []dbgen.ListRunsForOrgRow
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListRunsForOrg(ctx, dbgen.ListRunsForOrgParams{
			OrgID: user.OrgID, Limit: limit, Offset: offset, Status: statusFilter, WorkflowID: workflowFilter,
		})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list runs")
		return
	}

	out := make([]runSummary, len(rows))
	for i, r := range rows {
		out[i] = runSummary{
			ID: r.ID.String(), WorkflowID: r.WorkflowID.String(), Status: r.Status,
			Output: r.Output, CostSoFarUSD: r.CostSoFarUsd, DurationMs: r.DurationMs,
			StartedAt: formatTimestamptz(r.StartedAt), CompletedAt: formatTimestamptz(r.CompletedAt),
			CreatedAt: r.CreatedAt.Time.Format(rfc3339),
		}
	}
	response.OK(c, http.StatusOK, "Runs listed", gin.H{"runs": out})
}

type runDetail struct {
	runSummary
	CurrentNode   *string `json:"current_node,omitempty"`
	TriggeredBy   *string `json:"triggered_by,omitempty"`
	InputTokens   int32   `json:"input_tokens"`
	OutputTokens  int32   `json:"output_tokens"`
	CachedTokens  int32   `json:"cached_tokens"`
	ToolCallCount int32   `json:"tool_call_count"`
}

func (h *Handler) Get(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseRunID(c)
	if !ok {
		return
	}

	var row dbgen.GetRunDetailRow
	var notFound bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		row, err = q.GetRunDetail(ctx, dbgen.GetRunDetailParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch run")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "RUN_NOT_FOUND", "Run not found")
		return
	}

	var triggeredBy *string
	if row.TriggeredBy.Valid {
		s := row.TriggeredBy.String()
		triggeredBy = &s
	}

	response.OK(c, http.StatusOK, "Run fetched", runDetail{
		runSummary: runSummary{
			ID: row.ID.String(), WorkflowID: row.WorkflowID.String(), Status: row.Status,
			Output: row.Output, CostSoFarUSD: row.CostSoFarUsd, DurationMs: row.DurationMs,
			StartedAt: formatTimestamptz(row.StartedAt), CompletedAt: formatTimestamptz(row.CompletedAt),
			CreatedAt: row.CreatedAt.Time.Format(rfc3339),
		},
		CurrentNode: row.CurrentNode,
		TriggeredBy: triggeredBy, InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
		CachedTokens: row.CachedTokens, ToolCallCount: row.ToolCallCount,
	})
}

func (h *Handler) Cancel(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseRunID(c)
	if !ok {
		return
	}

	// engine.Cancel is process-wide with no org scoping of its own — this lookup is what
	// actually enforces tenant isolation here.
	var exists bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.GetRunStatus(ctx, dbgen.GetRunStatusParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		exists = true
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not look up run")
		return
	}
	if !exists {
		response.Fail(c, http.StatusNotFound, "RUN_NOT_FOUND", "Run not found")
		return
	}

	if !h.engine.Cancel(uuid.UUID(id.Bytes)) {
		response.Fail(c, http.StatusConflict, "RUN_NOT_IN_FLIGHT", "Run is not currently in flight on this instance")
		return
	}
	response.OK(c, http.StatusOK, "Cancellation requested", gin.H{"run_id": id.String()})
}

func (h *Handler) Stream(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseRunID(c)
	if !ok {
		return
	}

	var exists bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.GetRunStatus(ctx, dbgen.GetRunStatusParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		exists = true
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not look up run")
		return
	}
	if !exists {
		response.Fail(c, http.StatusNotFound, "RUN_NOT_FOUND", "Run not found")
		return
	}

	runID := uuid.UUID(id.Bytes)
	events, unsubscribe := h.engine.Bus.Subscribe(runID)
	defer unsubscribe()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	clientGone := c.Request.Context().Done()
	c.Stream(func(w io.Writer) bool {
		select {
		case <-clientGone:
			return false
		case ev, open := <-events:
			if !open {
				return false
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				return true // skip a malformed event, don't kill the stream over it
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
			// Complete/error is the run's natural end — close rather than wait on a channel
			// nothing will ever publish to again.
			return ev.Type != graph.EventComplete && ev.Type != graph.EventError
		}
	})
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

func formatTimestamptz(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.Format(rfc3339)
	return &s
}
