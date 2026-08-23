// Package workflows implements workflow 8 — workflow configuration CRUD,
// cron scheduling, and pause/resume. "Run Now" and the background
// scheduler both stop at inserting a 'pending' workflow_runs row; nothing
// here calls an LLM, executes a tool, or advances a run past 'pending' —
// that's workflow 9's job (internal/core/graph, not built yet).
package workflows

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	cron "github.com/robfig/cron/v3"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

var validTriggerTypes = map[string]bool{"manual": true, "scheduled": true, "webhook": true}

// standardGraphDefinition is fixed for every workflow — see
// InsertWorkflow's doc comment in internal/db/queries/workflows.sql for
// why this isn't user-configurable.
var standardGraphDefinition = []byte(`{"nodes":["planner","rag_retriever","executor","validator","reporter"]}`)

// Handler implements workflow 8's 6 endpoints.
type Handler struct {
	appPool *pgxpool.Pool
}

// NewHandler builds a Handler. appPool must be the app_user (RLS-enforced)
// pool — every DB operation here goes through tenant.WithTx.
func NewHandler(appPool *pgxpool.Pool) *Handler {
	return &Handler{appPool: appPool}
}

// Register mounts all 6 routes on rg. rg's group must already have
// middleware.RequireAuth applied.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/workflows", h.List)
	rg.POST("/workflows", h.Create)
	rg.GET("/workflows/:id", h.Get)
	rg.PATCH("/workflows/:id", h.Update)
	rg.DELETE("/workflows/:id", h.Delete)
	rg.POST("/workflows/:id/run", h.Run)
}

