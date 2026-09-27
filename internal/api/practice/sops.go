package practice

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	cron "github.com/robfig/cron/v3"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	corebilling "github.com/founderstack/api/internal/core/billing"
	"github.com/founderstack/api/internal/core/sop"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// ToolCatalog is the slice of the MCP registry SOP validation needs.
type ToolCatalog interface {
	ListTools(ctx context.Context) (map[string][]*gomcp.Tool, error)
}

// Same fixed pipeline internal/api/workflows gives every workflow.
var standardGraphDefinition = []byte(`{"nodes":["planner","rag_retriever","executor","validator","reporter"]}`)

func (h *Handler) registerSops(rg *gin.RouterGroup) {
	rg.GET("/practice/sops", h.ListSops)
	rg.POST("/practice/sops", h.CreateSop)
	rg.GET("/practice/sops/tools", h.ListSopTools)
	rg.GET("/practice/sops/:id", h.GetSop)
	rg.PATCH("/practice/sops/:id", h.UpdateSop)
	rg.DELETE("/practice/sops/:id", h.DeleteSop)
	rg.POST("/practice/sops/:id/deploy", h.DeploySop)
	rg.GET("/practice/sops/:id/deployments", h.ListSopDeployments)
	rg.PATCH("/practice/sops/:id/deployments/:deployment_id", h.UpdateSopDeployment)
	rg.POST("/practice/sops/:id/deployments/:deployment_id/sync", h.SyncSopDeployment)
	rg.DELETE("/practice/sops/:id/deployments/:deployment_id", h.UndeploySop)
}

// ---- views ----

type sopSummary struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Description          *string  `json:"description"`
	Category             *string  `json:"category"`
	CurrentVersion       int32    `json:"current_version"`
	AgentName            string   `json:"agent_name"`
	WorkflowName         *string  `json:"workflow_name"`
	TriggerType          *string  `json:"trigger_type"`
	RequiredIntegrations []string `json:"required_integrations"`
	ParameterCount       int      `json:"parameter_count"`
	ActiveDeployments    int64    `json:"active_deployments"`
	OutdatedDeployments  int64    `json:"outdated_deployments"`
	UpdatedAt            string   `json:"updated_at"`
}

type sopVersion struct {
	Version   int32   `json:"version"`
	Changelog *string `json:"changelog"`
	CreatedAt string  `json:"created_at"`
}

type sopDetail struct {
	sopSummary
	AgentConfig    sop.AgentConfig     `json:"agent_config"`
	WorkflowConfig *sop.WorkflowConfig `json:"workflow_config"`
	Parameters     []sop.Parameter     `json:"parameters"`
	Versions       []sopVersion        `json:"versions"`
}

type deploymentView struct {
	ID                  string        `json:"id"`
	WorkspaceID         string        `json:"workspace_id"`
	WorkspaceName       string        `json:"workspace_name"`
	DeployedVersion     int32         `json:"deployed_version"`
	CurrentVersion      int32         `json:"current_version"`
	Status              string        `json:"status"`
	ParameterOverrides  sop.Overrides `json:"parameter_overrides"`
	MissingIntegrations []string      `json:"missing_integrations"`
	AgentID             string        `json:"agent_id"`
	WorkflowID          *string       `json:"workflow_id"`
	DeployedAt          string        `json:"deployed_at"`
	SyncedAt            string        `json:"synced_at"`
}

// deploymentStatus: "broken" when the client removed the agent (sync can't
// repair that — remove and redeploy), else whether the version is current.
func deploymentStatus(agentActive bool, deployed, current int32) string {
	switch {
	case !agentActive:
		return "broken"
	case deployed < current:
		return "update_available"
	default:
		return "up_to_date"
	}
}

func summarize(id pgtype.UUID, name string, description, category *string, version int32, spec sop.Spec, active, outdated int64, updated pgtype.Timestamptz) sopSummary {
	s := sopSummary{
		ID: id.String(), Name: name, Description: description, Category: category, CurrentVersion: version,
		AgentName: spec.Agent.Name, RequiredIntegrations: sop.RequiredServices(spec.Agent),
		ParameterCount: len(spec.Parameters), ActiveDeployments: active, OutdatedDeployments: outdated,
		UpdatedAt: updated.Time.Format(time.RFC3339),
	}
	if spec.Workflow != nil {
		s.WorkflowName = &spec.Workflow.Name
		s.TriggerType = &spec.Workflow.TriggerType
	}
	return s
}

// ---- spec (de)serialization ----

func decodeSpec(agentConfig, workflowConfig, parameters []byte) (sop.Spec, error) {
	var s sop.Spec
	if err := json.Unmarshal(agentConfig, &s.Agent); err != nil {
		return s, err
	}
	if len(workflowConfig) > 0 && string(workflowConfig) != "null" {
		s.Workflow = &sop.WorkflowConfig{}
		if err := json.Unmarshal(workflowConfig, s.Workflow); err != nil {
			return s, err
		}
	}
	if len(parameters) > 0 {
		if err := json.Unmarshal(parameters, &s.Parameters); err != nil {
			return s, err
		}
	}
	if s.Parameters == nil {
		s.Parameters = []sop.Parameter{}
	}
	return s, nil
}

