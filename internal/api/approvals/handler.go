package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/middleware"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/core/graph"
	"github.com/founderstack/api/internal/core/notify"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

type Handler struct {
	appPool    *pgxpool.Pool // app_user (RLS) — the authenticated list/get/approve/reject path
	systemPool *pgxpool.Pool // app_system (BYPASSRLS) — only the action-token path, before org context exists
	launcher   *graph.Launcher
	tokens     *notify.ActionTokenSigner
	authCache  *middleware.JWKCache
	cfg        *config.Config
}

func NewHandler(appPool, systemPool *pgxpool.Pool, launcher *graph.Launcher, tokens *notify.ActionTokenSigner, cfg *config.Config) *Handler {
	return &Handler{
		appPool: appPool, systemPool: systemPool, launcher: launcher,
		tokens: tokens, authCache: middleware.NewJWKCache(), cfg: cfg,
	}
}

// Register mounts the two read routes on rg — rg's group must already
// have middleware.RequireAuth applied, same as every other authenticated
// resource in this codebase.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/approvals", h.List)
	rg.GET("/approvals/:id", h.Get)
}

// RegisterActions mounts approve/reject on their own, deliberately
// ungated group — each resolves its own actor via resolveActor, which
// accepts either a normal Authorization: Bearer <Clerk JWT> (the in-app
// ApprovalCard's path) or an ?action_token= query param (a push
// notification's Approve/Reject button, which has no live Clerk session
// to attach — see notify.ActionTokenSigner's doc comment). Mirrors
// internal/api/integrations/handler.go's Register/RegisterCallback split
// — a second, differently-authenticated route group for the same
// resource is an established pattern in this codebase, not a new one.
func (h *Handler) RegisterActions(rg *gin.RouterGroup) {
	rg.POST("/approvals/:id/approve", h.Approve)
	rg.POST("/approvals/:id/reject", h.Reject)
}

type approvalSummary struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	RiskLevel string `json:"risk_level"`
	// ContextData is json.RawMessage, not []byte — a bare []byte field
	// marshals as a base64 string (encoding/json's default for byte
	// slices), which would silently hand founderstack-web a base64 blob
	// instead of the tool-call array it expects. RawMessage's MarshalJSON
	// passes the bytes through as literal JSON instead.
	ContextData json.RawMessage `json:"context_data"`
	ExpiresAt   *string         `json:"expires_at,omitempty"`
	CreatedAt   string          `json:"created_at"`
}

func toSummary(id, runID pgtype.UUID, status, riskLevel *string, contextData []byte, expiresAt, createdAt pgtype.Timestamptz) approvalSummary {
	return approvalSummary{
		ID: uuid.UUID(id.Bytes).String(), RunID: uuid.UUID(runID.Bytes).String(),
		Status: derefOr(status, ""), RiskLevel: derefOr(riskLevel, ""),
		ContextData: json.RawMessage(contextData), ExpiresAt: formatTimestamptz(expiresAt), CreatedAt: createdAt.Time.Format(rfc3339),
	}
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

	var rows []dbgen.ListApprovalsForOrgRow
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListApprovalsForOrg(ctx, dbgen.ListApprovalsForOrgParams{
			OrgID: user.OrgID, Limit: limit, Offset: offset, Status: statusFilter,
		})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list approvals")
		return
	}

	out := make([]approvalSummary, len(rows))
	for i, r := range rows {
		out[i] = toSummary(r.ID, r.RunID, r.Status, r.RiskLevel, r.ContextData, r.ExpiresAt, r.CreatedAt)
	}
	response.OK(c, http.StatusOK, "Approvals listed", gin.H{"approvals": out})
}

func (h *Handler) Get(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	id, ok := parseApprovalID(c)
	if !ok {
		return
	}

	var row dbgen.GetApprovalRow
	var notFound bool
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		row, err = q.GetApproval(ctx, dbgen.GetApprovalParams{OrgID: user.OrgID, ID: id})
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
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch approval")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "APPROVAL_NOT_FOUND", "Approval not found")
		return
	}
	response.OK(c, http.StatusOK, "Approval fetched", toSummary(row.ID, row.RunID, row.Status, row.RiskLevel, row.ContextData, row.ExpiresAt, row.CreatedAt))
}

