package teams

import (
	"context"
	"errors"
	"net/http"
	"strings"

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

const rfc3339 = "2006-01-02T15:04:05Z07:00"

func formatTimestamptz(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.Format(rfc3339)
	return &s
}

type Handler struct {
	appPool  *pgxpool.Pool
	launcher *graph.Launcher
}

func NewHandler(appPool *pgxpool.Pool, launcher *graph.Launcher) *Handler {
	return &Handler{appPool: appPool, launcher: launcher}
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/teams", h.List)
	rg.POST("/teams", h.Create)
	rg.GET("/teams/:id", h.Get)
	rg.DELETE("/teams/:id", h.Delete)
	rg.POST("/teams/:id/run", h.Run)
	rg.GET("/teams/:id/runs", h.ListRuns)
	rg.GET("/teams/:id/runs/:run_id", h.Trace)
}

func parseTeamID(c *gin.Context) (pgtype.UUID, bool) {
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid team id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

type teamMember struct {
	ID          string `json:"id"`
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name"`
	Description string `json:"agent_description,omitempty"`
	Role        string `json:"role"`
	Priority    int32  `json:"priority"`
}

type teamSummary struct {
	ID                    string  `json:"id"`
	Name                  string  `json:"name"`
	Description           *string `json:"description,omitempty"`
	OrchestratorAgentID   string  `json:"orchestrator_agent_id"`
	OrchestratorAgentName string  `json:"orchestrator_agent_name"`
	ParallelExecution     bool    `json:"parallel_execution"`
	TimeoutSeconds        int32   `json:"timeout_seconds"`
	MemberCount           *int64  `json:"member_count,omitempty"`
	CreatedAt             string  `json:"created_at"`
}

func (h *Handler) List(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var rows []dbgen.ListAgentTeamsForOrgRow
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListAgentTeamsForOrg(ctx, user.OrgID)
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list teams")
		return
	}

	out := make([]teamSummary, 0, len(rows))
	for _, r := range rows {
		memberCount := r.MemberCount
		out = append(out, teamSummary{
			ID: r.ID.String(), Name: r.Name, Description: r.Description,
			OrchestratorAgentID: r.OrchestratorAgentID.String(), OrchestratorAgentName: r.OrchestratorAgentName,
			ParallelExecution: boolOr(r.ParallelExecution, true), TimeoutSeconds: int32Or(r.TimeoutSeconds, 600),
			MemberCount: &memberCount, CreatedAt: r.CreatedAt.Time.Format(rfc3339),
		})
	}
	response.OK(c, http.StatusOK, "Teams fetched", out)
}

type createTeamMemberRequest struct {
	AgentID  string `json:"agent_id" binding:"required"`
	Role     string `json:"role" binding:"required"`
	Priority *int32 `json:"priority"`
}

type createTeamRequest struct {
	Name                string                    `json:"name" binding:"required"`
	Description         *string                   `json:"description"`
	OrchestratorAgentID string                    `json:"orchestrator_agent_id" binding:"required"`
	Members             []createTeamMemberRequest `json:"members" binding:"required,min=1"`
}

// Create builds a team and its member roster in one transaction, plus the
// one companion `workflows` row POST /teams/{id}/run and every specialist
func (h *Handler) Create(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.CanModifyAgents() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can create agent teams")
		return
	}

	var req createTeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "name, orchestrator_agent_id, and at least one member are required")
		return
	}
	orchestratorAgentID, err := uuid.Parse(req.OrchestratorAgentID)
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid orchestrator_agent_id")
		return
	}
	roleSeen := make(map[string]bool, len(req.Members))
	for _, m := range req.Members {
		if _, err := uuid.Parse(m.AgentID); err != nil {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid member agent_id")
			return
		}
		role := strings.TrimSpace(m.Role)
		if role == "" {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Every member needs a non-empty role")
			return
		}
		if roleSeen[role] {
			response.Fail(c, http.StatusBadRequest, "DUPLICATE_TEAM_ROLE", "Two members cannot share the role \""+role+"\" — the orchestrator dispatches subtasks by role")
			return
		}
		roleSeen[role] = true
	}

	orchestratorPg := pgtype.UUID{Bytes: orchestratorAgentID, Valid: true}
	var team dbgen.InsertAgentTeamRow
	var invalidAgentID string
	err = tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		if _, err := q.ValidateAgentForTeamMembership(ctx, dbgen.ValidateAgentForTeamMembershipParams{OrgID: user.OrgID, ID: orchestratorPg}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				invalidAgentID = req.OrchestratorAgentID
				return nil
			}
			return err
		}

		var err error
		team, err = q.InsertAgentTeam(ctx, dbgen.InsertAgentTeamParams{
			OrgID: user.OrgID, Name: req.Name, Description: req.Description, OrchestratorAgentID: orchestratorPg,
		})
		if err != nil {
			return err
		}

		for _, m := range req.Members {
			agentID, _ := uuid.Parse(m.AgentID) // already validated above
			agentPg := pgtype.UUID{Bytes: agentID, Valid: true}
			if _, err := q.ValidateAgentForTeamMembership(ctx, dbgen.ValidateAgentForTeamMembershipParams{OrgID: user.OrgID, ID: agentPg}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					invalidAgentID = m.AgentID
					return nil
				}
				return err
			}
			if _, err := q.InsertAgentTeamMember(ctx, dbgen.InsertAgentTeamMemberParams{
				TeamID: team.ID, AgentID: agentPg, Role: strings.TrimSpace(m.Role), Priority: m.Priority,
			}); err != nil {
				return err
			}
		}

		_, err = q.InsertTeamWorkflow(ctx, dbgen.InsertTeamWorkflowParams{
			OrgID: user.OrgID, AgentID: orchestratorPg, TeamID: team.ID, Name: req.Name, Description: req.Description, CreatedBy: user.ID,
		})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not create team")
		return
	}
	if invalidAgentID != "" {
		response.Fail(c, http.StatusBadRequest, "AGENT_NOT_FOUND", "Agent "+invalidAgentID+" does not exist, is inactive, or belongs to another organization")
		return
	}

	response.OK(c, http.StatusCreated, "Team created", teamSummary{
		ID: team.ID.String(), Name: team.Name, Description: team.Description,
		OrchestratorAgentID: team.OrchestratorAgentID.String(), OrchestratorAgentName: "", // the just-created row has no self-join; List/Get populate this
		ParallelExecution: boolOr(team.ParallelExecution, true), TimeoutSeconds: int32Or(team.TimeoutSeconds, 600),
		CreatedAt: team.CreatedAt.Time.Format(rfc3339),
	})
}

