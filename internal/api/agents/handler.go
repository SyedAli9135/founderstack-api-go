package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

const (
	defaultAgentType       = "specialist"
	defaultModel           = "claude-sonnet-5"
	defaultMaxOutputTokens = int32(4096)
	defaultTemperature     = 0.3
	minSystemPromptLen     = 50
)

// Handler implements the 6 agent-configuration endpoints (5 CRUD + the
// available-tools listing the create/edit forms populate their multi-select
// from).
type Handler struct {
	appPool  *pgxpool.Pool
	registry *coremcp.Registry
}

// NewHandler builds a Handler. appPool must be the app_user (RLS-enforced)
// pool — every DB operation here goes through tenant.WithTx, never a bare
// query. MCP tool registry, used read-only here
// (ListTools) to validate policy_scope.allowed_tools and to populate
// GET .../agents/tools — never to execute a tool
func NewHandler(appPool *pgxpool.Pool, registry *coremcp.Registry) *Handler {
	return &Handler{appPool: appPool, registry: registry}
}

// Register mounts all 6 routes on rg. rg's group must already have
// middleware.RequireAuth applied. "/agents/tools" is registered before
// "/agents/:id" only for readability — Gin's router already prefers an
// exact static match over a param segment regardless of registration
// order, the same way internal/api/integrations layers static suffixes
// (".../status", ".../callback") after a shared ":service" param.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/agents", h.List)
	rg.POST("/agents", h.Create)
	rg.GET("/agents/tools", h.ListAvailableTools)
	rg.GET("/agents/:id", h.Get)
	rg.PATCH("/agents/:id", h.Update)
	rg.DELETE("/agents/:id", h.Delete)
}

type policyScope struct {
	MaxToolCalls     *int32   `json:"max_tool_calls,omitempty"`
	MaxCostPerRunUSD *float64 `json:"max_cost_per_run_usd,omitempty"`
	AllowedTools     []string `json:"allowed_tools"`
}

