package settings

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/digest"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

type digestSettings struct {
	DigestEnabled  bool   `json:"digest_enabled"`
	DigestSendHour int    `json:"digest_send_hour"`
	DigestTimezone string `json:"digest_timezone"`
}

func (h *Handler) GetDigestSettings(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var settings digestSettings
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetDigestSettings(ctx, user.OrgID)
		if err != nil {
			return err
		}
		settings = digestSettings{
			DigestEnabled: row.DigestEnabled, DigestSendHour: int(row.DigestSendHour), DigestTimezone: row.DigestTimezone,
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch digest settings")
		return
	}
	response.OK(c, http.StatusOK, "", settings)
}

type updateDigestSettingsRequest struct {
	DigestEnabled  bool   `json:"digest_enabled"`
	DigestSendHour int    `json:"digest_send_hour" binding:"min=0,max=23"`
	DigestTimezone string `json:"digest_timezone" binding:"required"`
}

func (h *Handler) UpdateDigestSettings(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var req updateDigestSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY",
			"Expected {\"digest_enabled\": bool, \"digest_send_hour\": 0-23, \"digest_timezone\": string}")
		return
	}
	// Validated here, not left to the SQL side's `AT TIME ZONE` -- an
	// unknown zone there throws at send/query time instead of at the
	// moment a founder typed it in.
	if _, err := time.LoadLocation(req.DigestTimezone); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_TIMEZONE", "Unrecognized IANA timezone: "+req.DigestTimezone)
		return
	}

	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.UpdateDigestSettings(ctx, dbgen.UpdateDigestSettingsParams{
			ID: user.OrgID, DigestEnabled: req.DigestEnabled, DigestSendHour: int32(req.DigestSendHour), DigestTimezone: req.DigestTimezone,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not update digest settings")
		return
	}
	response.OK(c, http.StatusOK, "Digest settings updated", digestSettings{
		DigestEnabled: req.DigestEnabled, DigestSendHour: req.DigestSendHour, DigestTimezone: req.DigestTimezone,
	})
}

// SendTestDigest builds yesterday's real digest for the caller's org and
// sends it to the caller's own email only -- not every admin, so testing
// never spams teammates.
func (h *Handler) SendTestDigest(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var email string
	var subject, textBody, htmlBody string
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		org, err := q.GetOrgNameAndTimezone(ctx, user.OrgID)
		if err != nil {
			return err
		}
		email, err = q.GetUserEmailByID(ctx, user.ID)
		if err != nil {
			return err
		}
		payload, err := digest.BuildPayload(ctx, q, user.OrgID, org.Name, org.DigestTimezone)
		if err != nil {
			return err
		}
		unsubscribeURL := h.unsubscribeURL(user.OrgID)
		subject, textBody, htmlBody, err = digest.RenderEmail(payload, unsubscribeURL)
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not build digest")
		return
	}

	if err := h.email.Send(c.Request.Context(), email, subject, textBody, htmlBody); err != nil {
		response.Fail(c, http.StatusServiceUnavailable, "EMAIL_SEND_FAILED", "Could not send the test email — check your email provider config")
		return
	}
	response.OK(c, http.StatusOK, "Test digest sent", gin.H{"status": "sent"})
}

func (h *Handler) unsubscribeURL(orgID pgtype.UUID) string {
	if h.digestTokens == nil {
		return ""
	}
	token := h.digestTokens.Sign(uuid.UUID(orgID.Bytes))
	if token == "" {
		return ""
	}
	return fmt.Sprintf("%s/api/v1/settings/digest/unsubscribe?token=%s", h.appBaseURL, token)
}

// Unsubscribe requires no login by design -- a founder clicking a footer
// link from their inbox has no live Clerk session. The token alone
// proves which org to disable; it can only ever turn the digest off, see
// DisableDigestForOrg's own doc comment.
func (h *Handler) Unsubscribe(c *gin.Context) {
	token := c.Query("token")
	orgID, err := h.digestTokens.Verify(token)
	if err != nil {
		c.Data(http.StatusBadRequest, "text/html; charset=utf-8", []byte(unsubscribeInvalidPage))
		return
	}

	orgPgUUID := pgtype.UUID{Bytes: orgID, Valid: true}
	err = tenant.WithTx(c.Request.Context(), h.appPool, orgPgUUID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.DisableDigestForOrg(ctx, orgPgUUID)
	})
	if err != nil {
		c.Data(http.StatusInternalServerError, "text/html; charset=utf-8", []byte(unsubscribeInvalidPage))
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(unsubscribeOKPage))
}

const unsubscribeOKPage = `<!doctype html><html><head><meta charset="utf-8"><title>Unsubscribed</title></head>
<body style="font-family:sans-serif;text-align:center;padding:64px 16px;color:#111827;">
<h1>You're unsubscribed</h1><p>Daily digest emails have been turned off for your organization. You can re-enable them any time from Settings → Notifications.</p>
</body></html>`

const unsubscribeInvalidPage = `<!doctype html><html><head><meta charset="utf-8"><title>Link expired</title></head>
<body style="font-family:sans-serif;text-align:center;padding:64px 16px;color:#111827;">
<h1>This link isn't valid</h1><p>Please unsubscribe from Settings → Notifications instead.</p>
</body></html>`
