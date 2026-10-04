package reports

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	"github.com/founderstack/api/internal/db/tenant"
)

const (
	defaultExpiryDays = 30
	maxExpiryDays     = 365
	maxWindowDays     = 366
	// Live (unrevoked, unexpired) share links per workspace.
	maxLiveReportsPerOrg = 500
)

type Handler struct {
	appPool    *pgxpool.Pool
	systemPool *pgxpool.Pool
	limiter    RateLimiter
}

// appPool (app_user) aggregates and writes a report inside the reported
// workspace's own tenant transaction; systemPool (app_system) serves the
// cross-workspace operator list and the public token lookup, each pinned to
// the caller's owner/admin memberships or the token itself.
func NewHandler(appPool, systemPool *pgxpool.Pool, limiter RateLimiter) *Handler {
	return &Handler{appPool: appPool, systemPool: systemPool, limiter: limiter}
}

// Register mounts the operator routes; rg must have RequireAuth.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/reports", h.Create)
	rg.GET("/reports", h.List)
	rg.GET("/reports/:id", h.Get)
	rg.DELETE("/reports/:id", h.Revoke)
}

// RegisterPublic mounts the no-auth share-link route.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/reports/:token", h.GetPublic)
}

type reportView struct {
	ID              string          `json:"id"`
	OrgID           string          `json:"org_id"`
	OrgName         string          `json:"org_name"`
	Title           string          `json:"title"`
	DateFrom        string          `json:"date_from"`
	DateTo          string          `json:"date_to"`
	VisibleSections Sections        `json:"visible_sections"`
	ShareToken      string          `json:"share_token"`
	SharePath       string          `json:"share_path"`
	ExpiresAt       string          `json:"expires_at"`
	Status          string          `json:"status"`
	ViewCount       int32           `json:"view_count"`
	LastViewedAt    *string         `json:"last_viewed_at"`
	CreatedAt       string          `json:"created_at"`
	Snapshot        json.RawMessage `json:"snapshot,omitempty"`
}

// status: "revoked" wins over "expired" — the operator's explicit action is
// the more useful thing to show them.
func status(revoked bool, expiresAt time.Time, now time.Time) string {
	switch {
	case revoked:
		return "revoked"
	case !now.Before(expiresAt):
		return "expired"
	default:
		return "active"
	}
}

func dateStr(d pgtype.Date) string { return d.Time.Format("2006-01-02") }

func timePtr(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.UTC().Format(time.RFC3339)
	return &s
}

