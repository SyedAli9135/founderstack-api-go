package practice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/db/dbgen"
)

// RestoreWindow is how long a removed client workspace stays restorable.
const RestoreWindow = 30 * 24 * time.Hour

type Handler struct {
	systemPool  *pgxpool.Pool
	provisioner WorkspaceProvisioner
}

// systemPool must be app_system: a portfolio spans several tenants, which
// no single RLS-scoped app_user transaction can see.
func NewHandler(systemPool *pgxpool.Pool, provisioner WorkspaceProvisioner) *Handler {
	return &Handler{systemPool: systemPool, provisioner: provisioner}
}

// RegisterIdentityOnly mounts routes that sit behind middleware.RequireIdentity
// rather than RequireAuth — see ListMyWorkspaces.
func (h *Handler) RegisterIdentityOnly(rg *gin.RouterGroup) {
	rg.GET("/me/workspaces", h.ListMyWorkspaces)
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/practice/client-workspaces", h.ListClientWorkspaces)
	rg.POST("/practice/client-workspaces", h.CreateClientWorkspace)
	rg.DELETE("/practice/client-workspaces/:id", h.RemoveClientWorkspace)
	rg.POST("/practice/client-workspaces/:id/restore", h.RestoreClientWorkspace)
	rg.GET("/practice/portfolio-summary", h.PortfolioSummary)
}

type workspaceRef struct {
	ID               string  `json:"id"`
	ClerkOrgID       string  `json:"clerk_org_id"`
	Name             string  `json:"name"`
	Slug             string  `json:"slug"`
	OrganizationType string  `json:"organization_type"`
	ParentPracticeID *string `json:"parent_practice_id"`
	Role             string  `json:"role"`
	IsCurrent        bool    `json:"is_current"`
}

// ListMyWorkspaces backs the workspace switcher: every active org the
// caller belongs to, whatever practice (if any) it sits under. It needs only
// a verified identity, not a usable active org — it's how the client
// recovers when the session's active org is deactivated or was never synced.
// Every row is still scoped to the caller's own active memberships.
func (h *Handler) ListMyWorkspaces(c *gin.Context) {
	id, ok := authctx.IdentityFromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	rows, err := dbgen.New(h.systemPool).ListWorkspacesForClerkUser(c.Request.Context(), id.ClerkUserID)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list workspaces")
		return
	}
	out := make([]workspaceRef, len(rows))
	for i, r := range rows {
		out[i] = workspaceRef{
			ID: r.ID.String(), ClerkOrgID: r.ClerkOrgID, Name: r.Name, Slug: r.Slug,
			OrganizationType: r.OrganizationType, ParentPracticeID: uuidPtr(r.ParentPracticeID),
			Role: r.Role, IsCurrent: id.ClerkOrgID != "" && r.ClerkOrgID == id.ClerkOrgID,
		}
	}
	response.OK(c, http.StatusOK, "Workspaces listed", gin.H{"workspaces": out})
}

// practiceContext is the practice a /practice request operates on, plus
// the caller's own membership in it.
type practiceContext struct {
	id   pgtype.UUID
	name string
	typ  string
	role string
}

func (p practiceContext) callerIsOwnerOrAdmin() bool {
	return p.role == "owner" || p.role == "admin"
}

// resolvePractice works from anywhere in the portfolio: from a client
// workspace it resolves to the parent practice. The caller must hold their
// own active membership in that practice — belonging to one client
// workspace (say, as the client's own staff) grants nothing over its
// siblings or its practice.
func (h *Handler) resolvePractice(c *gin.Context, user authctx.User) (practiceContext, bool) {
	practiceID := user.OrgID
	if user.OrganizationType == "client_workspace" {
		practiceID = user.ParentPracticeID
	}
	ctx := c.Request.Context()
	q := dbgen.New(h.systemPool)

	org, err := q.GetActiveOrganizationByID(ctx, practiceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "PRACTICE_NOT_FOUND", "Practice not found or inactive")
			return practiceContext{}, false
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not resolve practice")
		return practiceContext{}, false
	}
	member, err := q.GetActiveUserInOrg(ctx, dbgen.GetActiveUserInOrgParams{OrgID: practiceID, ClerkUserID: user.ClerkUserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusForbidden, "NOT_A_PRACTICE_MEMBER", "You are not a member of this workspace's practice")
			return practiceContext{}, false
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not resolve practice")
		return practiceContext{}, false
	}
	return practiceContext{id: org.ID, name: org.Name, typ: org.OrganizationType, role: member.Role}, true
}