type teamDetail struct {
	teamSummary
	Members []teamMember `json:"members"`
}

func (h *Handler) Get(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseTeamID(c)
	if !ok {
		return
	}

	var team dbgen.GetAgentTeamRow
	var memberRows []dbgen.ListAgentTeamMembersRow
	var notFound bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		team, err = q.GetAgentTeam(ctx, dbgen.GetAgentTeamParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}
		memberRows, err = q.ListAgentTeamMembers(ctx, id)
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch team")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "TEAM_NOT_FOUND", "Team not found")
		return
	}

	members := make([]teamMember, 0, len(memberRows))
	for _, m := range memberRows {
		desc := ""
		if m.AgentDescription != nil {
			desc = *m.AgentDescription
		}
		members = append(members, teamMember{
			ID: m.ID.String(), AgentID: m.AgentID.String(), AgentName: m.AgentName, Description: desc,
			Role: m.Role, Priority: int32Or(m.Priority, 0),
		})
	}

	response.OK(c, http.StatusOK, "Team fetched", teamDetail{
		teamSummary: teamSummary{
			ID: team.ID.String(), Name: team.Name, Description: team.Description,
			OrchestratorAgentID: team.OrchestratorAgentID.String(), OrchestratorAgentName: team.OrchestratorAgentName,
			ParallelExecution: boolOr(team.ParallelExecution, true), TimeoutSeconds: int32Or(team.TimeoutSeconds, 600),
			CreatedAt: team.CreatedAt.Time.Format(rfc3339),
		},
		Members: members,
	})
}

func (h *Handler) Delete(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.CanModifyAgents() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can delete agent teams")
		return
	}
	id, ok := parseTeamID(c)
	if !ok {
		return
	}

	var rowsAffected int64
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rowsAffected, err = q.DeactivateAgentTeam(ctx, dbgen.DeactivateAgentTeamParams{OrgID: user.OrgID, ID: id})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not delete team")
		return
	}
	if rowsAffected == 0 {
		response.Fail(c, http.StatusNotFound, "TEAM_NOT_FOUND", "Team not found")
		return
	}
	response.OK(c, http.StatusOK, "Team deleted", nil)
}