// newShareToken is 24 bytes from crypto/rand — 192 bits, far beyond
// brute-forcing even without the rate limit — URL-safe encoded (32 chars).
func newShareToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// adminMembership returns the caller's users row in orgID if they're an
// active owner/admin there.
func (h *Handler) adminMembership(ctx context.Context, orgID pgtype.UUID, clerkUserID string) (pgtype.UUID, bool, error) {
	m, err := dbgen.New(h.systemPool).GetActiveUserInOrg(ctx, dbgen.GetActiveUserInOrgParams{OrgID: orgID, ClerkUserID: clerkUserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, nil
	}
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	return m.ID, m.Role == "owner" || m.Role == "admin", nil
}

type createRequest struct {
	OrgID           string   `json:"org_id"`
	Title           string   `json:"title"`
	DateFrom        string   `json:"date_from"`
	DateTo          string   `json:"date_to"`
	VisibleSections Sections `json:"visible_sections"`
	ExpiresInDays   *int     `json:"expires_in_days"`
}

// Create generates the report snapshot and its share link. org_id is any
// workspace the caller administers — a practice's client workspace or their
// own org; omitted, it's the caller's current workspace.
func (h *Handler) Create(c *gin.Context) {
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
	orgID := user.OrgID
	if req.OrgID != "" {
		parsed, err := uuid.Parse(req.OrgID)
		if err != nil {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid org_id")
			return
		}
		orgID = pgtype.UUID{Bytes: parsed, Valid: true}
	}
	ctx := c.Request.Context()

	memberID, isAdmin, err := h.adminMembership(ctx, orgID, user.ClerkUserID)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not verify access")
		return
	}
	if !isAdmin {
		response.Fail(c, http.StatusForbidden, "NOT_AUTHORIZED", "Only an owner or admin of that workspace can create its reports")
		return
	}
	org, err := dbgen.New(h.systemPool).GetReportOrgInfo(ctx, orgID)
	if err != nil {
		response.Fail(c, http.StatusNotFound, "WORKSPACE_NOT_FOUND", "Workspace not found")
		return
	}
	loc, err := time.LoadLocation(org.DigestTimezone)
	if err != nil {
		loc = time.UTC
	}

	from, errFrom := time.ParseInLocation("2006-01-02", req.DateFrom, loc)
	to, errTo := time.ParseInLocation("2006-01-02", req.DateTo, loc)
	today := time.Now().In(loc)
	switch {
	case errFrom != nil || errTo != nil:
		response.Fail(c, http.StatusBadRequest, "INVALID_DATE_RANGE", "date_from and date_to must be YYYY-MM-DD")
		return
	case to.Before(from):
		response.Fail(c, http.StatusBadRequest, "INVALID_DATE_RANGE", "date_to can't be before date_from")
		return
	case to.Sub(from) >= maxWindowDays*24*time.Hour:
		response.Fail(c, http.StatusBadRequest, "INVALID_DATE_RANGE", fmt.Sprintf("A report can cover at most %d days", maxWindowDays))
		return
	case from.After(today):
		response.Fail(c, http.StatusBadRequest, "INVALID_DATE_RANGE", "date_from can't be in the future")
		return
	}
	expiryDays := defaultExpiryDays
	if req.ExpiresInDays != nil {
		if *req.ExpiresInDays < 1 || *req.ExpiresInDays > maxExpiryDays {
			response.Fail(c, http.StatusBadRequest, "INVALID_EXPIRY", fmt.Sprintf("expires_in_days must be between 1 and %d", maxExpiryDays))
			return
		}
		expiryDays = *req.ExpiresInDays
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = fmt.Sprintf("%s — automation report, %s – %s", org.Name, from.Format("Jan 2"), to.Format("Jan 2, 2006"))
	}
	if len(title) > 255 {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "title is at most 255 characters")
		return
	}
	token, err := newShareToken()
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not create share link")
		return
	}
	sectionsJSON, _ := json.Marshal(req.VisibleSections)
	expiresAt := time.Now().Add(time.Duration(expiryDays) * 24 * time.Hour)

	var snapJSON []byte
	var inserted dbgen.InsertClientReportRow
	var limitReached bool
	err = tenant.WithTx(ctx, h.appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		live, err := q.CountClientReportsForOrg(ctx, orgID)
		if err != nil {
			return err
		}
		if live >= maxLiveReportsPerOrg {
			limitReached = true
			return nil
		}
		snap, err := buildSnapshot(ctx, q, orgID, org.Name, org.PreparedBy, from, to, loc, req.VisibleSections)
		if err != nil {
			return err
		}
		if snapJSON, err = json.Marshal(snap); err != nil {
			return err
		}
		inserted, err = q.InsertClientReport(ctx, dbgen.InsertClientReportParams{
			OrgID: orgID, CreatedByUserID: memberID, Title: title,
			DateFrom: pgtype.Date{Time: from, Valid: true}, DateTo: pgtype.Date{Time: to, Valid: true},
			Timezone: loc.String(), VisibleSections: sectionsJSON, Snapshot: snapJSON,
			ShareToken: token, ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
		})
		if err != nil {
			return err
		}
		return h.audit(ctx, q, orgID, memberID, "report.created", inserted.ID, map[string]any{
			"title": title, "date_from": req.DateFrom, "date_to": req.DateTo, "visible_sections": req.VisibleSections,
		})
	})
	if err != nil {
		slog.Error("reports: create failed", "error", err, "request_id", response.RequestID(c))
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not generate report")
		return
	}

	if limitReached {
		response.Fail(c, http.StatusBadRequest, "REPORT_LIMIT_REACHED", "This workspace has 500 live report links — revoke some before creating more")
		return
	}

	response.OK(c, http.StatusCreated, "Report generated", reportView{
		ID: inserted.ID.String(), OrgID: orgID.String(), OrgName: org.Name, Title: title,
		DateFrom: req.DateFrom, DateTo: req.DateTo, VisibleSections: req.VisibleSections,
		ShareToken: token, SharePath: "/reports/" + token, ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		Status: "active", CreatedAt: inserted.CreatedAt.Time.UTC().Format(time.RFC3339), Snapshot: snapJSON,
	})
}