func encodeSpec(s sop.Spec) (agentJSON, workflowJSON, paramsJSON []byte, err error) {
	if agentJSON, err = json.Marshal(s.Agent); err != nil {
		return
	}
	if s.Workflow != nil {
		if workflowJSON, err = json.Marshal(s.Workflow); err != nil {
			return
		}
	}
	if s.Parameters == nil {
		s.Parameters = []sop.Parameter{}
	}
	paramsJSON, err = json.Marshal(s.Parameters)
	return
}

func (h *Handler) knownTools(ctx context.Context) (map[string]bool, error) {
	byService, err := h.tools.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for service, tools := range byService {
		for _, t := range tools {
			known[service+"."+t.Name] = true
		}
	}
	return known, nil
}

// ---- shared request plumbing ----

// sopContext resolves the caller's practice; admin=true additionally
// requires owner/admin there (every mutation).
func (h *Handler) sopContext(c *gin.Context, admin bool) (authctx.User, practiceContext, bool) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return user, practiceContext{}, false
	}
	p, ok := h.resolvePractice(c, user)
	if !ok {
		return user, p, false
	}
	if admin && !p.callerIsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only a practice owner or admin can manage SOPs")
		return user, p, false
	}
	return user, p, true
}

func parseUUIDParam(c *gin.Context, name, label string) (pgtype.UUID, bool) {
	parsed, err := uuid.Parse(c.Param(name))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid "+label)
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

func (h *Handler) loadSop(c *gin.Context, practiceID pgtype.UUID) (dbgen.GetSopPlaybookRow, sop.Spec, bool) {
	id, ok := parseUUIDParam(c, "id", "SOP id")
	if !ok {
		return dbgen.GetSopPlaybookRow{}, sop.Spec{}, false
	}
	row, err := dbgen.New(h.systemPool).GetSopPlaybook(c.Request.Context(), dbgen.GetSopPlaybookParams{ID: id, PracticeID: practiceID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "SOP_NOT_FOUND", "SOP not found")
		} else {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load SOP")
		}
		return row, sop.Spec{}, false
	}
	spec, err := decodeSpec(row.AgentConfig, row.WorkflowConfig, row.Parameters)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read SOP config")
		return row, spec, false
	}
	return row, spec, true
}

func failSopError(c *gin.Context, err error, fallback string) {
	var se *sop.Error
	if errors.As(err, &se) {
		response.Fail(c, http.StatusBadRequest, se.Code, se.Message)
		return
	}
	response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", fallback)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---- library ----

func (h *Handler) ListSops(c *gin.Context) {
	user, p, ok := h.sopContext(c, false)
	if !ok {
		return
	}
	rows, err := dbgen.New(h.systemPool).ListSopPlaybooks(c.Request.Context(), dbgen.ListSopPlaybooksParams{
		ClerkUserID: user.ClerkUserID, PracticeID: p.id,
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list SOPs")
		return
	}
	out := make([]sopSummary, 0, len(rows))
	for _, r := range rows {
		spec, err := decodeSpec(r.AgentConfig, r.WorkflowConfig, r.Parameters)
		if err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read SOP config")
			return
		}
		out = append(out, summarize(r.ID, r.Name, r.Description, r.Category, r.CurrentVersion, spec, r.ActiveDeployments, r.OutdatedDeployments, r.UpdatedAt))
	}
	response.OK(c, http.StatusOK, "SOPs listed", gin.H{"sops": out, "can_manage": p.callerIsOwnerOrAdmin()})
}

type toolOption struct {
	Service     string `json:"service"`
	Name        string `json:"name"`
	ToolID      string `json:"tool_id"`
	Description string `json:"description"`
}

// ListSopTools returns the full tool catalog, not just what the current
// workspace has connected — a SOP is authored once for many clients, and
// each deployment reports its own missing integrations instead.
func (h *Handler) ListSopTools(c *gin.Context) {
	if _, _, ok := h.sopContext(c, false); !ok {
		return
	}
	byService, err := h.tools.ListTools(c.Request.Context())
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load tool catalog")
		return
	}
	out := []toolOption{}
	for service, tools := range byService {
		for _, t := range tools {
			out = append(out, toolOption{Service: service, Name: t.Name, ToolID: service + "." + t.Name, Description: t.Description})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ToolID < out[j].ToolID })
	response.OK(c, http.StatusOK, "", out)
}

func (h *Handler) GetSop(c *gin.Context) {
	_, p, ok := h.sopContext(c, false)
	if !ok {
		return
	}
	row, spec, ok := h.loadSop(c, p.id)
	if !ok {
		return
	}
	h.respondDetail(c, http.StatusOK, "SOP fetched", p.id, row.ID, spec)
}