type workspaceStats struct {
	HoursSaved       float64 `json:"hours_saved"`
	ActiveRuns       int64   `json:"active_runs"`
	PendingApprovals int64   `json:"pending_approvals"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
}

type clientWorkspace struct {
	ID                 string         `json:"id"`
	ClerkOrgID         string         `json:"clerk_org_id"`
	Name               string         `json:"name"`
	Slug               string         `json:"slug"`
	Status             string         `json:"status"`
	ClientContactEmail *string        `json:"client_contact_email"`
	CreatedAt          string         `json:"created_at"`
	DeactivatedAt      *string        `json:"deactivated_at"`
	RestorableUntil    *string        `json:"restorable_until"`
	Stats              workspaceStats `json:"stats"`
}

type workspaceSettings struct {
	ClientContactEmail string `json:"client_contact_email,omitempty"`
}

func (h *Handler) ListClientWorkspaces(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	p, ok := h.resolvePractice(c, user)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	q := dbgen.New(h.systemPool)
	rows, err := q.ListClientWorkspacesWithStats(ctx, dbgen.ListClientWorkspacesWithStatsParams{
		ParentPracticeID: p.id, ClerkUserID: user.ClerkUserID,
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list client workspaces")
		return
	}
	limit, err := q.GetPracticeLimit(ctx, p.id)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list client workspaces")
		return
	}

	now := time.Now()
	out := make([]clientWorkspace, 0, len(rows))
	active := 0
	for _, r := range rows {
		ws := clientWorkspace{
			ID: r.ID.String(), ClerkOrgID: r.ClerkOrgID, Name: r.Name, Slug: r.Slug,
			ClientContactEmail: contactEmail(r.Settings), CreatedAt: r.CreatedAt.Time.Format(time.RFC3339),
			Stats: workspaceStats{
				HoursSaved: r.HoursSaved, ActiveRuns: r.ActiveRuns,
				PendingApprovals: r.PendingApprovals, TotalCostUSD: r.TotalCostUsd,
			},
		}
		ws.Status, ws.DeactivatedAt, ws.RestorableUntil = lifecycle(r.IsActive, r.DeactivatedAt, now)
		if ws.Status == "active" {
			active++
		}
		out = append(out, ws)
	}
	response.OK(c, http.StatusOK, "Client workspaces listed", gin.H{
		"practice": gin.H{
			"id": p.id.String(), "name": p.name, "organization_type": p.typ, "role": p.role,
			"max_client_workspaces": limit, "active_client_workspaces": active,
		},
		"workspaces": out,
	})
}

// lifecycle maps an org's is_active/deactivated_at to its API status:
// "active", "deactivated" (restorable), or "expired" (past the window).
func lifecycle(isActive *bool, deactivatedAt pgtype.Timestamptz, now time.Time) (status string, deactivated, restorableUntil *string) {
	if isActive != nil && *isActive {
		return "active", nil, nil
	}
	if !deactivatedAt.Valid {
		return "expired", nil, nil
	}
	d := deactivatedAt.Time.Format(time.RFC3339)
	until := deactivatedAt.Time.Add(RestoreWindow)
	u := until.Format(time.RFC3339)
	if now.After(until) {
		return "expired", &d, &u
	}
	return "deactivated", &d, &u
}

func contactEmail(settings []byte) *string {
	var s workspaceSettings
	if len(settings) == 0 || json.Unmarshal(settings, &s) != nil || s.ClientContactEmail == "" {
		return nil
	}
	return &s.ClientContactEmail
}

type createRequest struct {
	Name               string `json:"name"`
	ClientContactEmail string `json:"client_contact_email"`
}

var errLimitReached = errors.New("practice: client workspace limit reached")

func (h *Handler) CreateClientWorkspace(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.ClientContactEmail = strings.TrimSpace(req.ClientContactEmail)
	if req.Name == "" || len(req.Name) > 255 {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "name is required (max 255 characters)")
		return
	}
	if req.ClientContactEmail != "" {
		if addr, err := mail.ParseAddress(req.ClientContactEmail); err != nil || addr.Address != req.ClientContactEmail {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "client_contact_email is not a valid email address")
			return
		}
	}

	p, ok := h.resolvePractice(c, user)
	if !ok {
		return
	}
	if !p.callerIsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only a practice owner or admin can create client workspaces")
		return
	}

	ctx := c.Request.Context()
	// Cheap pre-check so an at-limit practice never creates a Clerk org at
	// all; the locked re-check inside the transaction is the real guard.
	if err := h.checkCapacity(ctx, dbgen.New(h.systemPool), p.id, false); err != nil {
		h.failCapacity(c, err)
		return
	}

	slug := newSlug(req.Name)
	clerkOrgID, err := h.provisioner.Create(ctx, req.Name, user.ClerkUserID)
	if err != nil {
		slog.Error("practice: clerk create organization failed", "err", err, "request_id", response.RequestID(c))
		response.Fail(c, http.StatusBadGateway, "WORKSPACE_PROVISIONING_FAILED", "Could not create the client workspace")
		return
	}

	settings, _ := json.Marshal(workspaceSettings{ClientContactEmail: req.ClientContactEmail})
	profile, err := dbgen.New(h.systemPool).GetUserProfile(ctx, user.ID)
	if err != nil {
		h.rollbackProvisioning(ctx, clerkOrgID)
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not create the client workspace")
		return
	}
	var orgID pgtype.UUID
	err = pgx.BeginFunc(ctx, h.systemPool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := h.checkCapacity(ctx, q, p.id, true); err != nil {
			return err
		}
		if err := q.MarkOrganizationAsPractice(ctx, p.id); err != nil {
			return err
		}
		var err error
		orgID, err = q.UpsertClientWorkspace(ctx, dbgen.UpsertClientWorkspaceParams{
			ClerkOrgID: clerkOrgID, Name: req.Name, Slug: slug, ParentPracticeID: p.id, Settings: settings,
		})
		if err != nil {
			return err
		}
		// Written now rather than left to the organizationMembership.created
		// webhook, so switching into the new workspace works immediately.
		// Same values the webhook will write (Clerk makes the creator an
		// admin), so its later arrival is a no-op.
		isAdmin := true
		return q.UpsertUserForMembership(ctx, dbgen.UpsertUserForMembershipParams{
			OrgID: orgID, ClerkUserID: user.ClerkUserID, Email: profile.Email, FullName: profile.FullName,
			Role: "admin", CanApproveWorkflows: &isAdmin, CanManageApiKeys: &isAdmin, CanManageIntegrations: &isAdmin,
		})
	})
	if err != nil {
		h.rollbackProvisioning(ctx, clerkOrgID)
		if errors.Is(err, errLimitReached) {
			h.failCapacity(c, err)
			return
		}
		slog.Error("practice: insert client workspace failed", "err", err, "request_id", response.RequestID(c))
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not create the client workspace")
		return
	}

	var contact *string
	if req.ClientContactEmail != "" {
		contact = &req.ClientContactEmail
	}
	response.OK(c, http.StatusCreated, "Client workspace created", clientWorkspace{
		ID: orgID.String(), ClerkOrgID: clerkOrgID, Name: req.Name, Slug: slug, Status: "active",
		ClientContactEmail: contact, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// checkCapacity returns errLimitReached when the practice is already at
// max_client_workspaces. lock takes the practice row lock, and must only
// be true inside a transaction.
func (h *Handler) checkCapacity(ctx context.Context, q *dbgen.Queries, practiceID pgtype.UUID, lock bool) error {
	var max int32
	if lock {
		p, err := q.GetPracticeForUpdate(ctx, practiceID)
		if err != nil {
			return err
		}
		max = p.MaxClientWorkspaces
	} else {
		var err error
		if max, err = q.GetPracticeLimit(ctx, practiceID); err != nil {
			return err
		}
	}
	n, err := q.CountActiveClientWorkspaces(ctx, practiceID)
	if err != nil {
		return err
	}
	if n >= int64(max) {
		return errLimitReached
	}
	return nil
}

func (h *Handler) failCapacity(c *gin.Context, err error) {
	if errors.Is(err, errLimitReached) {
		response.Fail(c, http.StatusPaymentRequired, "CLIENT_WORKSPACE_LIMIT_REACHED",
			"Your plan's client workspace limit is reached. Remove a workspace or upgrade your plan.")
		return
	}
	response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not check plan limits")
}

// rollbackProvisioning deletes a Clerk org whose local insert failed. Runs
// detached from the request's cancellation — a client disconnecting must
// not leave the orphan behind.
func (h *Handler) rollbackProvisioning(ctx context.Context, clerkOrgID string) {
	if err := h.provisioner.Delete(context.WithoutCancel(ctx), clerkOrgID); err != nil {
		slog.Error("practice: orphaned clerk organization after failed insert", "clerk_org_id", clerkOrgID, "err", err)
	}
}

func (h *Handler) RemoveClientWorkspace(c *gin.Context) {
	user, p, ws, ok := h.loadWorkspaceForMutation(c)
	if !ok {
		return
	}
	if ws.IsActive == nil || !*ws.IsActive {
		response.OK(c, http.StatusOK, "Client workspace already deactivated", gin.H{"id": ws.ID.String(), "status": "deactivated"})
		return
	}
	if _, err := dbgen.New(h.systemPool).DeactivateClientWorkspace(c.Request.Context(), dbgen.DeactivateClientWorkspaceParams{
		ID: ws.ID, ParentPracticeID: p.id,
	}); err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not deactivate the client workspace")
		return
	}
	slog.Info("practice: client workspace deactivated", "org_id", ws.ID.String(), "by", user.ClerkUserID)
	until := time.Now().Add(RestoreWindow).UTC().Format(time.RFC3339)
	response.OK(c, http.StatusOK, "Client workspace deactivated", gin.H{
		"id": ws.ID.String(), "status": "deactivated", "restorable_until": until,
	})
}

func (h *Handler) RestoreClientWorkspace(c *gin.Context) {
	_, p, ws, ok := h.loadWorkspaceForMutation(c)
	if !ok {
		return
	}
	if ws.IsActive != nil && *ws.IsActive {
		response.OK(c, http.StatusOK, "Client workspace already active", gin.H{"id": ws.ID.String(), "status": "active"})
		return
	}
	// Checked before capacity: an expired workspace can never come back, so
	// "limit reached" would point the caller at a fix that can't work.
	if !ws.DeactivatedAt.Valid || time.Since(ws.DeactivatedAt.Time) > RestoreWindow {
		response.Fail(c, http.StatusGone, "RESTORE_WINDOW_EXPIRED", "This workspace is past its 30-day restore window")
		return
	}
	ctx := c.Request.Context()
	var restored int64
	err := pgx.BeginFunc(ctx, h.systemPool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := h.checkCapacity(ctx, q, p.id, true); err != nil {
			return err
		}
		var err error
		restored, err = q.RestoreClientWorkspace(ctx, dbgen.RestoreClientWorkspaceParams{ID: ws.ID, ParentPracticeID: p.id})
		return err
	})
	if err != nil {
		if errors.Is(err, errLimitReached) {
			h.failCapacity(c, err)
		} else {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not restore the client workspace")
		}
		return
	}
	if restored == 0 {
		response.Fail(c, http.StatusGone, "RESTORE_WINDOW_EXPIRED", "This workspace is past its 30-day restore window")
		return
	}
	response.OK(c, http.StatusOK, "Client workspace restored", gin.H{"id": ws.ID.String(), "status": "active"})
}

// loadWorkspaceForMutation resolves :id to a client workspace under the
// caller's practice that the caller is a member of, and requires the
// caller to be a practice owner/admin. Any miss is a 404, so a workspace
// in someone else's practice is indistinguishable from a nonexistent one.
func (h *Handler) loadWorkspaceForMutation(c *gin.Context) (authctx.User, practiceContext, dbgen.GetClientWorkspaceForCallerRow, bool) {
	var none dbgen.GetClientWorkspaceForCallerRow
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return user, practiceContext{}, none, false
	}
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid workspace id")
		return user, practiceContext{}, none, false
	}
	p, ok := h.resolvePractice(c, user)
	if !ok {
		return user, p, none, false
	}
	if !p.callerIsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only a practice owner or admin can manage client workspaces")
		return user, p, none, false
	}
	ws, err := dbgen.New(h.systemPool).GetClientWorkspaceForCaller(c.Request.Context(), dbgen.GetClientWorkspaceForCallerParams{
		ID: pgtype.UUID{Bytes: parsed, Valid: true}, ParentPracticeID: p.id, ClerkUserID: user.ClerkUserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "WORKSPACE_NOT_FOUND", "Client workspace not found")
		} else {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load the client workspace")
		}
		return user, p, none, false
	}
	return user, p, ws, true
}

func (h *Handler) PortfolioSummary(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	p, ok := h.resolvePractice(c, user)
	if !ok {
		return
	}
	s, err := dbgen.New(h.systemPool).GetPortfolioSummary(c.Request.Context(), dbgen.GetPortfolioSummaryParams{
		ParentPracticeID: p.id, ClerkUserID: user.ClerkUserID,
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not compute portfolio summary")
		return
	}
	response.OK(c, http.StatusOK, "Portfolio summary fetched", gin.H{
		"active_workspaces": s.ActiveWorkspaces,
		"hours_saved":       s.HoursSaved,
		"active_runs":       s.ActiveRuns,
		"pending_approvals": s.PendingApprovals,
		"total_cost_usd":    s.TotalCostUsd,
	})
}

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// newSlug derives a readable slug from name plus a random suffix, since
// organizations.slug is globally unique and client names often collide
// ("Acme" under two different practices).
func newSlug(name string) string {
	base := strings.Trim(nonSlugChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(base) > 40 {
		base = strings.TrimRight(base[:40], "-")
	}
	if base == "" {
		base = "workspace"
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return base + "-" + hex.EncodeToString(b)
}

func uuidPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := u.String()
	return &s
}