func (h *Handler) audit(ctx context.Context, q *dbgen.Queries, orgID, actorID pgtype.UUID, action string, reportID pgtype.UUID, meta map[string]any) error {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	resourceType, st := "client_report", "success"
	return q.InsertAuditLog(ctx, dbgen.InsertAuditLogParams{
		OrgID: orgID, ActorID: actorID, ActorType: "user", Action: action,
		ResourceType: &resourceType, ResourceID: reportID, Status: &st, MetadataInfo: metaJSON,
	})
}

// List returns reports for every workspace the caller administers, or just
// one with ?org_id=.
func (h *Handler) List(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	params := dbgen.ListClientReportsForUserParams{ClerkUserID: user.ClerkUserID}
	if raw := c.Query("org_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid org_id")
			return
		}
		params.TargetOrgID = pgtype.UUID{Bytes: parsed, Valid: true}
	}
	rows, err := dbgen.New(h.systemPool).ListClientReportsForUser(c.Request.Context(), params)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not list reports")
		return
	}
	now := time.Now()
	out := make([]reportView, 0, len(rows))
	for _, r := range rows {
		var sections Sections
		_ = json.Unmarshal(r.VisibleSections, &sections)
		out = append(out, reportView{
			ID: r.ID.String(), OrgID: r.OrgID.String(), OrgName: r.OrgName, Title: r.Title,
			DateFrom: dateStr(r.DateFrom), DateTo: dateStr(r.DateTo), VisibleSections: sections,
			ShareToken: r.ShareToken, SharePath: "/reports/" + r.ShareToken,
			ExpiresAt: r.ExpiresAt.Time.UTC().Format(time.RFC3339), Status: status(r.IsRevoked, r.ExpiresAt.Time, now),
			ViewCount: r.ViewCount, LastViewedAt: timePtr(r.LastViewedAt), CreatedAt: r.CreatedAt.Time.UTC().Format(time.RFC3339),
		})
	}
	response.OK(c, http.StatusOK, "Reports listed", gin.H{"reports": out})
}

func (h *Handler) loadForUser(c *gin.Context) (dbgen.GetClientReportForUserRow, bool) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return dbgen.GetClientReportForUserRow{}, false
	}
	parsed, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid report id")
		return dbgen.GetClientReportForUserRow{}, false
	}
	r, err := dbgen.New(h.systemPool).GetClientReportForUser(c.Request.Context(), dbgen.GetClientReportForUserParams{
		ID: pgtype.UUID{Bytes: parsed, Valid: true}, ClerkUserID: user.ClerkUserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.Fail(c, http.StatusNotFound, "REPORT_NOT_FOUND", "Report not found")
		} else {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not load report")
		}
		return r, false
	}
	return r, true
}