// respondDetail re-reads the list row (for deployment counts) and versions.
func (h *Handler) respondDetail(c *gin.Context, status int, message string, practiceID, id pgtype.UUID, spec sop.Spec) {
	ctx := c.Request.Context()
	q := dbgen.New(h.systemPool)
	user, _ := authctx.FromContext(c)
	rows, err := q.ListSopPlaybooks(ctx, dbgen.ListSopPlaybooksParams{ClerkUserID: user.ClerkUserID, PracticeID: practiceID})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load SOP")
		return
	}
	var detail sopDetail
	for _, r := range rows {
		if r.ID == id {
			detail.sopSummary = summarize(r.ID, r.Name, r.Description, r.Category, r.CurrentVersion, spec, r.ActiveDeployments, r.OutdatedDeployments, r.UpdatedAt)
		}
	}
	versions, err := q.ListSopPlaybookVersions(ctx, id)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load SOP versions")
		return
	}
	detail.AgentConfig, detail.WorkflowConfig, detail.Parameters = spec.Agent, spec.Workflow, spec.Parameters
	detail.Versions = make([]sopVersion, len(versions))
	for i, v := range versions {
		detail.Versions[i] = sopVersion{Version: v.Version, Changelog: v.Changelog, CreatedAt: v.CreatedAt.Time.Format(time.RFC3339)}
	}
	response.OK(c, status, message, detail)
}

type sopSource struct {
	AgentID    string  `json:"agent_id"`
	WorkflowID *string `json:"workflow_id"`
}

type createSopRequest struct {
	Name           string              `json:"name"`
	Description    *string             `json:"description"`
	Category       *string             `json:"category"`
	AgentConfig    *sop.AgentConfig    `json:"agent_config"`
	WorkflowConfig *sop.WorkflowConfig `json:"workflow_config"`
	Parameters     []sop.Parameter     `json:"parameters"`
	// Source promotes an existing agent (and optionally one of its
	// workflows) from the caller's current workspace instead of agent_config.
	Source *sopSource `json:"source"`
}

func (h *Handler) CreateSop(c *gin.Context) {
	user, p, ok := h.sopContext(c, true)
	if !ok {
		return
	}
	var req createSopRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 255 {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "name is required (max 255 characters)")
		return
	}
	ctx := c.Request.Context()

	spec := sop.Spec{Parameters: req.Parameters}
	switch {
	case req.Source != nil:
		agent, workflow, code, msg := h.specFromSource(ctx, user, *req.Source)
		if code != "" {
			status := http.StatusBadRequest
			if code == "INTERNAL_SERVER_ERROR" {
				status = http.StatusInternalServerError
			}
			response.Fail(c, status, code, msg)
			return
		}
		spec.Agent, spec.Workflow = agent, workflow
	case req.AgentConfig != nil:
		spec.Agent, spec.Workflow = *req.AgentConfig, req.WorkflowConfig
	default:
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Provide agent_config, or source to promote an existing agent")
		return
	}
	if spec.Parameters == nil {
		spec.Parameters = []sop.Parameter{}
	}

	known, err := h.knownTools(ctx)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load tool catalog")
		return
	}
	if err := sop.Validate(spec, known); err != nil {
		failSopError(c, err, "Could not validate SOP")
		return
	}
	agentJSON, workflowJSON, paramsJSON, err := encodeSpec(spec)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not encode SOP")
		return
	}

	var id pgtype.UUID
	var duplicate bool
	createdBy := h.practiceMembershipID(ctx, p.id, user)
	err = tenant.WithTx(ctx, h.appPool, p.id, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		id, err = q.InsertSopPlaybook(ctx, dbgen.InsertSopPlaybookParams{
			PracticeID: p.id, Name: req.Name, Description: req.Description, Category: req.Category,
			AgentConfig: agentJSON, WorkflowConfig: workflowJSON, Parameters: paramsJSON, CreatedBy: createdBy,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			duplicate = true
			return nil
		}
		if err != nil {
			return err
		}
		changelog := "Initial version"
		return q.InsertSopPlaybookVersion(ctx, dbgen.InsertSopPlaybookVersionParams{
			SopPlaybookID: id, Version: 1, AgentConfig: agentJSON, WorkflowConfig: workflowJSON,
			Parameters: paramsJSON, Changelog: &changelog, CreatedBy: createdBy,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not create SOP")
		return
	}
	if duplicate {
		response.Fail(c, http.StatusConflict, "DUPLICATE_SOP_NAME", "A SOP with this name already exists in your library")
		return
	}
	h.respondDetail(c, http.StatusCreated, "SOP created", p.id, id, spec)
}

// practiceMembershipID is the caller's users row in the practice, for
// created_by columns on practice-owned rows (the request's own users row may
// belong to a client workspace).
func (h *Handler) practiceMembershipID(ctx context.Context, practiceID pgtype.UUID, user authctx.User) pgtype.UUID {
	m, err := dbgen.New(h.systemPool).GetActiveUserInOrg(ctx, dbgen.GetActiveUserInOrgParams{OrgID: practiceID, ClerkUserID: user.ClerkUserID})
	if err != nil {
		return pgtype.UUID{}
	}
	return m.ID
}

// specFromSource copies an existing agent (and optional workflow) from the
// caller's current workspace — always inside the practice, since
// resolvePractice already required that.
func (h *Handler) specFromSource(ctx context.Context, user authctx.User, src sopSource) (sop.AgentConfig, *sop.WorkflowConfig, string, string) {
	agentID, err := uuid.Parse(src.AgentID)
	if err != nil {
		return sop.AgentConfig{}, nil, "INVALID_REQUEST_BODY", "Invalid source.agent_id"
	}
	var workflowID *uuid.UUID
	if src.WorkflowID != nil {
		w, err := uuid.Parse(*src.WorkflowID)
		if err != nil {
			return sop.AgentConfig{}, nil, "INVALID_REQUEST_BODY", "Invalid source.workflow_id"
		}
		workflowID = &w
	}

	var agent sop.AgentConfig
	var workflow *sop.WorkflowConfig
	var code, msg string
	err = tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		a, err := q.GetAgent(ctx, dbgen.GetAgentParams{OrgID: user.OrgID, ID: pgtype.UUID{Bytes: agentID, Valid: true}})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (a.IsActive == nil || !*a.IsActive)) {
			code, msg = "AGENT_NOT_FOUND", "source.agent_id is not an active agent in your current workspace"
			return nil
		}
		if err != nil {
			return err
		}
		var ps sop.PolicyScope
		if len(a.PolicyScope) > 0 {
			if err := json.Unmarshal(a.PolicyScope, &ps); err != nil {
				return err
			}
		}
		agent = sop.AgentConfig{
			Name: a.Name, Description: a.Description, AgentType: a.AgentType, SystemPrompt: a.SystemPrompt,
			MaxOutputTokens: a.MaxOutputTokens, Temperature: a.Temperature, PolicyScope: ps,
		}
		if a.Model != nil {
			agent.Model = *a.Model
		}
		if workflowID == nil {
			return nil
		}
		wf, err := q.GetWorkflow(ctx, dbgen.GetWorkflowParams{OrgID: user.OrgID, ID: pgtype.UUID{Bytes: *workflowID, Valid: true}})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && wf.AgentID != a.ID) {
			code, msg = "WORKFLOW_NOT_FOUND", "source.workflow_id is not a workflow of that agent"
			return nil
		}
		if err != nil {
			return err
		}
		workflow = &sop.WorkflowConfig{
			Name: wf.Name, Description: wf.Description, TriggerType: wf.TriggerType, CronExpression: wf.CronExpression,
			RequiresApproval:  wf.RequiresApproval != nil && *wf.RequiresApproval,
			TaskInputTemplate: wf.TaskInputTemplate, EstimatedManualMinutes: wf.EstimatedManualMinutes,
		}
		return nil
	})
	if err != nil {
		return agent, nil, "INTERNAL_SERVER_ERROR", "Could not read the source agent"
	}
	return agent, workflow, code, msg
}