type agentView struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Slug                string          `json:"slug"`
	Description         *string         `json:"description,omitempty"`
	AgentType           string          `json:"agent_type"`
	Model               string          `json:"model"`
	SystemPrompt        string          `json:"system_prompt"`
	ContextWindowTokens int32           `json:"context_window_tokens"`
	MaxOutputTokens     int32           `json:"max_output_tokens"`
	Temperature         float64         `json:"temperature"`
	PolicyScope         json.RawMessage `json:"policy_scope"`
	AllowedMCPServers   json.RawMessage `json:"allowed_mcp_servers"`
	IsActive            bool            `json:"is_active"`
	Version             int32           `json:"version"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	// WorkflowCount (workflow 8) is only populated by List/Get — a
	// freshly created or updated agent can't have any workflows pointing
	// at it yet within the same request, so Insert/Update always report 0
	// rather than needing their own extra query.
	WorkflowCount int64 `json:"workflow_count"`
}

// row is the shape shared by every agents-table sqlc row type
// (ListAgentsRow, GetAgentRow, InsertAgentRow, UpdateAgentRow) — sqlc
// generates one struct per query rather than reusing dbgen.Agent, so this
// is what lets toView do the field-by-field mapping once instead of once
// per query.
type row struct {
	ID                  pgtype.UUID
	Name                string
	Slug                string
	Description         *string
	AgentType           string
	Model               *string
	SystemPrompt        string
	ContextWindowTokens *int32
	MaxOutputTokens     *int32
	Temperature         *float64
	PolicyScope         []byte
	AllowedMcpServers   []byte
	IsActive            *bool
	Version             *int32
	CreatedAt           pgtype.Timestamptz
	UpdatedAt           pgtype.Timestamptz
	WorkflowCount       int64
}

func toView(r row) agentView {
	return agentView{
		ID:                  r.ID.String(),
		Name:                r.Name,
		Slug:                r.Slug,
		Description:         r.Description,
		AgentType:           r.AgentType,
		Model:               derefOr(r.Model, defaultModel),
		SystemPrompt:        r.SystemPrompt,
		ContextWindowTokens: derefInt32(r.ContextWindowTokens),
		MaxOutputTokens:     derefInt32(r.MaxOutputTokens),
		Temperature:         derefFloat(r.Temperature, defaultTemperature),
		PolicyScope:         nonNullJSON(r.PolicyScope),
		AllowedMCPServers:   nonNullJSON(r.AllowedMcpServers),
		IsActive:            derefBool(r.IsActive),
		Version:             derefInt32(r.Version),
		CreatedAt:           r.CreatedAt.Time,
		UpdatedAt:           r.UpdatedAt.Time,
		WorkflowCount:       r.WorkflowCount,
	}
}

func viewFromList(r dbgen.ListAgentsRow) agentView {
	return toView(row{r.ID, r.Name, r.Slug, r.Description, r.AgentType, r.Model, r.SystemPrompt,
		r.ContextWindowTokens, r.MaxOutputTokens, r.Temperature, r.PolicyScope, r.AllowedMcpServers,
		r.IsActive, r.Version, r.CreatedAt, r.UpdatedAt, r.WorkflowCount})
}

func viewFromGet(r dbgen.GetAgentRow) agentView {
	return toView(row{r.ID, r.Name, r.Slug, r.Description, r.AgentType, r.Model, r.SystemPrompt,
		r.ContextWindowTokens, r.MaxOutputTokens, r.Temperature, r.PolicyScope, r.AllowedMcpServers,
		r.IsActive, r.Version, r.CreatedAt, r.UpdatedAt, r.WorkflowCount})
}

func viewFromInsert(r dbgen.InsertAgentRow) agentView {
	return toView(row{r.ID, r.Name, r.Slug, r.Description, r.AgentType, r.Model, r.SystemPrompt,
		r.ContextWindowTokens, r.MaxOutputTokens, r.Temperature, r.PolicyScope, r.AllowedMcpServers,
		r.IsActive, r.Version, r.CreatedAt, r.UpdatedAt, 0})
}

func viewFromUpdate(r dbgen.UpdateAgentRow) agentView {
	return toView(row{r.ID, r.Name, r.Slug, r.Description, r.AgentType, r.Model, r.SystemPrompt,
		r.ContextWindowTokens, r.MaxOutputTokens, r.Temperature, r.PolicyScope, r.AllowedMcpServers,
		r.IsActive, r.Version, r.CreatedAt, r.UpdatedAt, 0})
}

// List returns every active agent for the org — GET /api/v1/agents.
func (h *Handler) List(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	views := []agentView{}
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		rows, err := q.ListAgents(ctx, user.OrgID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			views = append(views, viewFromList(r))
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list agents")
		return
	}

	response.OK(c, http.StatusOK, "", views)
}

// Get returns one agent's detail — GET /api/v1/agents/{id}.
func (h *Handler) Get(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseAgentID(c)
	if !ok {
		return
	}

	var view agentView
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		r, err := q.GetAgent(ctx, dbgen.GetAgentParams{OrgID: user.OrgID, ID: id})
		if err != nil {
			return err
		}
		view = viewFromGet(r)
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "AGENT_NOT_FOUND", "Agent not found")
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch agent")
		return
	}

	response.OK(c, http.StatusOK, "", view)
}

type createAgentRequest struct {
	Name            string      `json:"name" binding:"required"`
	Description     *string     `json:"description"`
	AgentType       string      `json:"agent_type"`
	Model           string      `json:"model"`
	SystemPrompt    string      `json:"system_prompt" binding:"required"`
	MaxOutputTokens *int32      `json:"max_output_tokens"`
	Temperature     *float64    `json:"temperature"`
	PolicyScope     policyScope `json:"policy_scope"`
}

// Create validates and inserts a new agent — POST /api/v1/agents.
func (h *Handler) Create(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var req createAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "name and system_prompt are required")
		return
	}

	if code, msg := h.validatePolicyScope(c.Request.Context(), req.PolicyScope); code != "" {
		response.Fail(c, http.StatusBadRequest, code, msg)
		return
	}
	if len(strings.TrimSpace(req.SystemPrompt)) < minSystemPromptLen {
		response.Fail(c, http.StatusBadRequest, "SYSTEM_PROMPT_TOO_SHORT",
			fmt.Sprintf("system_prompt must be at least %d characters", minSystemPromptLen))
		return
	}

	agentType := req.AgentType
	if agentType == "" {
		agentType = defaultAgentType
	}
	model := req.Model
	if model == "" {
		model = defaultModel
	}
	maxOutputTokens := defaultMaxOutputTokens
	if req.MaxOutputTokens != nil {
		maxOutputTokens = *req.MaxOutputTokens
	}
	temperature := float64(defaultTemperature)
	if req.Temperature != nil {
		temperature = *req.Temperature
	}

	policyScopeJSON, err := json.Marshal(req.PolicyScope)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not encode policy_scope")
		return
	}
	allowedServersJSON, err := json.Marshal(serversFromTools(req.PolicyScope.AllowedTools))
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not encode allowed_mcp_servers")
		return
	}

	var view agentView
	var limitReached, duplicate bool
	err = tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		maxAgents, err := q.GetOrganizationMaxAgents(ctx, user.OrgID)
		if err != nil {
			return err
		}
		activeCount, err := q.CountActiveAgents(ctx, user.OrgID)
		if err != nil {
			return err
		}
		if maxAgents != nil && activeCount >= int64(*maxAgents) {
			limitReached = true
			return nil
		}

		r, err := q.InsertAgent(ctx, dbgen.InsertAgentParams{
			OrgID: user.OrgID, Name: req.Name, Slug: slugify(req.Name), Description: req.Description,
			AgentType: agentType, Model: &model, SystemPrompt: req.SystemPrompt,
			MaxOutputTokens: &maxOutputTokens, Temperature: &temperature,
			PolicyScope: policyScopeJSON, AllowedMcpServers: allowedServersJSON, CreatedBy: user.ID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				duplicate = true
				return nil
			}
			return err
		}
		view = viewFromInsert(r)
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not create agent")
		return
	}
	if limitReached {
		response.Fail(c, http.StatusBadRequest, "PLAN_LIMIT_REACHED", "Your plan's agent limit has been reached")
		return
	}
	if duplicate {
		response.Fail(c, http.StatusBadRequest, "DUPLICATE_AGENT_NAME",
			fmt.Sprintf("An agent named %q already exists", req.Name))
		return
	}

	response.OK(c, http.StatusCreated, "Agent created", view)
}

type updateAgentRequest struct {
	Name            *string      `json:"name"`
	Description     *string      `json:"description"`
	AgentType       *string      `json:"agent_type"`
	Model           *string      `json:"model"`
	SystemPrompt    *string      `json:"system_prompt"`
	MaxOutputTokens *int32       `json:"max_output_tokens"`
	Temperature     *float64     `json:"temperature"`
	PolicyScope     *policyScope `json:"policy_scope"`
}

// Update applies a partial update — PATCH /api/v1/agents/{id}. Every field
// is optional; only fields present in the request body are changed.
func (h *Handler) Update(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseAgentID(c)
	if !ok {
		return
	}

	var req updateAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Malformed request body")
		return
	}
	if req.SystemPrompt != nil && len(strings.TrimSpace(*req.SystemPrompt)) < minSystemPromptLen {
		response.Fail(c, http.StatusBadRequest, "SYSTEM_PROMPT_TOO_SHORT",
			fmt.Sprintf("system_prompt must be at least %d characters", minSystemPromptLen))
		return
	}

	params := dbgen.UpdateAgentParams{
		OrgID: user.OrgID, ID: id, Name: req.Name, Description: req.Description,
		AgentType: req.AgentType, Model: req.Model, SystemPrompt: req.SystemPrompt,
		MaxOutputTokens: req.MaxOutputTokens, Temperature: req.Temperature,
	}
	if req.PolicyScope != nil {
		if code, msg := h.validatePolicyScope(c.Request.Context(), *req.PolicyScope); code != "" {
			response.Fail(c, http.StatusBadRequest, code, msg)
			return
		}
		policyScopeJSON, err := json.Marshal(*req.PolicyScope)
		if err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not encode policy_scope")
			return
		}
		allowedServersJSON, err := json.Marshal(serversFromTools(req.PolicyScope.AllowedTools))
		if err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not encode allowed_mcp_servers")
			return
		}
		params.PolicyScope = policyScopeJSON
		params.AllowedMcpServers = allowedServersJSON
	}

	var view agentView
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		r, err := q.UpdateAgent(ctx, params)
		if err != nil {
			return err
		}
		view = viewFromUpdate(r)
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "AGENT_NOT_FOUND", "Agent not found")
			return
		}
		if isUniqueViolation(err) {
			name := ""
			if req.Name != nil {
				name = *req.Name
			}
			response.Fail(c, http.StatusBadRequest, "DUPLICATE_AGENT_NAME",
				fmt.Sprintf("An agent named %q already exists", name))
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not update agent")
		return
	}

	response.OK(c, http.StatusOK, "Agent updated", view)
}

// Delete deactivates an agent (soft delete, is_active=false) — run history
// is preserved, matching every other
// delete-ish endpoint's convention in this codebase (integrations,
// documents) — DELETE /api/v1/agents/{id}.
func (h *Handler) Delete(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseAgentID(c)
	if !ok {
		return
	}

	var rowsAffected int64
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		n, err := q.DeactivateAgent(ctx, dbgen.DeactivateAgentParams{OrgID: user.OrgID, ID: id})
		rowsAffected = n
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not delete agent")
		return
	}
	if rowsAffected == 0 {
		response.Fail(c, http.StatusNotFound, "AGENT_NOT_FOUND", "Agent not found")
		return
	}

	c.Status(http.StatusNoContent)
}

type toolOption struct {
	Service     string `json:"service"`
	Name        string `json:"name"`
	ToolID      string `json:"tool_id"`
	Description string `json:"description"`
}

// ListAvailableTools returns every tool from a service the org has
// actually connected — GET /api/v1/agents/tools. Populates the create/edit
// form's allowed-tools multi-select; offering a tool from an unconnected
// service would just be a confusing choice nothing could ever execute.
func (h *Handler) ListAvailableTools(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	connected := map[string]bool{}
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		rows, err := q.ListConnectionsByOrg(ctx, user.OrgID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.IsActive != nil && *r.IsActive {
				connected[r.ServiceName] = true
			}
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load connected integrations")
		return
	}

	toolsByService, err := h.registry.ListTools(c.Request.Context())
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load tool catalog")
		return
	}

	options := []toolOption{}
	for service, tools := range toolsByService {
		if !connected[service] {
			continue
		}
		for _, t := range tools {
			options = append(options, toolOption{
				Service: service, Name: t.Name, ToolID: service + "." + t.Name, Description: t.Description,
			})
		}
	}
	sort.Slice(options, func(i, j int) bool {
		if options[i].Service != options[j].Service {
			return options[i].Service < options[j].Service
		}
		return options[i].Name < options[j].Name
	})

	response.OK(c, http.StatusOK, "", options)
}

// validatePolicyScope enforces policy rules: at least one
// allowed tool, every tool a real "service.tool_name" the MCP registry
// actually knows about (workflow 5), and a positive cost cap when one is
// set. Returns ("", "") when valid, or an error code + message otherwise.
func (h *Handler) validatePolicyScope(ctx context.Context, ps policyScope) (string, string) {
	if len(ps.AllowedTools) == 0 {
		return "NO_ALLOWED_TOOLS", "At least one allowed tool is required"
	}
	if ps.MaxCostPerRunUSD != nil && *ps.MaxCostPerRunUSD <= 0 {
		return "INVALID_COST_CAP", "max_cost_per_run_usd must be a positive number"
	}
	if ps.MaxToolCalls != nil && *ps.MaxToolCalls <= 0 {
		return "INVALID_TOOL_CALL_CAP", "max_tool_calls must be a positive number"
	}

	toolsByService, err := h.registry.ListTools(ctx)
	if err != nil {
		return "INTERNAL_SERVER_ERROR", "Could not verify allowed_tools against the tool registry"
	}
	known := map[string]bool{}
	for service, tools := range toolsByService {
		for _, t := range tools {
			known[service+"."+t.Name] = true
		}
	}
	for _, toolID := range ps.AllowedTools {
		if !known[toolID] {
			return "UNKNOWN_TOOL", fmt.Sprintf("%q is not a recognized tool", toolID)
		}
	}
	return "", ""
}

// serversFromTools derives the unique set of services referenced by
// "service.tool_name" allowed_tools entries — allowed_mcp_servers is kept
// in sync automatically rather than accepted as a separate, independently
// editable field, so the two columns can never drift out of agreement.
func serversFromTools(toolIDs []string) []string {
	seen := map[string]bool{}
	var servers []string
	for _, id := range toolIDs {
		service, _, ok := strings.Cut(id, ".")
		if !ok {
			continue
		}
		if !seen[service] {
			seen[service] = true
			servers = append(servers, service)
		}
	}
	sort.Strings(servers)
	if servers == nil {
		servers = []string{}
	}
	return servers
}

// slugify lowercases and hyphenates name for the slug column — a 5-line
// hand-written helper rather than gosimple/slug, per this codebase's
// "don't add a dependency this doesn't justify" policy (see CLAUDE.md's
// Dependency policy section); slug has no uniqueness constraint (name
// already does, via 000006's partial index), so collisions aren't a
// correctness concern here.
func slugify(name string) string {
	var b strings.Builder
	lastHyphen := true // suppresses a leading hyphen
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

func parseAgentID(c *gin.Context) (pgtype.UUID, bool) {
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid agent id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
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

func derefFloat(f *float64, fallback float64) float64 {
	if f == nil {
		return fallback
	}
	return *f
}

func derefBool(b *bool) bool {
	return b != nil && *b
}

// nonNullJSON returns b, or a JSON null literal when b is empty — a
// jsonb column read back as an empty []byte would otherwise serialize as
// an empty (invalid-JSON) string in the response body.
func nonNullJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}