// actor is the caller resolveActor identifies — either a real
// authctx.User (Bearer path) or an action-token-verified user id (push
// path). orgID always comes from a source the caller can't forge: the
// authenticated user's own org, or the approval row's own org_id.
type actor struct {
	userID uuid.UUID
	orgID  pgtype.UUID
}

// resolveActor tries the Authorization header first (the in-app
// ApprovalCard always sends one), then ?action_token= (a push
// notification's action buttons never can). Neither path trusts the
// caller's claimed identity as authorization by itself — Approve/Reject
// both re-check can_approve_workflows/expiry/agents_paused after this
// returns, exactly like the authenticated path would.
func (h *Handler) resolveActor(c *gin.Context, approvalID pgtype.UUID) (actor, bool) {
	ctx := c.Request.Context()

	if token := bearerToken(c.GetHeader("Authorization")); token != "" {
		clerkUserID, err := middleware.VerifyToken(ctx, h.authCache, h.cfg, token)
		if err != nil {
			response.Fail(c, http.StatusUnauthorized, "INVALID_TOKEN", "Invalid session token")
			return actor{}, false
		}
		user, err := middleware.ResolveUser(ctx, dbgen.New(h.systemPool), clerkUserID)
		if err != nil {
			response.Fail(c, http.StatusUnauthorized, "USER_NOT_SYNCHRONIZED", "User profile not synchronized")
			return actor{}, false
		}
		return actor{userID: uuid.UUID(user.ID.Bytes), orgID: user.OrgID}, true
	}

	if token := c.Query("action_token"); token != "" {
		// The approval's own org_id is the source of truth here, looked
		// up system-scoped since there's no tenant context yet to trust —
		// see GetApprovalSystemScoped's doc comment.
		approval, err := dbgen.New(h.systemPool).GetApprovalSystemScoped(ctx, approvalID)
		if err != nil {
			response.Fail(c, http.StatusNotFound, "APPROVAL_NOT_FOUND", "Approval not found")
			return actor{}, false
		}
		userID, err := h.tokens.Verify(token, uuid.UUID(approvalID.Bytes))
		if err != nil {
			response.Fail(c, http.StatusUnauthorized, "INVALID_TOKEN", "Invalid or expired action token")
			return actor{}, false
		}
		return actor{userID: userID, orgID: approval.OrgID}, true
	}

	response.Fail(c, http.StatusUnauthorized, "MISSING_AUTHORIZATION", "Missing Authorization header or action_token")
	return actor{}, false
}

func (h *Handler) Approve(c *gin.Context) {
	h.decide(c, true, "")
}

type rejectRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) Reject(c *gin.Context) {
	var req rejectRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Reason == "" {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "A non-empty \"reason\" is required to reject")
		return
	}
	h.decide(c, false, req.Reason)
}