type updateSopRequest struct {
	Name           *string             `json:"name"`
	Description    *string             `json:"description"`
	Category       *string             `json:"category"`
	AgentConfig    *sop.AgentConfig    `json:"agent_config"`
	WorkflowConfig *sop.WorkflowConfig `json:"workflow_config"`
	// RemoveWorkflow drops the workflow from future versions.
	RemoveWorkflow bool            `json:"remove_workflow"`
	Parameters     []sop.Parameter `json:"parameters"`
	Changelog      *string         `json:"changelog"`
}

// UpdateSop edits metadata in place; any change to what a deployment
// renders (agent/workflow config, parameters) creates a new version, which
// existing deployments then report as "update available" until synced.
func (h *Handler) UpdateSop(c *gin.Context) {
	user, p, ok := h.sopContext(c, true)
	if !ok {
		return
	}
	row, spec, ok := h.loadSop(c, p.id)
	if !ok {
		return
	}
	var req updateSopRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid request body")
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" || len(n) > 255 {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "name can't be empty (max 255 characters)")
			return
		}
		req.Name = &n
	}
	ctx := c.Request.Context()

	configChanged := req.AgentConfig != nil || req.WorkflowConfig != nil || req.RemoveWorkflow || req.Parameters != nil
	next := spec
	if req.AgentConfig != nil {
		next.Agent = *req.AgentConfig
	}
	if req.WorkflowConfig != nil {
		next.Workflow = req.WorkflowConfig
	}
	if req.RemoveWorkflow {
		next.Workflow = nil
	}
	if req.Parameters != nil {
		next.Parameters = req.Parameters
	}

	var agentJSON, workflowJSON, paramsJSON []byte
	if configChanged {
		known, err := h.knownTools(ctx)
		if err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load tool catalog")
			return
		}
		if err := sop.Validate(next, known); err != nil {
			failSopError(c, err, "Could not validate SOP")
			return
		}
		if agentJSON, workflowJSON, paramsJSON, err = encodeSpec(next); err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not encode SOP")
			return
		}
	}

	createdBy := h.practiceMembershipID(ctx, p.id, user)
	err := tenant.WithTx(ctx, h.appPool, p.id, func(ctx context.Context, q *dbgen.Queries) error {
		if req.Name != nil || req.Description != nil || req.Category != nil {
			if _, err := q.UpdateSopPlaybookMeta(ctx, dbgen.UpdateSopPlaybookMetaParams{
				Name: req.Name, Description: req.Description, Category: req.Category, ID: row.ID, PracticeID: p.id,
			}); err != nil {
				return err
			}
		}
		if !configChanged {
			return nil
		}
		version, err := q.BumpSopPlaybookVersion(ctx, dbgen.BumpSopPlaybookVersionParams{
			ID: row.ID, PracticeID: p.id, AgentConfig: agentJSON, WorkflowConfig: workflowJSON, Parameters: paramsJSON,
		})
		if err != nil {
			return err
		}
		return q.InsertSopPlaybookVersion(ctx, dbgen.InsertSopPlaybookVersionParams{
			SopPlaybookID: row.ID, Version: version, AgentConfig: agentJSON, WorkflowConfig: workflowJSON,
			Parameters: paramsJSON, Changelog: req.Changelog, CreatedBy: createdBy,
		})
	})
	if err != nil {
		if isUniqueViolation(err) {
			response.Fail(c, http.StatusConflict, "DUPLICATE_SOP_NAME", "A SOP with this name already exists in your library")
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not update SOP")
		return
	}
	h.respondDetail(c, http.StatusOK, "SOP updated", p.id, row.ID, next)
}