// Get is the operator's own preview — it never counts as a view.
func (h *Handler) Get(c *gin.Context) {
	r, ok := h.loadForUser(c)
	if !ok {
		return
	}
	var sections Sections
	_ = json.Unmarshal(r.VisibleSections, &sections)
	response.OK(c, http.StatusOK, "Report fetched", reportView{
		ID: r.ID.String(), OrgID: r.OrgID.String(), OrgName: r.OrgName, Title: r.Title,
		DateFrom: dateStr(r.DateFrom), DateTo: dateStr(r.DateTo), VisibleSections: sections,
		ShareToken: r.ShareToken, SharePath: "/reports/" + r.ShareToken,
		ExpiresAt: r.ExpiresAt.Time.UTC().Format(time.RFC3339), Status: status(r.IsRevoked, r.ExpiresAt.Time, time.Now()),
		ViewCount: r.ViewCount, LastViewedAt: timePtr(r.LastViewedAt),
		CreatedAt: r.CreatedAt.Time.UTC().Format(time.RFC3339), Snapshot: r.Snapshot,
	})
}

// Revoke takes effect on the very next public request — the public handler
// reads is_revoked on every view, nothing is cached.
func (h *Handler) Revoke(c *gin.Context) {
	r, ok := h.loadForUser(c)
	if !ok {
		return
	}
	user, _ := authctx.FromContext(c)
	ctx := c.Request.Context()
	memberID, _, err := h.adminMembership(ctx, r.OrgID, user.ClerkUserID)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not verify access")
		return
	}
	err = tenant.WithTx(ctx, h.appPool, r.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		n, err := q.RevokeClientReport(ctx, dbgen.RevokeClientReportParams{ID: r.ID, OrgID: r.OrgID})
		if err != nil || n == 0 {
			return err
		}
		return h.audit(ctx, q, r.OrgID, memberID, "report.revoked", r.ID, map[string]any{"title": r.Title})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not revoke report")
		return
	}
	response.OK(c, http.StatusOK, "Report revoked", gin.H{"id": r.ID.String(), "status": "revoked"})
}

// publicReport deliberately omits visible_sections: telling the client
// "cost: false" would reveal there's a cost view being withheld. A section is
// shown iff it's present in the snapshot.
type publicReport struct {
	Title     string          `json:"title"`
	DateFrom  string          `json:"date_from"`
	DateTo    string          `json:"date_to"`
	Timezone  string          `json:"timezone"`
	ExpiresAt string          `json:"expires_at"`
	Snapshot  json.RawMessage `json:"snapshot"`
}

// GetPublic serves a share link with no auth. Missing, revoked, expired and
// deactivated-workspace all get the identical response, so a caller can't
// tell a guessed token from one that once existed.
func (h *Handler) GetPublic(c *gin.Context) {
	ctx := c.Request.Context()
	if !h.limiter.Allow(ctx, "public-report:"+c.ClientIP()) {
		response.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests — try again in a minute")
		return
	}
	notAvailable := func() {
		response.Fail(c, http.StatusNotFound, "REPORT_NOT_AVAILABLE", "This report is no longer available")
	}
	token := c.Param("token")
	if len(token) < 20 || len(token) > 64 {
		notAvailable()
		return
	}
	q := dbgen.New(h.systemPool)
	r, err := q.GetClientReportByToken(ctx, token)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("reports: public lookup failed", "error", err)
		}
		notAvailable()
		return
	}
	if r.IsRevoked || !time.Now().Before(r.ExpiresAt.Time) || r.OrgActive == nil || !*r.OrgActive {
		notAvailable()
		return
	}
	if err := q.RecordClientReportView(ctx, r.ID); err != nil {
		slog.Warn("reports: record view failed", "error", err)
	}
	// Reports are personal to one client — keep shared caches (proxies, CDNs)
	// from storing them, and search engines from indexing them.
	c.Header("Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	response.OK(c, http.StatusOK, "", publicReport{
		Title: r.Title, DateFrom: dateStr(r.DateFrom), DateTo: dateStr(r.DateTo), Timezone: r.Timezone,
		ExpiresAt: r.ExpiresAt.Time.UTC().Format(time.RFC3339), Snapshot: r.Snapshot,
	})
}
