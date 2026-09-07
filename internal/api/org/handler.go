package org

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

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

// validRoles is this app's own role vocabulary — a finer hierarchy than
// Clerk's own default org roles (typically just admin/member), which is
// exactly why role sync to Clerk (MembershipSyncer.UpdateRole) is
// best-effort, not authoritative. No DB CHECK constraint enforces this
// (the Clerk webhook's own sync path writes whatever Clerk sends,
// normalized — see clerk.go's normalizeRole — and a constraint could
// reject a legitimate webhook payload this app doesn't control).
var validRoles = map[string]bool{"owner": true, "admin": true, "member": true, "viewer": true}

type Handler struct {
	appPool     *pgxpool.Pool
	syncer      MembershipSyncer
	invitations InvitationLister
}

func NewHandler(appPool *pgxpool.Pool, syncer MembershipSyncer, invitations InvitationLister) *Handler {
	return &Handler{appPool: appPool, syncer: syncer, invitations: invitations}
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/org/members", h.List)
	rg.PATCH("/org/members/:user_id/role", h.UpdateRole)
	rg.DELETE("/org/members/:user_id", h.Remove)
	rg.GET("/org/invitations", h.ListInvitations)
	rg.DELETE("/org/invitations/:invitation_id", h.RevokeInvitation)
}

type member struct {
	ID                    string  `json:"id"`
	Email                 string  `json:"email"`
	FullName              *string `json:"full_name,omitempty"`
	AvatarURL             *string `json:"avatar_url,omitempty"`
	Role                  string  `json:"role"`
	CanManageAPIKeys      bool    `json:"can_manage_api_keys"`
	CanManageIntegrations bool    `json:"can_manage_integrations"`
	CanApproveWorkflows   bool    `json:"can_approve_workflows"`
	LastLoginAt           *string `json:"last_login_at,omitempty"`
	CreatedAt             string  `json:"created_at"`
	// ClerkUserID lets the frontend identify "this row is me" by comparing
	// against Clerk's own useUser().user.id, without a separate /me
	// endpoint — this app's local user id is otherwise meaningless to the
	// frontend, which only ever sees Clerk's identity.
	ClerkUserID string `json:"clerk_user_id"`
}

func (h *Handler) List(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var rows []dbgen.ListOrgMembersRow
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListOrgMembers(ctx, user.OrgID)
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list team members")
		return
	}

	members := make([]member, len(rows))
	for i, r := range rows {
		members[i] = member{
			ID: r.ID.String(), Email: r.Email, FullName: r.FullName, AvatarURL: r.AvatarUrl, Role: r.Role,
			CanManageAPIKeys: derefBool(r.CanManageApiKeys), CanManageIntegrations: derefBool(r.CanManageIntegrations),
			CanApproveWorkflows: derefBool(r.CanApproveWorkflows), LastLoginAt: timestamptzPtr(r.LastLoginAt),
			CreatedAt: r.CreatedAt.Time.Format(time.RFC3339), ClerkUserID: r.ClerkUserID,
		}
	}
	response.OK(c, http.StatusOK, "Team members listed", gin.H{"members": members})
}

type updateRoleRequest struct {
	Role string `json:"role"`
}

// defaultPermissionsForRole derives the 3 boolean permission flags from a
// role — they're not independently settable via this API (see
// UpdateMemberRoleAndPermissions's own doc comment for why).
func defaultPermissionsForRole(role string) (canManageAPIKeys, canManageIntegrations, canApproveWorkflows bool) {
	admin := role == "owner" || role == "admin"
	return admin, admin, admin
}

func (h *Handler) UpdateRole(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.IsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can change a member's role")
		return
	}
	targetID, ok := parseUserID(c)
	if !ok {
		return
	}
	var req updateRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil || !validRoles[req.Role] {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "role must be one of: owner, admin, member, viewer")
		return
	}

	ctx := c.Request.Context()
	var target dbgen.GetOrgMemberForUpdateRow
	var notFound bool
	err := tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		target, err = q.GetOrgMemberForUpdate(ctx, dbgen.GetOrgMemberForUpdateParams{OrgID: user.OrgID, ID: targetID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				notFound = true
				return nil
			}
			return err
		}
		apiKeys, integrations, approve := defaultPermissionsForRole(req.Role)
		return q.UpdateMemberRoleAndPermissions(ctx, dbgen.UpdateMemberRoleAndPermissionsParams{
			OrgID: user.OrgID, ID: targetID, Role: req.Role,
			CanManageApiKeys: &apiKeys, CanManageIntegrations: &integrations, CanApproveWorkflows: &approve,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not update member role")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "MEMBER_NOT_FOUND", "Team member not found")
		return
	}

	// Best-effort: Clerk rejecting this app's finer-grained role vocabulary
	// (e.g. "viewer", which Clerk's own default org roles don't have)
	// must not undo the local change Postgres just committed — see
	// MembershipSyncer's own doc comment.
	if err := h.syncer.UpdateRole(ctx, user.ClerkOrgID, target.ClerkUserID, req.Role); err != nil {
		slog.Warn("org: clerk role sync failed, local role change stands", "member_id", targetID.String(), "err", err)
	}

	response.OK(c, http.StatusOK, "Role updated", gin.H{"id": targetID.String(), "role": req.Role})
}