// decide is Approve/Reject's shared body: resolve actor -> re-check
// permission/expiry/agents_paused -> UPDATE approvals (status='pending'
// guard, so a double-click or a race with the expiry job can't double-
// decide) -> INSERT approval_decisions -> audit log -> Launcher.Resume.
func (h *Handler) decide(c *gin.Context, approved bool, reason string) {
	id, ok := parseApprovalID(c)
	if !ok {
		return
	}
	who, ok := h.resolveActor(c, id)
	if !ok {
		return
	}

	var permRow dbgen.GetUserApprovalPermissionsRow
	var approvalRow dbgen.GetApprovalRow
	var agentsPaused bool
	var problem string // set to a response.Fail code when a guard fails, checked after the tx

	err := tenant.WithTx(c.Request.Context(), h.appPool, who.orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		permRow, err = q.GetUserApprovalPermissions(ctx, pgtype.UUID{Bytes: who.userID, Valid: true})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				problem = "USER_NOT_SYNCHRONIZED"
				return nil
			}
			return err
		}
		if permRow.CanApproveWorkflows == nil || !*permRow.CanApproveWorkflows {
			problem = "NOT_AUTHORIZED_TO_APPROVE"
			return nil
		}

		approvalRow, err = q.GetApproval(ctx, dbgen.GetApprovalParams{OrgID: who.orgID, ID: id})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				problem = "APPROVAL_NOT_FOUND"
				return nil
			}
			return err
		}
		if approvalRow.Status == nil || *approvalRow.Status != "pending" {
			problem = "APPROVAL_ALREADY_DECIDED"
			return nil
		}
		if approvalRow.ExpiresAt.Valid && approvalRow.ExpiresAt.Time.Before(nowUTC()) {
			problem = "APPROVAL_EXPIRED"
			return nil
		}

		settings, err := q.GetOrgRunSettings(ctx, who.orgID)
		if err != nil {
			return err
		}
		agentsPaused = settings.AgentsPaused

		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not evaluate approval")
		return
	}
	switch problem {
	case "USER_NOT_SYNCHRONIZED":
		response.Fail(c, http.StatusUnauthorized, problem, "User profile not synchronized")
		return
	case "NOT_AUTHORIZED_TO_APPROVE":
		response.Fail(c, http.StatusForbidden, problem, "You are not authorized to approve or reject workflow runs")
		return
	case "APPROVAL_NOT_FOUND":
		response.Fail(c, http.StatusNotFound, problem, "Approval not found")
		return
	case "APPROVAL_ALREADY_DECIDED":
		response.Fail(c, http.StatusConflict, problem, "This approval has already been decided")
		return
	case "APPROVAL_EXPIRED":
		response.Fail(c, http.StatusConflict, problem, "This approval has expired")
		return
	}
	// Approving is exactly what lets a write_destructive_or_financial tool
	// call execute — the org kill switch must block it here too, not just
	// at POST .../run (workflow 9's Preflight only guards *starting* a new
	// run, not resuming one already suspended).
	if approved && agentsPaused {
		response.Fail(c, http.StatusForbidden, "AGENTS_PAUSED", "Agents are paused for this organization")
		return
	}

	decision := "rejected"
	if approved {
		decision = "approved"
	}
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}

	err = tenant.WithTx(c.Request.Context(), h.appPool, who.orgID, func(ctx context.Context, q *dbgen.Queries) error {
		rowsAffected, err := q.UpdateApprovalStatus(ctx, dbgen.UpdateApprovalStatusParams{OrgID: who.orgID, ID: id, Status: &decision})
		if err != nil {
			return err
		}
		if rowsAffected == 0 {
			// Lost a race (double-click, or the expiry job beat us to it)
			// — the pre-check above already confirmed 'pending' moments
			// ago, but that's not a lock, so re-confirm atomically here.
			problem = "APPROVAL_ALREADY_DECIDED"
			return nil
		}
		if err := q.InsertApprovalDecision(ctx, dbgen.InsertApprovalDecisionParams{
			ApprovalID: id, UserID: pgtype.UUID{Bytes: who.userID, Valid: true}, Decision: decision, Reason: reasonPtr,
		}); err != nil {
			return err
		}
		metadata, err := json.Marshal(map[string]any{"approval_id": uuid.UUID(id.Bytes).String(), "run_id": uuid.UUID(approvalRow.RunID.Bytes).String()})
		if err != nil {
			return err
		}
		status := "success"
		action := "workflow.approval." + decision
		resourceType := "approval"
		return q.InsertAuditLog(ctx, dbgen.InsertAuditLogParams{
			OrgID: who.orgID, ActorID: pgtype.UUID{Bytes: who.userID, Valid: true}, ActorType: "user",
			Action: action, ResourceType: &resourceType, ResourceID: id, Status: &status, MetadataInfo: metadata,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not record decision")
		return
	}
	if problem == "APPROVAL_ALREADY_DECIDED" {
		response.Fail(c, http.StatusConflict, problem, "This approval has already been decided")
		return
	}

	h.launcher.Resume(uuid.UUID(who.orgID.Bytes), uuid.UUID(approvalRow.RunID.Bytes), approved, reason)
	response.OK(c, http.StatusOK, "Decision recorded", gin.H{"approval_id": uuid.UUID(id.Bytes).String(), "decision": decision})
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && header[:len(prefix)] == prefix {
		return header[len(prefix):]
	}
	return ""
}