type runTeamRequest struct {
	Input string `json:"input" binding:"required"`
}

// runQueuedResponse matches workflows.Handler.Run's own {run_id, status,
// stream_url} shape exactly — same underlying GET /runs/{id}/stream SSE
// endpoint, since the orchestrator's own run is an ordinary workflow_runs
// row like any other.
type runQueuedResponse struct {
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	StreamURL string `json:"stream_url"`
}

// Run is POST /teams/{id}/run's handler — same preflight-then-insert-
// then-launch shape as workflows.Handler.Run, but resolves the team's
// full member roster first (graph.TeamMember) and calls
// Launcher.LaunchTeam instead of Launch.
func (h *Handler) Run(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.CanTriggerWorkflows() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Viewers cannot trigger team runs")
		return
	}
	id, ok := parseTeamID(c)
	if !ok {
		return
	}
	var req runTeamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "input is required")
		return
	}

	if err := h.launcher.Preflight(c.Request.Context(), user.OrgID); err != nil {
		var pe *graph.PreflightError
		if errors.As(err, &pe) {
			response.Fail(c, http.StatusBadRequest, pe.Code, pe.Message)
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not check run preflight")
		return
	}

	var run dbgen.InsertTeamWorkflowRunRow
	var orchestratorAgentID uuid.UUID
	var members []graph.TeamMember
	var notFound bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		team, err := q.GetAgentTeam(ctx, dbgen.GetAgentTeamParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}
		workflowID, err := q.GetTeamWorkflow(ctx, dbgen.GetTeamWorkflowParams{OrgID: user.OrgID, TeamID: id})
		if err != nil {
			return err
		}
		memberRows, err := q.ListAgentTeamMembers(ctx, id)
		if err != nil {
			return err
		}
		if len(memberRows) == 0 {
			return errNoMembers
		}
		for _, m := range memberRows {
			members = append(members, graph.TeamMember{
				AgentID: uuid.UUID(m.AgentID.Bytes), Name: m.AgentName, Description: derefOr(m.AgentDescription, ""), Role: m.Role,
			})
		}

		orchestratorAgentID = uuid.UUID(team.OrchestratorAgentID.Bytes)
		run, err = q.InsertTeamWorkflowRun(ctx, dbgen.InsertTeamWorkflowRunParams{
			WorkflowID: workflowID, OrgID: user.OrgID, TriggeredBy: user.ID, AgentID: team.OrchestratorAgentID,
		})
		return err
	})
	if err != nil {
		if errors.Is(err, errNoMembers) {
			response.Fail(c, http.StatusBadRequest, "TEAM_HAS_NO_MEMBERS", "This team has no specialist members to delegate to")
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not queue team run")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "TEAM_NOT_FOUND", "Team not found")
		return
	}

	h.launcher.LaunchTeam(uuid.UUID(user.OrgID.Bytes), orchestratorAgentID, uuid.UUID(run.ID.Bytes), members, req.Input)

	response.OK(c, http.StatusAccepted, "Team run queued", runQueuedResponse{
		RunID: run.ID.String(), Status: run.Status, StreamURL: "/api/v1/runs/" + run.ID.String() + "/stream",
	})
}

var errNoMembers = errors.New("teams: team has no members")

type childRun struct {
	ID           string  `json:"id"`
	AgentID      string  `json:"agent_id"`
	AgentName    string  `json:"agent_name"`
	Role         *string `json:"role,omitempty"`
	Status       string  `json:"status"`
	CurrentNode  *string `json:"current_node,omitempty"`
	Output       *string `json:"output,omitempty"`
	CostSoFarUSD float64 `json:"cost_so_far_usd"`
	InputTokens  int32   `json:"input_tokens"`
	OutputTokens int32   `json:"output_tokens"`
	StartedAt    *string `json:"started_at,omitempty"`
	CompletedAt  *string `json:"completed_at,omitempty"`
	DurationMs   *int32  `json:"duration_ms,omitempty"`
}

type teamRunTrace struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	CurrentNode  *string    `json:"current_node,omitempty"`
	Output       *string    `json:"output,omitempty"`
	CostSoFarUSD float64    `json:"cost_so_far_usd"`
	StartedAt    *string    `json:"started_at,omitempty"`
	CompletedAt  *string    `json:"completed_at,omitempty"`
	DurationMs   *int32     `json:"duration_ms,omitempty"`
	CreatedAt    string     `json:"created_at"`
	Specialists  []childRun `json:"specialists"`
}

