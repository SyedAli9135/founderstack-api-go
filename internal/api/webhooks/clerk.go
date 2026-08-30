package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/pkg/svix"
)

// ClerkHandler syncs Clerk organization/user events into the local
// database. It runs against the app_system (BYPASSRLS) pool, never
// app_user — an organization.created event is, by definition, creating a
// tenant the RLS-scoped pool has no session context for yet.
type ClerkHandler struct {
	db     *dbgen.Queries
	secret string
}

// NewClerkHandler builds a ClerkHandler. pool must be the app_system pool;
// webhookSecret is CLERK_WEBHOOK_SECRET ("whsec_...").
func NewClerkHandler(pool *pgxpool.Pool, webhookSecret string) *ClerkHandler {
	return &ClerkHandler{db: dbgen.New(pool), secret: webhookSecret}
}

// Register mounts POST /clerk on rg.
func (h *ClerkHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/clerk", h.Handle)
}

// handlerTimeout bounds how long processing a single webhook delivery may
// take — DB writes here are simple upserts, so this is generous headroom,
// not a tight budget.
const handlerTimeout = 10 * time.Second

// envelope is Clerk's outer webhook shape: {"type": "...", "data": {...}}.
type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (h *ClerkHandler) Handle(c *gin.Context) {
	if h.secret == "" {
		slog.Error("CLERK_WEBHOOK_SECRET is not set")
		response.Fail(c, http.StatusInternalServerError, "WEBHOOK_MISCONFIGURED",
			"The authentication service is misconfigured. Please contact support.")
		return
	}

	body, err := c.GetRawData()
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_BODY", "Could not read request body")
		return
	}

	err = svix.Verify(h.secret, c.GetHeader("svix-id"), c.GetHeader("svix-timestamp"), c.GetHeader("svix-signature"), body)
	switch {
	case errors.Is(err, svix.ErrMissingHeaders):
		response.Fail(c, http.StatusBadRequest, "MISSING_SVIX_HEADERS", "Missing svix headers")
		return
	case err != nil:
		slog.Warn("clerk webhook signature verification failed", "error", err, "request_id", response.RequestID(c))
		response.Fail(c, http.StatusBadRequest, "INVALID_SIGNATURE", "Invalid signature")
		return
	}

	var evt envelope
	if err := json.Unmarshal(body, &evt); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_PAYLOAD", "Could not parse webhook payload")
		return
	}

	slog.Info("received clerk webhook", "type", evt.Type, "request_id", response.RequestID(c))

	// Bounds how long a single webhook delivery can hold open a DB
	// connection — without this, a hung query blocks indefinitely since
	// Gin sets no request deadline of its own.
	ctx, cancel := context.WithTimeout(c.Request.Context(), handlerTimeout)
	defer cancel()

	var handleErr error
	switch evt.Type {
	case "organization.created", "organization.updated":
		handleErr = h.upsertOrganization(ctx, evt.Data)
	case "organizationMembership.created", "organizationMembership.updated":
		handleErr = h.upsertMembership(ctx, evt.Data)
	case "user.updated":
		handleErr = h.updateUserProfile(ctx, evt.Data)
	case "organization.deleted":
		handleErr = h.softDeleteOrganization(ctx, evt.Data)
	case "organizationMembership.deleted":
		handleErr = h.softDeleteMembership(ctx, evt.Data)
	case "user.deleted":
		handleErr = h.softDeleteUser(ctx, evt.Data)
	default:
		// Deliberately unhandled, not just "not implemented yet":
		//   session.*             — sessions table has no reader; would be
		//                           write-only data nothing consumes.
		//   organizationInvitation.* — no invitations table, and an accepted
		//                           invite already fires
		//                           organizationMembership.created, which we
		//                           do handle — nothing left to add.
		//   user.created          — fires before any org membership exists;
		//                           users.org_id is NOT NULL, so there's no
		//                           valid row to create yet.
		// Ack every other type so Clerk doesn't retry something we'll never act on.
		slog.Debug("ignoring unhandled clerk webhook type", "type", evt.Type)
	}

	var notFound *orgNotFoundYetError
	switch {
	case errors.As(handleErr, &notFound):
		// 422 tells Clerk/Svix to retry with backoff — self-heals the rare
		// case where a membership event arrives before its org's event.
		response.Fail(c, http.StatusUnprocessableEntity, "ORG_NOT_FOUND_YET", notFound.Error())
	case handleErr != nil:
		slog.Error("clerk webhook processing failed", "type", evt.Type, "error", handleErr, "request_id", response.RequestID(c))
		response.Fail(c, http.StatusInternalServerError, "WEBHOOK_PROCESSING_FAILED",
			fmt.Sprintf("An unexpected error occurred while processing the %s event.", evt.Type))
	default:
		response.OK(c, http.StatusOK, "Webhook processed successfully.", nil)
	}
}

type orgNotFoundYetError struct{ clerkOrgID string }

func (e *orgNotFoundYetError) Error() string {
	return fmt.Sprintf("organization %s not found yet", e.clerkOrgID)
}

type organizationPayload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// upsertOrganization backs organization.created and organization.updated —
// both are the same idempotent write (INSERT ... ON CONFLICT DO UPDATE on
// clerk_org_id), so a redelivered or out-of-order event is harmless.
func (h *ClerkHandler) upsertOrganization(ctx context.Context, raw json.RawMessage) error {
	var data organizationPayload
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode organization payload: %w", err)
	}
	if _, err := h.db.UpsertOrganization(ctx, dbgen.UpsertOrganizationParams{
		ClerkOrgID: data.ID,
		Name:       data.Name,
		Slug:       data.Slug,
	}); err != nil {
		return fmt.Errorf("upsert organization: %w", err)
	}
	return nil
}