// DeleteSop removes the playbook from the library only. Deployed agents and
// workflows keep running in their client workspaces, no longer synced or
// labeled as SOP-managed. Runs on the system pool (with the practice_id
// guard) because detaching deployments crosses into client workspaces.
func (h *Handler) DeleteSop(c *gin.Context) {
	_, p, ok := h.sopContext(c, true)
	if !ok {
		return
	}
	row, _, ok := h.loadSop(c, p.id)
	if !ok {
		return
	}
	err := pgx.BeginFunc(c.Request.Context(), h.systemPool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.DeactivateSopPlaybook(c.Request.Context(), dbgen.DeactivateSopPlaybookParams{ID: row.ID, PracticeID: p.id}); err != nil {
			return err
		}
		return q.DetachSopDeployments(c.Request.Context(), row.ID)
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not delete SOP")
		return
	}
	response.OK(c, http.StatusOK, "SOP removed from library; deployed agents keep running", gin.H{"id": row.ID.String()})
}

// ---- deployments ----

// targetMembership checks target is an active client workspace of the
// practice and the caller is owner/admin there, returning the caller's
// users row in it (created_by for the rows written there).
func (h *Handler) targetMembership(c *gin.Context, p practiceContext, user authctx.User, targetID pgtype.UUID) (pgtype.UUID, bool) {
	ctx := c.Request.Context()
	q := dbgen.New(h.systemPool)
	org, err := q.GetActiveOrganizationByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "WORKSPACE_NOT_FOUND", "Client workspace not found")
		} else {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load client workspace")
		}
		return pgtype.UUID{}, false
	}
	if org.OrganizationType != "client_workspace" || org.ParentPracticeID != p.id {
		response.Fail(c, http.StatusBadRequest, "TARGET_NOT_CLIENT_WORKSPACE", "SOPs deploy only to client workspaces of this practice")
		return pgtype.UUID{}, false
	}
	m, err := q.GetActiveUserInOrg(ctx, dbgen.GetActiveUserInOrgParams{OrgID: targetID, ClerkUserID: user.ClerkUserID})
	if err != nil || (m.Role != "owner" && m.Role != "admin") {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "You need admin access to that client workspace")
		return pgtype.UUID{}, false
	}
	return m.ID, true
}

func (h *Handler) connectedServices(ctx context.Context, orgID pgtype.UUID) (map[string]bool, error) {
	services, err := dbgen.New(h.systemPool).ListConnectedServices(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, s := range services {
		out[s] = true
	}
	return out, nil
}

type deployRequest struct {
	TargetOrgID        string        `json:"target_org_id"`
	ParameterOverrides sop.Overrides `json:"parameter_overrides"`
}

var (
	errAlreadyDeployed = errors.New("sop: already deployed")
	errAgentLimit      = errors.New("sop: agent plan limit reached")
	errWorkflowLimit   = errors.New("sop: workflow plan limit reached")
	errAgentNameTaken  = errors.New("sop: agent name taken")
	errAgentRemoved    = errors.New("sop: deployed agent removed")
)

func (h *Handler) DeploySop(c *gin.Context) {
	user, p, ok := h.sopContext(c, true)
	if !ok {
		return
	}
	row, spec, ok := h.loadSop(c, p.id)
	if !ok {
		return
	}
	var req deployRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid request body")
		return
	}
	target, err := uuid.Parse(req.TargetOrgID)
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid target_org_id")
		return
	}
	targetID := pgtype.UUID{Bytes: target, Valid: true}
	memberID, ok := h.targetMembership(c, p, user, targetID)
	if !ok {
		return
	}
	if err := sop.ValidateOverrides(spec, req.ParameterOverrides); err != nil {
		failSopError(c, err, "Could not validate overrides")
		return
	}
	overridesJSON, err := json.Marshal(req.ParameterOverrides)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not encode overrides")
		return
	}
	agent, workflow := sop.Render(spec, req.ParameterOverrides)

	ctx := c.Request.Context()
	var deploymentID, agentID, workflowID pgtype.UUID
	err = tenant.WithTx(ctx, h.appPool, targetID, func(ctx context.Context, q *dbgen.Queries) error {
		exists, err := q.HasActiveSopDeployment(ctx, dbgen.HasActiveSopDeploymentParams{SopPlaybookID: row.ID, TargetOrgID: targetID})
		if err != nil {
			return err
		}
		if exists {
			return errAlreadyDeployed
		}
		maxAgents, err := q.GetOrganizationMaxAgents(ctx, targetID)
		if err != nil {
			return err
		}
		count, err := q.CountActiveAgents(ctx, targetID)
		if err != nil {
			return err
		}
		if maxAgents != nil && count >= int64(*maxAgents) {
			return errAgentLimit
		}
		if workflow != nil {
			if reached, err := corebilling.WorkflowLimitReached(ctx, q, targetID); err != nil || reached {
				return cmp.Or(err, errWorkflowLimit)
			}
		}
		if agentID, err = insertAgent(ctx, q, targetID, memberID, agent); err != nil {
			return err
		}
		if workflow != nil {
			if workflowID, err = insertWorkflow(ctx, q, targetID, memberID, agentID, *workflow); err != nil {
				return err
			}
		}
		d, err := q.InsertSopDeployment(ctx, dbgen.InsertSopDeploymentParams{
			SopPlaybookID: row.ID, SopName: row.Name, DeployedVersion: row.CurrentVersion, TargetOrgID: targetID,
			AgentID: agentID, WorkflowID: workflowID, ParameterOverrides: overridesJSON, DeployedBy: memberID,
		})
		deploymentID = d.ID
		return err
	})
	if err != nil {
		failDeploymentWrite(c, err, "Could not deploy SOP")
		return
	}

	connected, err := h.connectedServices(ctx, targetID)
	if err != nil {
		connected = map[string]bool{}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	view := deploymentView{
		ID: deploymentID.String(), WorkspaceID: targetID.String(), DeployedVersion: row.CurrentVersion,
		CurrentVersion: row.CurrentVersion, Status: "up_to_date", ParameterOverrides: req.ParameterOverrides,
		MissingIntegrations: sop.MissingServices(agent, connected), AgentID: agentID.String(),
		WorkflowID: uuidPtr(workflowID), DeployedAt: now, SyncedAt: now,
	}
	if org, err := dbgen.New(h.systemPool).GetActiveOrganizationByID(ctx, targetID); err == nil {
		view.WorkspaceName = org.Name
	}
	response.OK(c, http.StatusCreated, "SOP deployed", view)
}