type teamRunSummary struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	Output       *string `json:"output,omitempty"`
	CostSoFarUSD float64 `json:"cost_so_far_usd"`
	StartedAt    *string `json:"started_at,omitempty"`
	CompletedAt  *string `json:"completed_at,omitempty"`
	DurationMs   *int32  `json:"duration_ms,omitempty"`
	CreatedAt    string  `json:"created_at"`
}

const defaultRunsLimit = 20

// ListRuns is GET /teams/{id}/runs — one team's own run history. Added
// after a real, reported gap: once a founder navigated away from a run
// they'd just triggered, there was no way back to it short of still
// having that exact URL — GET /teams/{id} itself never returned anything
// about past runs. Each row here links to GET /teams/{id}/runs/{run_id}
// for the full aggregated trace; this endpoint only returns the summary
// fields the same list-page card layout every other resource in this app
// already uses (see internal/api/runs.Handler.List's runSummary).
func (h *Handler) ListRuns(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseTeamID(c)
	if !ok {
		return
	}

	var rows []dbgen.ListTeamRunsRow
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListTeamRuns(ctx, dbgen.ListTeamRunsParams{
			OrgID: user.OrgID, TeamID: id, Limit: defaultRunsLimit, Offset: 0,
		})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list team runs")
		return
	}

	out := make([]teamRunSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, teamRunSummary{
			ID: r.ID.String(), Status: r.Status, Output: r.Output, CostSoFarUSD: r.CostSoFarUsd,
			StartedAt: formatTimestamptz(r.StartedAt), CompletedAt: formatTimestamptz(r.CompletedAt),
			DurationMs: r.DurationMs, CreatedAt: r.CreatedAt.Time.Format(rfc3339),
		})
	}
	response.OK(c, http.StatusOK, "Team runs listed", out)
}

// Trace is GET /teams/{id}/runs/{run_id} — the orchestrator's own run
// detail plus every specialist sub-run it dispatched, for the multi-agent
// pipeline UI's collapsible per-specialist sub-timelines. Doesn't confirm
// run_id actually belongs to team id (ListChildRuns/GetRunDetail are
// already org-scoped, which is the real tenant boundary here) — id is
// used only to keep the URL shape symmetric with the rest of this
// package's team-scoped routes.
func (h *Handler) Trace(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if _, ok := parseTeamID(c); !ok {
		return
	}
	runID, err := uuid.Parse(c.Param("run_id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid run id")
		return
	}
	runPg := pgtype.UUID{Bytes: runID, Valid: true}

	var run dbgen.GetRunDetailRow
	var children []dbgen.ListChildRunsRow
	var notFound bool
	err = tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		run, err = q.GetRunDetail(ctx, dbgen.GetRunDetailParams{OrgID: user.OrgID, ID: runPg})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}
		children, err = q.ListChildRuns(ctx, dbgen.ListChildRunsParams{OrgID: user.OrgID, ParentRunID: runPg})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch team run trace")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "RUN_NOT_FOUND", "Run not found")
		return
	}

	specialists := make([]childRun, 0, len(children))
	for _, r := range children {
		specialists = append(specialists, childRun{
			ID: r.ID.String(), AgentID: r.AgentID.String(), AgentName: r.AgentName, Role: r.DelegatedRole,
			Status: r.Status, CurrentNode: r.CurrentNode, Output: r.Output, CostSoFarUSD: r.CostSoFarUsd,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			StartedAt: formatTimestamptz(r.StartedAt), CompletedAt: formatTimestamptz(r.CompletedAt), DurationMs: r.DurationMs,
		})
	}

	response.OK(c, http.StatusOK, "Team run trace fetched", teamRunTrace{
		ID: run.ID.String(), Status: run.Status, CurrentNode: run.CurrentNode, Output: run.Output, CostSoFarUSD: run.CostSoFarUsd,
		StartedAt: formatTimestamptz(run.StartedAt), CompletedAt: formatTimestamptz(run.CompletedAt), DurationMs: run.DurationMs,
		CreatedAt: run.CreatedAt.Time.Format(rfc3339), Specialists: specialists,
	})
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func int32Or(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