type membershipPayload struct {
	Organization   organizationPayload `json:"organization"`
	PublicUserData struct {
		UserID     string `json:"user_id"`
		Identifier string `json:"identifier"`
		FirstName  string `json:"first_name"`
		LastName   string `json:"last_name"`
	} `json:"public_user_data"`
	Role string `json:"role"`
}

// upsertMembership backs organizationMembership.created and .updated.
func (h *ClerkHandler) upsertMembership(ctx context.Context, raw json.RawMessage) error {
	var data membershipPayload
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode membership payload: %w", err)
	}

	orgID, err := h.db.GetOrganizationIDByClerkOrgID(ctx, data.Organization.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &orgNotFoundYetError{clerkOrgID: data.Organization.ID}
		}
		return fmt.Errorf("look up organization %s: %w", data.Organization.ID, err)
	}

	fullName := nilIfEmpty(strings.TrimSpace(data.PublicUserData.FirstName + " " + data.PublicUserData.LastName))
	role := normalizeRole(data.Role)
	canApprove := canApproveByDefault(role)
	if err := h.db.UpsertUserForMembership(ctx, dbgen.UpsertUserForMembershipParams{
		OrgID:               orgID,
		ClerkUserID:         data.PublicUserData.UserID,
		Email:               data.PublicUserData.Identifier,
		FullName:            fullName,
		Role:                role,
		CanApproveWorkflows: &canApprove,
	}); err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}
	return nil
}

// canApproveByDefault decides a brand-new user's initial
// can_approve_workflows value (see UpsertUserForMembership's doc comment
// for why this only matters on first insert). Clerk's own default role
// for whoever creates an organization is "admin" — treating admin/owner
// as pre-authorized to approve their own agents' actions is what makes
// Workflow 10 usable at all before workflow 13 (team management) exists
// to grant this explicitly.
func canApproveByDefault(role string) bool {
	return role == "admin" || role == "owner"
}

type userPayload struct {
	ID        string `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	ImageURL  string `json:"image_url"`
}

// updateUserProfile backs user.updated. A user not yet synced (0 rows
// affected) is not an error — matches the Python backend's silent no-op,
// since the membership webhook that will create them may simply not have
// arrived yet.
func (h *ClerkHandler) updateUserProfile(ctx context.Context, raw json.RawMessage) error {
	var data userPayload
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode user payload: %w", err)
	}
	_, err := h.db.UpdateUserProfile(ctx, dbgen.UpdateUserProfileParams{
		ClerkUserID: data.ID,
		FullName:    nilIfEmpty(strings.TrimSpace(data.FirstName + " " + data.LastName)),
		AvatarUrl:   nilIfEmpty(data.ImageURL),
	})
	if err != nil {
		return fmt.Errorf("update user profile: %w", err)
	}
	return nil
}

// softDeleteOrganization backs organization.deleted. Deliberately a
// soft-delete (is_active=false), not the Python backend's hard DELETE:
// (a) it matches this codebase's own convention for every other
// delete-ish endpoint (agents, documents), (b) workflow_runs and approvals
// reference organizations.id without ON DELETE CASCADE, so a hard delete
// would fail outright once an org has any run history, and (c) it
// preserves audit_logs/cost_ledger history for a deleted tenant instead of
// destroying it.
func (h *ClerkHandler) softDeleteOrganization(ctx context.Context, raw json.RawMessage) error {
	var data organizationPayload
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode organization payload: %w", err)
	}
	if _, err := h.db.SoftDeleteOrganizationByClerkOrgID(ctx, data.ID); err != nil {
		return fmt.Errorf("soft-delete organization: %w", err)
	}
	return nil
}

// softDeleteMembership backs organizationMembership.deleted — a member was
// removed from an org (but their Clerk account still exists elsewhere).
// Since a users row is scoped to exactly one org_id in this schema,
// removing that membership means this backend should treat them as
// deactivated, not partially-linked to an org they no longer belong to. No
// cascade beyond the users row itself, same as organization.deleted.
func (h *ClerkHandler) softDeleteMembership(ctx context.Context, raw json.RawMessage) error {
	var data membershipPayload
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode membership payload: %w", err)
	}
	if _, err := h.db.SoftDeleteUserByClerkUserID(ctx, data.PublicUserData.UserID); err != nil {
		return fmt.Errorf("soft-delete user: %w", err)
	}
	return nil
}

// softDeleteUser backs user.deleted — the Clerk account itself was deleted,
// not just removed from one org. Same soft-delete as softDeleteMembership;
// kept as a separate function because the payload shape differs (a bare
// {"id": ...}, not nested under public_user_data) even though the DB write
// is identical.
func (h *ClerkHandler) softDeleteUser(ctx context.Context, raw json.RawMessage) error {
	var data userPayload
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode user payload: %w", err)
	}
	if _, err := h.db.SoftDeleteUserByClerkUserID(ctx, data.ID); err != nil {
		return fmt.Errorf("soft-delete user: %w", err)
	}
	return nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// normalizeRole strips Clerk's namespace prefix from a role string, e.g.
// "org:admin" -> "admin". A role with no colon (or an empty string) is
// returned unchanged.
func normalizeRole(role string) string {
	if i := strings.LastIndex(role, ":"); i != -1 {
		return role[i+1:]
	}
	return role
}
