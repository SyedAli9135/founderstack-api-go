package a2a

import (
	"context"
	"encoding/json"
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
	corea2a "github.com/founderstack/api/internal/core/a2a"
	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

type Handler struct {
	appPool  *pgxpool.Pool
	launcher *graph.Launcher
	tokens   *corea2a.TaskTokenSigner
	baseURL  string
}

func NewHandler(appPool *pgxpool.Pool, launcher *graph.Launcher, tokens *corea2a.TaskTokenSigner, baseURL string) *Handler {
	return &Handler{appPool: appPool, launcher: launcher, tokens: tokens, baseURL: baseURL}
}

// Register wires the manifest route — call under a middleware.RequireAuth-gated group
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/a2a/agents/:agent_id/.well-known/agent.json", h.Manifest)
}

func (h *Handler) RegisterTasksSend(rg *gin.RouterGroup) {
	rg.POST("/a2a/agents/:agent_id/tasks/send", h.TasksSend)
}

func parseAgentID(c *gin.Context) (pgtype.UUID, uuid.UUID, bool) {
	parsed, err := uuid.Parse(c.Param("agent_id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid agent id")
		return pgtype.UUID{}, uuid.Nil, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, parsed, true
}

// Manifest is GET /.well-known/agent.json for one specialist agent — the
// A2A spec's discovery endpoint, path-scoped per agent (not host-root)
func (h *Handler) Manifest(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	agentPg, agentID, ok := parseAgentID(c)
	if !ok {
		return
	}

	var agentRow dbgen.GetAgentForA2AManifestRow
	var onTeam bool
	var notFound bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		agentRow, err = q.GetAgentForA2AManifest(ctx, dbgen.GetAgentForA2AManifestParams{OrgID: user.OrgID, ID: agentPg})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}
		onTeam, err = q.IsAgentOnActiveTeam(ctx, dbgen.IsAgentOnActiveTeamParams{AgentID: agentPg, OrgID: user.OrgID})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch agent card")
		return
	}
	if notFound || !onTeam {
		response.Fail(c, http.StatusNotFound, "AGENT_NOT_FOUND", "Agent not found or not a member of any active team")
		return
	}

	var policy struct {
		AllowedTools []string `json:"allowed_tools"`
	}
	_ = json.Unmarshal(agentRow.PolicyScope, &policy) // malformed/empty policy_scope just yields an empty skills list, not a 500

	url := h.baseURL + "/api/v1/a2a/agents/" + agentID.String() + "/tasks/send"
	model := ""
	if agentRow.Model != nil {
		model = *agentRow.Model
	}
	desc := ""
	if agentRow.Description != nil {
		desc = *agentRow.Description
	}
	version := int32(1)
	if agentRow.Version != nil {
		version = *agentRow.Version
	}

	c.JSON(http.StatusOK, corea2a.BuildAgentCard(agentRow.Name, desc, model, url, policy.AllowedTools, version))
}

func (h *Handler) TasksSend(c *gin.Context) {
	_, agentID, ok := parseAgentID(c)
	if !ok {
		return
	}

	token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if token == "" {
		respondRPCError(c, http.StatusUnauthorized, "", -32000, "Missing Authorization bearer token")
		return
	}
	orgID, err := h.tokens.Verify(token, agentID)
	if err != nil {
		respondRPCError(c, http.StatusUnauthorized, "", -32000, "Invalid or expired task token")
		return
	}
	orgPg := pgtype.UUID{Bytes: orgID, Valid: true}
	agentPg := pgtype.UUID{Bytes: agentID, Valid: true}

	var req corea2a.TaskSendRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Method != "tasks/send" || req.Params.ID == "" || req.Params.SessionID == "" {
		respondRPCError(c, http.StatusBadRequest, "", -32600, "Malformed tasks/send request")
		return
	}
	taskID, err := uuid.Parse(req.Params.ID)
	if err != nil {
		respondRPCError(c, http.StatusBadRequest, req.ID, -32602, "Invalid task id")
		return
	}
	parentRunID, err := uuid.Parse(req.Params.SessionID)
	if err != nil {
		respondRPCError(c, http.StatusBadRequest, req.ID, -32602, "Invalid sessionId")
		return
	}
	parentRunPg := pgtype.UUID{Bytes: parentRunID, Valid: true}

	var childRun dbgen.InsertTeamWorkflowRunRow
	var forbidden bool
	var notFound bool
	err = tenant.WithTx(c.Request.Context(), h.appPool, orgPg, func(ctx context.Context, q *dbgen.Queries) error {
		teamAndWorkflow, err := q.GetRunTeamAndWorkflow(ctx, dbgen.GetRunTeamAndWorkflowParams{OrgID: orgPg, ID: parentRunPg})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}
		if !teamAndWorkflow.TeamID.Valid {
			notFound = true
			return nil
		}

		role, err := q.GetAgentTeamMemberRole(ctx, dbgen.GetAgentTeamMemberRoleParams{TeamID: teamAndWorkflow.TeamID, AgentID: agentPg})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				forbidden = true
				return nil
			}
			return err
		}
		delegatedRole := role

		taskPg := pgtype.UUID{Bytes: taskID, Valid: true}
		childRun, err = q.InsertTeamWorkflowRun(ctx, dbgen.InsertTeamWorkflowRunParams{
			ID: taskPg, WorkflowID: teamAndWorkflow.WorkflowID, OrgID: orgPg,
			AgentID: agentPg, ParentRunID: parentRunPg, DelegatedRole: &delegatedRole,
		})
		return err
	})
	if err != nil {
		respondRPCError(c, http.StatusInternalServerError, req.ID, -32603, "Could not create specialist run")
		return
	}
	if notFound {
		respondRPCError(c, http.StatusNotFound, req.ID, -32001, "Dispatching run not found or is not a team run")
		return
	}
	if forbidden {
		respondRPCError(c, http.StatusForbidden, req.ID, -32002, "Agent is not a member of this dispatching run's team")
		return
	}

	input := ""
	for _, part := range req.Params.Message.Parts {
		if part.Type == "text" {
			input += part.Text
		}
	}

	output, runErr := h.launcher.RunSpecialist(c.Request.Context(), orgID, agentID, uuid.UUID(childRun.ID.Bytes), input)
	state := "completed"
	if runErr != nil {
		state = "failed"
		if output == "" {
			output = runErr.Error()
		}
	}

	c.JSON(http.StatusOK, corea2a.TaskSendResponse{
		JSONRPC: "2.0", ID: req.ID,
		Result: &corea2a.Task{
			ID:     req.Params.ID,
			Status: corea2a.TaskStatus{State: state},
			Artifacts: []corea2a.Artifact{{
				Name:  "result",
				Parts: []corea2a.Part{{Type: "text", Text: output}},
			}},
		},
	})
}

func respondRPCError(c *gin.Context, httpStatus int, rpcID string, code int, message string) {
	c.JSON(httpStatus, corea2a.TaskSendResponse{
		JSONRPC: "2.0", ID: rpcID,
		Error: &corea2a.JSONRPCError{Code: code, Message: message},
	})
}
