package auditlogs

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

type Handler struct {
	appPool *pgxpool.Pool
}

func NewHandler(appPool *pgxpool.Pool) *Handler {
	return &Handler{appPool: appPool}
}

// rg must already have middleware.RequireAuth applied.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/audit-logs", h.List)
}

type auditLogEntry struct {
	ID           string  `json:"id"`
	CreatedAt    string  `json:"created_at"`
	ActorType    string  `json:"actor_type"`
	ActorName    string  `json:"actor_name"`
	Action       string  `json:"action"`
	ResourceType *string `json:"resource_type,omitempty"`
	ResourceID   *string `json:"resource_id,omitempty"`
	Status       *string `json:"status,omitempty"`
}

type cursor struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

type auditLogsResponse struct {
	Entries    []auditLogEntry `json:"entries"`
	NextCursor *cursor         `json:"next_cursor,omitempty"`
}

// optString/optTimestamptz turn a blank query param into a real nil so
// the query's own `sqlc.narg(...) IS NULL OR ...` filters skip cleanly —
// an empty string is not the same thing as "no filter" to Postgres.
func optString(raw string) *string {
	if raw == "" {
		return nil
	}
	return &raw
}

func optTimestamptz(raw string) (pgtype.Timestamptz, error) {
	if raw == "" {
		return pgtype.Timestamptz{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return pgtype.Timestamptz{}, err
	}
	return pgtype.Timestamptz{Time: t, Valid: true}, nil
}

// List is guarded to owner/admin only — the plan's own acceptance
// criteria, since a member/viewer seeing every agent's tool-call history
// org-wide is a real information-disclosure boundary this app doesn't
// otherwise draw (documents' owner_only ACL is the closest precedent, but
// scoped to one resource type, not everything an org's agents ever did).
func (h *Handler) List(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.IsOwnerOrAdmin() {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin can view audit logs")
		return
	}

	limit := int32(defaultPageLimit)
	if raw := c.Query("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= maxPageLimit {
			limit = int32(v)
		}
	}

	dateFrom, err := optTimestamptz(c.Query("date_from"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "date_from must be RFC3339")
		return
	}
	dateTo, err := optTimestamptz(c.Query("date_to"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "date_to must be RFC3339")
		return
	}

	// Cursor fields travel together — a cursor_created_at with no
	// cursor_id (or vice versa) is a malformed request, not a partial
	// filter, since the composite (created_at, id) tuple comparison in
	// SQL needs both or neither.
	var cursorCreatedAt pgtype.Timestamptz
	var cursorID pgtype.UUID
	if rawCreatedAt, rawID := c.Query("cursor_created_at"), c.Query("cursor_id"); rawCreatedAt != "" || rawID != "" {
		cursorCreatedAt, err = optTimestamptz(rawCreatedAt)
		if err != nil || rawID == "" {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "cursor_created_at and cursor_id must both be present and valid")
			return
		}
		parsedID, err := uuid.Parse(rawID)
		if err != nil {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "cursor_id must be a valid UUID")
			return
		}
		cursorID = pgtype.UUID{Bytes: parsedID, Valid: true}
	}

	var rows []dbgen.ListAuditLogsPageRow
	err = tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListAuditLogsPage(ctx, dbgen.ListAuditLogsPageParams{
			OrgID: user.OrgID, ActorType: optString(c.Query("actor_type")), ActionPrefix: optString(c.Query("action")),
			Status: optString(c.Query("status")), DateFrom: dateFrom, DateTo: dateTo,
			CursorCreatedAt: cursorCreatedAt, CursorID: cursorID, PageLimit: limit,
		})
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch audit logs")
		return
	}

	entries := make([]auditLogEntry, 0, len(rows))
	for _, r := range rows {
		entry := auditLogEntry{
			ID: r.ID.String(), CreatedAt: r.CreatedAt.Time.Format(time.RFC3339),
			ActorType: r.ActorType, ActorName: r.ActorName, Action: r.Action,
			ResourceType: r.ResourceType, Status: r.Status,
		}
		if r.ResourceID.Valid {
			id := uuid.UUID(r.ResourceID.Bytes).String()
			entry.ResourceID = &id
		}
		entries = append(entries, entry)
	}

	// A returned page shorter than the requested limit means there's
	// nothing more — no next_cursor, so the frontend's "Load more" knows
	// to stop rather than firing one guaranteed-empty request to find out.
	var next *cursor
	if len(rows) == int(limit) {
		last := rows[len(rows)-1]
		next = &cursor{CreatedAt: last.CreatedAt.Time.Format(time.RFC3339), ID: last.ID.String()}
	}

	response.OK(c, http.StatusOK, "", auditLogsResponse{Entries: entries, NextCursor: next})
}
