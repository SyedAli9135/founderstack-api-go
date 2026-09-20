// Package templates implements workflow 19 (Agent Templates Marketplace):
// a founder browses a shared, global gallery of pre-built agents and
// installs one with a single click. Templates themselves are
// admin-curated seed data (migration 000015), not created through this
// API — the plan's own checklist listed a `POST /templates` admin-only
// endpoint guarded by an "internal PSK," but no PSK/internal-auth
// mechanism exists anywhere else in this codebase yet, and building one
// for a rare, low-frequency operation (adding a new template) ahead of
// any real need would be exactly the kind of speculative infra this
// codebase's own Dependency policy already argues against. Add a new
// template the same way the founder's own [TEST] fixtures got created —
// a direct INSERT — until that changes.
package templates

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

const (
	defaultMaxOutputTokens = int32(4096)
	defaultTemperature     = 0.3
)

type Handler struct {
	appPool *pgxpool.Pool
}

func NewHandler(appPool *pgxpool.Pool) *Handler {
	return &Handler{appPool: appPool}
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/templates", h.List)
	rg.GET("/templates/:id", h.Get)
	rg.POST("/templates/:id/install", h.Install)
}

func parseTemplateID(c *gin.Context) (pgtype.UUID, bool) {
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid template id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

type templateSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Icon        string `json:"icon"`
	IsFeatured  bool   `json:"is_featured"`
	ToolCount   int32  `json:"tool_count"`
}

// List is GET /templates — the gallery. Global, not org-scoped (see
// package doc), so every request just runs a plain query, no
// tenant.WithTx — there is no tenant boundary to enforce here.
func (h *Handler) List(c *gin.Context) {
	if _, ok := authctx.FromContext(c); !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	var category *string
	if v := strings.TrimSpace(c.Query("category")); v != "" {
		category = &v
	}

	rows, err := dbgen.New(h.appPool).ListAgentTemplates(c.Request.Context(), category)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list templates")
		return
	}

	out := make([]templateSummary, len(rows))
	for i, r := range rows {
		out[i] = templateSummary{
			ID: r.ID.String(), Name: r.Name, Description: r.Description, Category: r.Category,
			Icon: r.Icon, IsFeatured: r.IsFeatured, ToolCount: r.ToolCount,
		}
	}
	response.OK(c, http.StatusOK, "Templates fetched", out)
}

type templateDetail struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Category     string   `json:"category"`
	SystemPrompt string   `json:"system_prompt"`
	Model        string   `json:"model"`
	AllowedTools []string `json:"allowed_tools"`
	Icon         string   `json:"icon"`
	IsFeatured   bool     `json:"is_featured"`
}

// Get is GET /templates/{id} — the full preview (system prompt + tool
// list) a founder sees before installing.
func (h *Handler) Get(c *gin.Context) {
	if _, ok := authctx.FromContext(c); !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseTemplateID(c)
	if !ok {
		return
	}

	row, err := dbgen.New(h.appPool).GetAgentTemplate(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "TEMPLATE_NOT_FOUND", "Template not found")
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch template")
		return
	}

	tools, err := allowedToolsFromPolicyScope(row.PolicyScope)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read template's tool list")
		return
	}

	response.OK(c, http.StatusOK, "Template fetched", templateDetail{
		ID: row.ID.String(), Name: row.Name, Description: row.Description, Category: row.Category,
		SystemPrompt: row.SystemPrompt, Model: row.Model, AllowedTools: tools,
		Icon: row.Icon, IsFeatured: row.IsFeatured,
	})
}

type installResponse struct {
	AgentID string `json:"agent_id"`
}

// Install is POST /templates/{id}/install — reads the template, then
// INSERTs a new `agents` row for this org pre-filled from it (name,
// system_prompt, model, policy_scope — the plan's own exact field list).
// The installed agent is a completely ordinary agent from that point on:
// fully editable via PATCH /agents/{id} like any other, same as the
// plan's own acceptance criterion ("template is just a starting point").
func (h *Handler) Install(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.CanModifyAgents() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can install agent templates")
		return
	}
	id, ok := parseTemplateID(c)
	if !ok {
		return
	}

	tmpl, err := dbgen.New(h.appPool).GetAgentTemplate(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "TEMPLATE_NOT_FOUND", "Template not found")
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch template")
		return
	}

	tools, err := allowedToolsFromPolicyScope(tmpl.PolicyScope)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read template's tool list")
		return
	}
	allowedServersJSON, err := json.Marshal(serversFromTools(tools))
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not encode allowed_mcp_servers")
		return
	}

	maxOutputTokens := defaultMaxOutputTokens
	temperature := float64(defaultTemperature)
	model := tmpl.Model

	var agentID pgtype.UUID
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

		// A second install of the same template is a completely
		// reasonable thing to want (e.g. one GitHub PR Reviewer per
		// repo) — retry once with a disambiguated name rather than
		// rejecting outright the way agents.Handler.Create does for a
		// founder's own deliberate rename collision.
		for attempt, name := range []string{tmpl.Name, tmpl.Name + " (2)"} {
			r, err := q.InsertAgent(ctx, dbgen.InsertAgentParams{
				OrgID: user.OrgID, Name: name, Slug: slugify(name), Description: &tmpl.Description,
				AgentType: "specialist", Model: &model, SystemPrompt: tmpl.SystemPrompt,
				MaxOutputTokens: &maxOutputTokens, Temperature: &temperature,
				PolicyScope: tmpl.PolicyScope, AllowedMcpServers: allowedServersJSON, CreatedBy: user.ID,
			})
			if err != nil {
				// ON CONFLICT ... DO NOTHING means a name collision
				// surfaces as pgx.ErrNoRows on this :one query's Scan
				// (no row to return), not a Postgres error — the same
				// distinction agents.Handler.Create's own duplicate-name
				// handling already makes. Only that specific case should
				// fall through to the next candidate name; anything else
				// is a real failure worth aborting the whole install on.
				if errors.Is(err, pgx.ErrNoRows) {
					if attempt == 1 {
						duplicate = true
					}
					continue
				}
				return err
			}
			agentID = r.ID
			return nil
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not install template")
		return
	}
	if limitReached {
		response.Fail(c, http.StatusBadRequest, "PLAN_LIMIT_REACHED", "Your plan's agent limit has been reached")
		return
	}
	if duplicate {
		response.Fail(c, http.StatusBadRequest, "DUPLICATE_AGENT_NAME",
			"You already have 2 agents installed from this template — rename or delete one first")
		return
	}

	response.OK(c, http.StatusCreated, "Agent installed", installResponse{AgentID: agentID.String()})
}

func allowedToolsFromPolicyScope(raw []byte) ([]string, error) {
	var ps struct {
		AllowedTools []string `json:"allowed_tools"`
	}
	if err := json.Unmarshal(raw, &ps); err != nil {
		return nil, err
	}
	return ps.AllowedTools, nil
}

// serversFromTools derives allowed_mcp_servers from allowed_tools —
// duplicated from agents.Handler's own unexported helper of the same
// name/shape rather than exported across packages for one small pure
// function; see this codebase's own established per-package duplication
// convention (e.g. formatTimestamptz).
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

// slugify duplicated from agents.Handler for the same reason as
// serversFromTools above.
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
