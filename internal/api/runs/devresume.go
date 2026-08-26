package runs

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// DevResumeHandler exposes graph.Launcher.Resume over HTTP — a stand-in
// for workflow 10's real POST /approvals/{id}/approve|reject, which
// doesn't exist yet (no approvals table INSERT, no notification, no
// approval_decisions row — this skips straight to the resume mechanism
// itself). Registered by cmd/api/main.go ONLY when config.MockLLMMode is
// set, so a founder without a real BYOK key yet can still manually
// exercise the full suspend-then-resume approval-gate guardrail against
// a real, running server — see WORKFLOW_PLAN_GO.md's Workflow 9 manual
// verification guide. Never registered otherwise; there is no route to
// disable in production, since main.go only calls Register at all inside
// the same MockLLMMode-gated block that builds the mock Launcher.
type DevResumeHandler struct {
	appPool  *pgxpool.Pool
	launcher *graph.Launcher
}

// NewDevResumeHandler builds a DevResumeHandler.
func NewDevResumeHandler(appPool *pgxpool.Pool, launcher *graph.Launcher) *DevResumeHandler {
	return &DevResumeHandler{appPool: appPool, launcher: launcher}
}

// Register mounts the one dev-only route on rg. rg's group must already
// have middleware.RequireAuth applied — this still requires a real,
// authenticated org member, it just skips the human-review UI/notification
// half workflow 10 will build.
func (h *DevResumeHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/runs/:id/dev-resume", h.Resume)
}

type devResumeRequest struct {
	Approved bool   `json:"approved"`
	Reason   string `json:"reason"`
}

// Resume — POST /api/v1/runs/{id}/dev-resume. Confirms the run belongs to
// the caller's org and is actually awaiting_approval before firing
// Launcher.Resume (fire-and-forget, same as POST /workflows/{id}/run) —
// poll GET /runs/{id} afterward to see the result.
func (h *DevResumeHandler) Resume(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseRunID(c)
	if !ok {
		return
	}
	var req devResumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Expected {\"approved\": bool, \"reason\": string}")
		return
	}

	var status string
	var notFound bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		status, err = q.GetRunStatus(ctx, dbgen.GetRunStatusParams{OrgID: user.OrgID, ID: id})
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
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not look up run")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "RUN_NOT_FOUND", "Run not found")
		return
	}
	if status != "awaiting_approval" {
		response.Fail(c, http.StatusConflict, "RUN_NOT_AWAITING_APPROVAL", "Run is not currently awaiting approval (status: "+status+")")
		return
	}

	h.launcher.Resume(uuid.UUID(user.OrgID.Bytes), uuid.UUID(id.Bytes), req.Approved, req.Reason)
	response.OK(c, http.StatusOK, "Resume requested", gin.H{"run_id": id.String(), "approved": req.Approved})
}