func (h *Handler) Remove(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.IsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can remove a member")
		return
	}
	targetID, ok := parseUserID(c)
	if !ok {
		return
	}
	if targetID == user.ID {
		response.Fail(c, http.StatusBadRequest, "CANNOT_REMOVE_SELF", "You cannot remove yourself from the team")
		return
	}

	ctx := c.Request.Context()
	var target dbgen.GetOrgMemberForUpdateRow
	var notFound bool
	err := tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		target, err = q.GetOrgMemberForUpdate(ctx, dbgen.GetOrgMemberForUpdateParams{OrgID: user.OrgID, ID: targetID})
		if errors.Is(err, pgx.ErrNoRows) {
			notFound = true
			return nil
		}
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not look up member")
		return
	}
	if notFound {
		response.Fail(c, http.StatusNotFound, "MEMBER_NOT_FOUND", "Team member not found")
		return
	}

	// Real Clerk removal happens BEFORE the local soft-delete, and a
	// failure here stops the request entirely (not best-effort, unlike
	// UpdateRole) — the Clerk webhook's UpsertUserForMembership sets
	// is_active=true unconditionally on any future organizationMembership
	// re-sync (a role change, a metadata update, Clerk resending an
	// event). If the Clerk-side membership were left intact after a local
	// soft-delete, an unrelated future webhook could silently resurrect
	// this member's access — deleting the Clerk membership for real is
	// what prevents that webhook from ever firing again for this user.
	if err := h.syncer.Remove(ctx, user.ClerkOrgID, target.ClerkUserID); err != nil {
		slog.Error("org: clerk membership removal failed, local record left untouched", "member_id", targetID.String(), "err", err)
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not remove member from the organization")
		return
	}

	if err := tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.DeactivateMember(ctx, dbgen.DeactivateMemberParams{OrgID: user.OrgID, ID: targetID})
	}); err != nil {
		// Clerk-side removal already succeeded and can't be undone here —
		// log loudly; a stuck-active local row for an already-Clerk-removed
		// user is a real inconsistency but not a security regression (their
		// Clerk session/JWT will fail to verify going forward regardless).
		slog.Error("org: local deactivation failed after clerk removal succeeded", "member_id", targetID.String(), "err", err)
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Removed from Clerk but could not update local record")
		return
	}

	response.OK(c, http.StatusOK, "Member removed", gin.H{"id": targetID.String()})
}

type invitation struct {
	ID        string  `json:"id"`
	Email     string  `json:"email"`
	Role      string  `json:"role"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// ListInvitations reads live from Clerk on every call — Postgres has
// nothing to read here (a pending invite isn't a `users` row; it only
// becomes one once accepted and the webhook syncs it). Only pending
// invitations are ever returned — see InvitationLister's own doc comment.
func (h *Handler) ListInvitations(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.IsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can view pending invitations")
		return
	}

	invites, err := h.invitations.List(c.Request.Context(), user.ClerkOrgID)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch pending invitations")
		return
	}

	out := make([]invitation, len(invites))
	for i, inv := range invites {
		out[i] = invitation{
			ID: inv.ID, Email: inv.Email, Role: inv.Role, Status: inv.Status,
			CreatedAt: clerkMillisToRFC3339(inv.CreatedAt), ExpiresAt: clerkMillisPtrToRFC3339(inv.ExpiresAt),
		}
	}
	response.OK(c, http.StatusOK, "Pending invitations listed", gin.H{"invitations": out})
}

func (h *Handler) RevokeInvitation(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.IsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can revoke an invitation")
		return
	}
	invitationID := c.Param("invitation_id")
	if invitationID == "" {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid invitation id")
		return
	}

	if err := h.invitations.Revoke(c.Request.Context(), user.ClerkOrgID, invitationID, user.ClerkUserID); err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not revoke invitation")
		return
	}
	response.OK(c, http.StatusOK, "Invitation revoked", gin.H{"id": invitationID})
}

func clerkMillisToRFC3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func clerkMillisPtrToRFC3339(ms *int64) *string {
	if ms == nil {
		return nil
	}
	s := clerkMillisToRFC3339(*ms)
	return &s
}

func parseUserID(c *gin.Context) (pgtype.UUID, bool) {
	parsed, err := uuid.Parse(c.Param("user_id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid member id")
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, true
}

func derefBool(b *bool) bool { return b != nil && *b }

func timestamptzPtr(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.Format(time.RFC3339)
	return &s
}