type workflowView struct {
	ID                     string     `json:"id"`
	AgentID                string     `json:"agent_id"`
	AgentName              string     `json:"agent_name"`
	Name                   string     `json:"name"`
	Description            *string    `json:"description,omitempty"`
	TriggerType            string     `json:"trigger_type"`
	CronExpression         *string    `json:"cron_expression,omitempty"`
	Timezone               string     `json:"timezone"`
	NextRunAt              *time.Time `json:"next_run_at,omitempty"`
	RequiresApproval       bool       `json:"requires_approval"`
	TaskInputTemplate      *string    `json:"task_input_template,omitempty"`
	EstimatedManualMinutes *int32     `json:"estimated_manual_minutes,omitempty"`
	IsActive               bool       `json:"is_active"`
	Version                int32      `json:"version"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

// row is the shape shared by ListWorkflowsRow/GetWorkflowRow/
// InsertWorkflowRow/UpdateWorkflowRow, minus agent_name — Insert/Update
// don't JOIN agents (agent_id is either just-validated or immutable on
// PATCH), so their view conversions take agentName as a separate argument
// instead of a struct field.
type row struct {
	ID                     pgtype.UUID
	AgentID                pgtype.UUID
	Name                   string
	Description            *string
	TriggerType            string
	CronExpression         *string
	Timezone               *string
	NextRunAt              pgtype.Timestamptz
	RequiresApproval       *bool
	TaskInputTemplate      *string
	EstimatedManualMinutes *int32
	IsActive               *bool
	Version                *int32
	CreatedAt              pgtype.Timestamptz
	UpdatedAt              pgtype.Timestamptz
}

func toView(r row, agentName string) workflowView {
	var nextRunAt *time.Time
	if r.NextRunAt.Valid {
		t := r.NextRunAt.Time
		nextRunAt = &t
	}
	return workflowView{
		ID: r.ID.String(), AgentID: r.AgentID.String(), AgentName: agentName,
		Name: r.Name, Description: r.Description, TriggerType: r.TriggerType,
		CronExpression: r.CronExpression, Timezone: derefOr(r.Timezone, "UTC"),
		NextRunAt: nextRunAt, RequiresApproval: derefBool(r.RequiresApproval),
		TaskInputTemplate: r.TaskInputTemplate, EstimatedManualMinutes: r.EstimatedManualMinutes,
		IsActive: derefBool(r.IsActive), Version: derefInt32(r.Version),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

func viewFromList(r dbgen.ListWorkflowsRow) workflowView {
	return toView(row{r.ID, r.AgentID, r.Name, r.Description, r.TriggerType, r.CronExpression,
		r.Timezone, r.NextRunAt, r.RequiresApproval, r.TaskInputTemplate, r.EstimatedManualMinutes,
		r.IsActive, r.Version, r.CreatedAt, r.UpdatedAt}, r.AgentName)
}

func viewFromGet(r dbgen.GetWorkflowRow) workflowView {
	return toView(row{r.ID, r.AgentID, r.Name, r.Description, r.TriggerType, r.CronExpression,
		r.Timezone, r.NextRunAt, r.RequiresApproval, r.TaskInputTemplate, r.EstimatedManualMinutes,
		r.IsActive, r.Version, r.CreatedAt, r.UpdatedAt}, r.AgentName)
}

func viewFromInsert(r dbgen.InsertWorkflowRow, agentName string) workflowView {
	return toView(row{r.ID, r.AgentID, r.Name, r.Description, r.TriggerType, r.CronExpression,
		r.Timezone, r.NextRunAt, r.RequiresApproval, r.TaskInputTemplate, r.EstimatedManualMinutes,
		r.IsActive, r.Version, r.CreatedAt, r.UpdatedAt}, agentName)
}

func viewFromUpdate(r dbgen.UpdateWorkflowRow, agentName string) workflowView {
	return toView(row{r.ID, r.AgentID, r.Name, r.Description, r.TriggerType, r.CronExpression,
		r.Timezone, r.NextRunAt, r.RequiresApproval, r.TaskInputTemplate, r.EstimatedManualMinutes,
		r.IsActive, r.Version, r.CreatedAt, r.UpdatedAt}, agentName)
}

// List returns every workflow for the org, active and paused alike —
// GET /api/v1/workflows.
func (h *Handler) List(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	views := []workflowView{}
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		rows, err := q.ListWorkflows(ctx, user.OrgID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			views = append(views, viewFromList(r))
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list workflows")
		return
	}

	response.OK(c, http.StatusOK, "", views)
}

// Get returns one workflow's detail — GET /api/v1/workflows/{id}.
func (h *Handler) Get(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseWorkflowID(c)
	if !ok {
		return
	}

	var view workflowView
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		r, err := q.GetWorkflow(ctx, dbgen.GetWorkflowParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			return err
		}
		view = viewFromGet(r)
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "Workflow not found")
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch workflow")
		return
	}

	response.OK(c, http.StatusOK, "", view)
}

type createWorkflowRequest struct {
	AgentID                string  `json:"agent_id" binding:"required"`
	Name                   string  `json:"name" binding:"required"`
	Description            *string `json:"description"`
	TriggerType            string  `json:"trigger_type" binding:"required"`
	CronExpression         *string `json:"cron_expression"`
	RequiresApproval       *bool   `json:"requires_approval"`
	TaskInputTemplate      *string `json:"task_input_template"`
	EstimatedManualMinutes *int32  `json:"estimated_manual_minutes"`
}

// Create validates and inserts a new workflow — POST /api/v1/workflows.
func (h *Handler) Create(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var req createWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "agent_id, name, and trigger_type are required")
		return
	}
	agentID, err := uuid.Parse(req.AgentID)
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid agent_id")
		return
	}
	if !validTriggerTypes[req.TriggerType] {
		response.Fail(c, http.StatusBadRequest, "UNKNOWN_TRIGGER_TYPE", "trigger_type must be manual, scheduled, or webhook")
		return
	}

	cronExpr, nextRunAt, code, msg := computeSchedule(req.TriggerType, req.CronExpression)
	if code != "" {
		response.Fail(c, http.StatusBadRequest, code, msg)
		return
	}

	requiresApproval := false
	if req.RequiresApproval != nil {
		requiresApproval = *req.RequiresApproval
	}

	var view workflowView
	var agentNotFound bool
	err = tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		agentRow, err := q.ValidateAgentForOrg(ctx, dbgen.ValidateAgentForOrgParams{
			OrgID: user.OrgID, ID: pgtype.UUID{Bytes: agentID, Valid: true},
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				agentNotFound = true
				return nil
			}
			return err
		}

		r, err := q.InsertWorkflow(ctx, dbgen.InsertWorkflowParams{
			OrgID: user.OrgID, AgentID: agentRow.ID, Name: req.Name, Description: req.Description,
			TriggerType: req.TriggerType, GraphDefinition: standardGraphDefinition,
			CronExpression: cronExpr, NextRunAt: nextRunAt, RequiresApproval: &requiresApproval,
			TaskInputTemplate: req.TaskInputTemplate, EstimatedManualMinutes: req.EstimatedManualMinutes,
			CreatedBy: user.ID,
		})
		if err != nil {
			return err
		}
		view = viewFromInsert(r, agentRow.Name)
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not create workflow")
		return
	}
	if agentNotFound {
		response.Fail(c, http.StatusBadRequest, "AGENT_NOT_FOUND", "agent_id does not refer to an active agent in this org")
		return
	}

	response.OK(c, http.StatusCreated, "Workflow created", view)
}

type updateWorkflowRequest struct {
	Name                   *string `json:"name"`
	Description            *string `json:"description"`
	TriggerType            *string `json:"trigger_type"`
	CronExpression         *string `json:"cron_expression"`
	RequiresApproval       *bool   `json:"requires_approval"`
	TaskInputTemplate      *string `json:"task_input_template"`
	EstimatedManualMinutes *int32  `json:"estimated_manual_minutes"`
	IsActive               *bool   `json:"is_active"`
}

// Update applies a partial update, including pause/resume via is_active —
// PATCH /api/v1/workflows/{id}.
func (h *Handler) Update(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseWorkflowID(c)
	if !ok {
		return
	}

	var req updateWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Malformed request body")
		return
	}

	var view workflowView
	var notFound bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		existing, err := q.GetWorkflow(ctx, dbgen.GetWorkflowParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}

		// trigger_type, cron_expression, and next_run_at are one coupled
		// unit (see workflows.sql's UpdateWorkflow doc comment) — computed
		// together here from the *effective* state (request value if
		// present, existing value otherwise), never independently.
		effectiveTriggerType := existing.TriggerType
		if req.TriggerType != nil {
			if !validTriggerTypes[*req.TriggerType] {
				return errUnknownTriggerType
			}
			effectiveTriggerType = *req.TriggerType
		}
		effectiveCron := req.CronExpression
		if effectiveCron == nil {
			effectiveCron = existing.CronExpression
		}
		cronExpr, nextRunAt, code, msg := computeSchedule(effectiveTriggerType, effectiveCron)
		if code != "" {
			return &validationError{code: code, msg: msg}
		}

		r, err := q.UpdateWorkflow(ctx, dbgen.UpdateWorkflowParams{
			OrgID: user.OrgID, ID: id, Name: req.Name, Description: req.Description,
			TriggerType: &effectiveTriggerType, CronExpression: cronExpr, NextRunAt: nextRunAt,
			RequiresApproval: req.RequiresApproval, TaskInputTemplate: req.TaskInputTemplate,
			EstimatedManualMinutes: req.EstimatedManualMinutes, IsActive: req.IsActive,
		})
		if err != nil {
			return err
		}
		view = viewFromUpdate(r, existing.AgentName)
		return nil
	})
	if err != nil {
		var verr *validationError
		if errors.As(err, &verr) {
			response.Fail(c, http.StatusBadRequest, verr.code, verr.msg)
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not update workflow")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "Workflow not found")
		return
	}

	response.OK(c, http.StatusOK, "Workflow updated", view)
}

// Delete pauses a workflow (is_active=false) — DELETE /api/v1/workflows/{id}.
// Semantically identical to PATCH {is_active: false}; see
// internal/db/queries/workflows.sql's DeactivateWorkflow doc comment for
// why workflows don't have a separate "really gone" state.
func (h *Handler) Delete(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseWorkflowID(c)
	if !ok {
		return
	}

	var rowsAffected int64
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		n, err := q.DeactivateWorkflow(ctx, dbgen.DeactivateWorkflowParams{OrgID: user.OrgID, ID: id})
		rowsAffected = n
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not delete workflow")
		return
	}
	if rowsAffected == 0 {
		response.Fail(c, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "Workflow not found")
		return
	}

	c.Status(http.StatusNoContent)
}

// Run queues a workflow run — POST /api/v1/workflows/{id}/run. Only
// inserts a 'pending' workflow_runs row; nothing processes it yet (see
// package doc comment). A founder clicking "Run Now" today gets a real,
// durable queued-run record, not a fake success — workflow 9 is what will
// make it actually execute.
func (h *Handler) Run(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseWorkflowID(c)
	if !ok {
		return
	}

	var runID string
	var notFound bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		wf, err := q.GetWorkflow(ctx, dbgen.GetWorkflowParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}
		run, err := q.InsertWorkflowRun(ctx, dbgen.InsertWorkflowRunParams{
			WorkflowID: wf.ID, OrgID: user.OrgID, TriggeredBy: user.ID,
		})
		if err != nil {
			return err
		}
		runID = run.ID.String()
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not queue run")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "WORKFLOW_NOT_FOUND", "Workflow not found")
		return
	}

	response.OK(c, http.StatusAccepted, "Run queued", gin.H{"run_id": runID, "status": "pending"})
}

// computeSchedule validates triggerType and, for "scheduled", parses
// cronExpr and computes the next fire time. Returns ("", "") for the
// error fields when valid. cronExpr/nextRunAt come back nil for
// non-scheduled trigger types — a manual or webhook workflow has no
// schedule to track.
func computeSchedule(triggerType string, cronExpr *string) (*string, pgtype.Timestamptz, string, string) {
	if triggerType != "scheduled" {
		return nil, pgtype.Timestamptz{}, "", ""
	}
	if cronExpr == nil || *cronExpr == "" {
		return nil, pgtype.Timestamptz{}, "CRON_EXPRESSION_REQUIRED", "cron_expression is required when trigger_type is scheduled"
	}
	schedule, err := cron.ParseStandard(*cronExpr)
	if err != nil {
		return nil, pgtype.Timestamptz{}, "INVALID_CRON_EXPRESSION", fmt.Sprintf("Invalid cron_expression: %v", err)
	}
	next := schedule.Next(time.Now())
	return cronExpr, pgtype.Timestamptz{Time: next, Valid: true}, "", ""
}

type validationError struct {
	code string
	msg  string
}

func (e *validationError) Error() string { return e.msg }

var errUnknownTriggerType = &validationError{code: "UNKNOWN_TRIGGER_TYPE", msg: "trigger_type must be manual, scheduled, or webhook"}

func parseWorkflowID(c *gin.Context) (pgtype.UUID, bool) {
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid workflow id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

func derefOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

func derefInt32(n *int32) int32 {
	if n == nil {
		return 0
	}
	return *n
}

func derefBool(b *bool) bool {
	return b != nil && *b
}
