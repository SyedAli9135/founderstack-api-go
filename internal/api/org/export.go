package org

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/pkg/ratelimit"
)

// exportRowCap bounds the two unbounded-growth tables so one export can't
// become an unbounded read; newest rows win.
const exportRowCap = 50000

// exportLimiter: one export per workspace per hour is plenty for a data request.
var exportLimiter *ratelimit.Limiter

// EnableExportLimit turns on the per-workspace export limit (needs Redis).
func EnableExportLimit(rdb *redis.Client) { exportLimiter = ratelimit.New(rdb, 3, time.Hour) }

// exportSections maps a section name to a query returning one jsonb document.
// Secrets never leave: encrypted keys and OAuth credentials are not selected,
// and files are listed by metadata only. Every query is also RLS-scoped.
var exportSections = []struct{ name, sql string }{
	{"organization", `SELECT to_jsonb(o) - 'active_api_key_id' - 'stripe_customer_id' - 'stripe_subscription_id' FROM organizations o WHERE o.id = current_org_id()`},
	{"members", `SELECT COALESCE(jsonb_agg(jsonb_build_object('email', email, 'full_name', full_name, 'role', role, 'is_active', is_active,
		'can_manage_api_keys', can_manage_api_keys, 'can_manage_integrations', can_manage_integrations,
		'can_approve_workflows', can_approve_workflows, 'created_at', created_at, 'last_login_at', last_login_at)), '[]') FROM users`},
	{"agents", `SELECT COALESCE(jsonb_agg(to_jsonb(a)), '[]') FROM agents a`},
	{"workflows", `SELECT COALESCE(jsonb_agg(to_jsonb(w)), '[]') FROM workflows w`},
	{"teams", `SELECT COALESCE(jsonb_agg(to_jsonb(t)), '[]') FROM agent_teams t`},
	{"workflow_runs", `SELECT COALESCE(jsonb_agg(to_jsonb(r) - 'checkpoint_state'), '[]') FROM
		(SELECT * FROM workflow_runs ORDER BY created_at DESC LIMIT 50000) r`},
	{"approvals", `SELECT COALESCE(jsonb_agg(to_jsonb(a)), '[]') FROM (SELECT * FROM approvals ORDER BY created_at DESC LIMIT 50000) a`},
	{"documents", `SELECT COALESCE(jsonb_agg(to_jsonb(d) - 's3_path'), '[]') FROM documents d`},
	{"api_keys", `SELECT COALESCE(jsonb_agg(jsonb_build_object('provider', provider, 'key_prefix', key_prefix, 'is_valid', is_valid, 'created_at', created_at)), '[]') FROM api_key_registry`},
	{"integrations", `SELECT COALESCE(jsonb_agg(jsonb_build_object('service', service_name, 'status', oauth_status, 'scopes', oauth_scopes, 'is_active', is_active, 'created_at', created_at)), '[]') FROM mcp_connections`},
	{"client_reports", `SELECT COALESCE(jsonb_agg(to_jsonb(c) - 'share_token'), '[]') FROM client_reports c`},
	{"audit_logs", `SELECT COALESCE(jsonb_agg(to_jsonb(a)), '[]') FROM (SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT 50000) a`},
	{"cost_ledger", `SELECT COALESCE(jsonb_agg(to_jsonb(c)), '[]') FROM (SELECT * FROM cost_ledger ORDER BY created_at DESC LIMIT 50000) c`},
}

// Export is GET /org/export: the workspace's data as one JSON download (GDPR
// access/portability). Owner/admin only; never includes credentials.
func (h *Handler) Export(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.IsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only owners and admins can export workspace data")
		return
	}
	if exportLimiter != nil && !exportLimiter.Allow(c.Request.Context(), "export:"+user.OrgID.String()) {
		response.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED", "Workspace data can be exported 3 times per hour")
		return
	}

	out := map[string]json.RawMessage{}
	err := h.exportTx(c.Request.Context(), user.OrgID.String(), out)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not export workspace data")
		return
	}
	body, err := json.Marshal(gin.H{
		"exported_at":  time.Now().UTC().Format(time.RFC3339),
		"row_cap":      exportRowCap,
		"data":         out,
		"not_included": "Encrypted API keys and integration credentials, uploaded file contents, and run checkpoints.",
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not export workspace data")
		return
	}
	c.Header("Content-Disposition", `attachment; filename="founderstack-export.json"`)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", body)
}

// exportTx reads every section in one read-only snapshot scoped to the org.
func (h *Handler) exportTx(ctx context.Context, orgID string, out map[string]json.RawMessage) error {
	tx, err := h.appPool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.current_org_id', $1, true)`, orgID); err != nil {
		return err
	}
	for _, s := range exportSections {
		var raw []byte
		if err := tx.QueryRow(ctx, s.sql).Scan(&raw); err != nil {
			return err
		}
		out[s.name] = raw
	}
	return nil
}