func failDeploymentWrite(c *gin.Context, err error, fallback string) {
	switch {
	case errors.Is(err, errAlreadyDeployed) || isUniqueViolation(err) && strings.Contains(err.Error(), "sop_deployments"):
		response.Fail(c, http.StatusConflict, "ALREADY_DEPLOYED", "This SOP is already deployed to that workspace — sync it instead")
	case errors.Is(err, errAgentLimit):
		response.Fail(c, http.StatusBadRequest, "PLAN_LIMIT_REACHED", "That workspace has reached its plan's agent limit")
	case errors.Is(err, errWorkflowLimit):
		response.Fail(c, http.StatusBadRequest, "PLAN_LIMIT_REACHED", "That workspace has reached its plan's workflow limit")
	case errors.Is(err, errAgentNameTaken) || isUniqueViolation(err):
		response.Fail(c, http.StatusConflict, "DUPLICATE_AGENT_NAME", "That workspace already has an agent with this SOP's agent name")
	case errors.Is(err, errAgentRemoved):
		response.Fail(c, http.StatusConflict, "DEPLOYMENT_BROKEN", "The client workspace removed this SOP's agent — remove the deployment and deploy again")
	default:
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", fallback)
	}
}

func insertAgent(ctx context.Context, q *dbgen.Queries, orgID, createdBy pgtype.UUID, a sop.AgentConfig) (pgtype.UUID, error) {
	policyJSON, err := json.Marshal(a.PolicyScope)
	if err != nil {
		return pgtype.UUID{}, err
	}
	serversJSON, err := json.Marshal(sop.RequiredServices(a))
	if err != nil {
		return pgtype.UUID{}, err
	}
	r, err := q.InsertAgent(ctx, dbgen.InsertAgentParams{
		OrgID: orgID, Name: a.Name, Slug: slugify(a.Name), Description: a.Description, AgentType: a.AgentType,
		Model: &a.Model, SystemPrompt: a.SystemPrompt, MaxOutputTokens: a.MaxOutputTokens, Temperature: a.Temperature,
		PolicyScope: policyJSON, AllowedMcpServers: serversJSON, CreatedBy: createdBy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, errAgentNameTaken
	}
	return r.ID, err
}

func insertWorkflow(ctx context.Context, q *dbgen.Queries, orgID, createdBy, agentID pgtype.UUID, w sop.WorkflowConfig) (pgtype.UUID, error) {
	cronExpr, nextRunAt := schedule(w)
	requiresApproval := w.RequiresApproval
	r, err := q.InsertWorkflow(ctx, dbgen.InsertWorkflowParams{
		OrgID: orgID, AgentID: agentID, Name: w.Name, Description: w.Description, TriggerType: w.TriggerType,
		GraphDefinition: standardGraphDefinition, CronExpression: cronExpr, NextRunAt: nextRunAt,
		RequiresApproval: &requiresApproval, TaskInputTemplate: w.TaskInputTemplate,
		EstimatedManualMinutes: w.EstimatedManualMinutes, CreatedBy: createdBy,
	})
	return r.ID, err
}

// schedule mirrors internal/api/workflows' computeSchedule for an
// already-validated config.
func schedule(w sop.WorkflowConfig) (*string, pgtype.Timestamptz) {
	if w.TriggerType != "scheduled" || w.CronExpression == nil {
		return nil, pgtype.Timestamptz{}
	}
	s, err := cron.ParseStandard(*w.CronExpression)
	if err != nil {
		return nil, pgtype.Timestamptz{}
	}
	return w.CronExpression, pgtype.Timestamptz{Time: s.Next(time.Now()), Valid: true}
}

func slugify(name string) string {
	var b strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func (h *Handler) ListSopDeployments(c *gin.Context) {
	user, p, ok := h.sopContext(c, false)
	if !ok {
		return
	}
	row, spec, ok := h.loadSop(c, p.id)
	if !ok {
		return
	}
	rows, err := dbgen.New(h.systemPool).ListSopDeploymentsForPlaybook(c.Request.Context(), dbgen.ListSopDeploymentsForPlaybookParams{
		SopPlaybookID: row.ID, ClerkUserID: user.ClerkUserID,
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list deployments")
		return
	}
	out := make([]deploymentView, 0, len(rows))
	for _, r := range rows {
		if r.WorkspaceActive == nil || !*r.WorkspaceActive {
			continue
		}
		var o sop.Overrides
		_ = json.Unmarshal(r.ParameterOverrides, &o)
		connected := map[string]bool{}
		for _, s := range r.ConnectedServices {
			connected[s] = true
		}
		agent, _ := sop.Render(spec, o)
		out = append(out, deploymentView{
			ID: r.ID.String(), WorkspaceID: r.TargetOrgID.String(), WorkspaceName: r.WorkspaceName,
			DeployedVersion: r.DeployedVersion, CurrentVersion: row.CurrentVersion,
			Status:             deploymentStatus(r.AgentActive, r.DeployedVersion, row.CurrentVersion),
			ParameterOverrides: o, MissingIntegrations: sop.MissingServices(agent, connected),
			AgentID: r.AgentID.String(), WorkflowID: uuidPtr(r.WorkflowID),
			DeployedAt: r.DeployedAt.Time.Format(time.RFC3339), SyncedAt: r.SyncedAt.Time.Format(time.RFC3339),
		})
	}
	response.OK(c, http.StatusOK, "Deployments listed", gin.H{"deployments": out, "current_version": row.CurrentVersion})
}

// loadDeployment resolves :deployment_id under the SOP and checks the caller
// may write into its workspace.
func (h *Handler) loadDeployment(c *gin.Context) (authctx.User, practiceContext, dbgen.GetSopPlaybookRow, sop.Spec, dbgen.GetSopDeploymentRow, pgtype.UUID, bool) {
	var d dbgen.GetSopDeploymentRow
	user, p, ok := h.sopContext(c, true)
	if !ok {
		return user, p, dbgen.GetSopPlaybookRow{}, sop.Spec{}, d, pgtype.UUID{}, false
	}
	row, spec, ok := h.loadSop(c, p.id)
	if !ok {
		return user, p, row, spec, d, pgtype.UUID{}, false
	}
	id, ok := parseUUIDParam(c, "deployment_id", "deployment id")
	if !ok {
		return user, p, row, spec, d, pgtype.UUID{}, false
	}
	d, err := dbgen.New(h.systemPool).GetSopDeployment(c.Request.Context(), dbgen.GetSopDeploymentParams{ID: id, SopPlaybookID: row.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "DEPLOYMENT_NOT_FOUND", "Deployment not found")
		} else {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load deployment")
		}
		return user, p, row, spec, d, pgtype.UUID{}, false
	}
	memberID, ok := h.targetMembership(c, p, user, d.TargetOrgID)
	if !ok {
		return user, p, row, spec, d, pgtype.UUID{}, false
	}
	return user, p, row, spec, d, memberID, true
}

// applyDeployment re-renders spec with overrides onto the deployment's
// existing agent/workflow rows and records the version it now matches. A
// workflow the new version added is created; one it dropped is paused.
func (h *Handler) applyDeployment(ctx context.Context, d dbgen.GetSopDeploymentRow, memberID pgtype.UUID, sopName string, version int32, spec sop.Spec, o sop.Overrides) (pgtype.UUID, error) {
	overridesJSON, err := json.Marshal(o)
	if err != nil {
		return pgtype.UUID{}, err
	}
	agent, workflow := sop.Render(spec, o)
	workflowID := d.WorkflowID
	err = tenant.WithTx(ctx, h.appPool, d.TargetOrgID, func(ctx context.Context, q *dbgen.Queries) error {
		policyJSON, err := json.Marshal(agent.PolicyScope)
		if err != nil {
			return err
		}
		serversJSON, err := json.Marshal(sop.RequiredServices(agent))
		if err != nil {
			return err
		}
		_, err = q.SyncSopAgent(ctx, dbgen.SyncSopAgentParams{
			OrgID: d.TargetOrgID, ID: d.AgentID, Name: agent.Name, Description: agent.Description,
			AgentType: agent.AgentType, Model: &agent.Model, SystemPrompt: agent.SystemPrompt,
			MaxOutputTokens: agent.MaxOutputTokens, Temperature: agent.Temperature,
			PolicyScope: policyJSON, AllowedMcpServers: serversJSON,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errAgentRemoved
		}
		if err != nil {
			return err
		}

		switch {
		case workflow != nil && workflowID.Valid:
			cronExpr, next := schedule(*workflow)
			requiresApproval := workflow.RequiresApproval
			if _, err := q.SyncSopWorkflow(ctx, dbgen.SyncSopWorkflowParams{
				OrgID: d.TargetOrgID, ID: workflowID, Name: workflow.Name, Description: workflow.Description,
				TriggerType: workflow.TriggerType, CronExpression: cronExpr, NextRunAt: next,
				RequiresApproval: &requiresApproval, TaskInputTemplate: workflow.TaskInputTemplate,
				EstimatedManualMinutes: workflow.EstimatedManualMinutes,
			}); err != nil {
				return err
			}
		case workflow != nil:
			if reached, err := corebilling.WorkflowLimitReached(ctx, q, d.TargetOrgID); err != nil || reached {
				return cmp.Or(err, errWorkflowLimit)
			}
			if workflowID, err = insertWorkflow(ctx, q, d.TargetOrgID, memberID, d.AgentID, *workflow); err != nil {
				return err
			}
		case workflowID.Valid:
			if err := q.PauseSopWorkflow(ctx, dbgen.PauseSopWorkflowParams{OrgID: d.TargetOrgID, ID: workflowID}); err != nil {
				return err
			}
			workflowID = pgtype.UUID{}
		}
		return q.MarkSopDeploymentSynced(ctx, dbgen.MarkSopDeploymentSyncedParams{
			ID: d.ID, TargetOrgID: d.TargetOrgID, DeployedVersion: version, ParameterOverrides: overridesJSON,
			SopName: sopName, WorkflowID: workflowID,
		})
	})
	return workflowID, err
}

// SyncSopDeployment brings one client up to the SOP's current version,
// re-applying that client's stored overrides.
func (h *Handler) SyncSopDeployment(c *gin.Context) {
	_, _, row, spec, d, memberID, ok := h.loadDeployment(c)
	if !ok {
		return
	}
	var o sop.Overrides
	_ = json.Unmarshal(d.ParameterOverrides, &o)
	workflowID, err := h.applyDeployment(c.Request.Context(), d, memberID, row.Name, row.CurrentVersion, spec, o)
	if err != nil {
		failDeploymentWrite(c, err, "Could not sync deployment")
		return
	}
	response.OK(c, http.StatusOK, "Deployment synced", gin.H{
		"id": d.ID.String(), "deployed_version": row.CurrentVersion, "status": "up_to_date", "workflow_id": uuidPtr(workflowID),
	})
}

type updateDeploymentRequest struct {
	ParameterOverrides sop.Overrides `json:"parameter_overrides"`
}

// UpdateSopDeployment replaces one client's overrides and re-applies them at
// the version that client is already on — changing overrides never
// silently upgrades a deployment; that's what sync is for.
func (h *Handler) UpdateSopDeployment(c *gin.Context) {
	_, _, row, _, d, memberID, ok := h.loadDeployment(c)
	if !ok {
		return
	}
	var req updateDeploymentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid request body")
		return
	}
	ctx := c.Request.Context()
	v, err := dbgen.New(h.systemPool).GetSopPlaybookVersion(ctx, dbgen.GetSopPlaybookVersionParams{SopPlaybookID: row.ID, Version: d.DeployedVersion})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load the deployed version")
		return
	}
	spec, err := decodeSpec(v.AgentConfig, v.WorkflowConfig, v.Parameters)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read the deployed version")
		return
	}
	if err := sop.ValidateOverrides(spec, req.ParameterOverrides); err != nil {
		failSopError(c, err, "Could not validate overrides")
		return
	}
	if _, err := h.applyDeployment(ctx, d, memberID, row.Name, d.DeployedVersion, spec, req.ParameterOverrides); err != nil {
		failDeploymentWrite(c, err, "Could not update overrides")
		return
	}
	response.OK(c, http.StatusOK, "Overrides updated", gin.H{
		"id": d.ID.String(), "deployed_version": d.DeployedVersion, "parameter_overrides": req.ParameterOverrides,
	})
}

// UndeploySop removes the SOP from one client: its agent is deactivated and
// workflow paused (history kept, same soft-delete convention as the agents
// and workflows APIs), and the deployment stops being tracked.
func (h *Handler) UndeploySop(c *gin.Context) {
	_, _, _, _, d, _, ok := h.loadDeployment(c)
	if !ok {
		return
	}
	err := tenant.WithTx(c.Request.Context(), h.appPool, d.TargetOrgID, func(ctx context.Context, q *dbgen.Queries) error {
		if d.WorkflowID.Valid {
			if err := q.PauseSopWorkflow(ctx, dbgen.PauseSopWorkflowParams{OrgID: d.TargetOrgID, ID: d.WorkflowID}); err != nil {
				return err
			}
		}
		if err := q.DeactivateSopAgent(ctx, dbgen.DeactivateSopAgentParams{OrgID: d.TargetOrgID, ID: d.AgentID}); err != nil {
			return err
		}
		_, err := q.DeactivateSopDeployment(ctx, dbgen.DeactivateSopDeploymentParams{ID: d.ID, TargetOrgID: d.TargetOrgID})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not remove deployment")
		return
	}
	response.OK(c, http.StatusOK, "SOP removed from workspace", gin.H{"id": d.ID.String()})
}
