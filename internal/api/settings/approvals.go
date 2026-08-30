package settings

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// approvalsSettings is the org-level Slack notification config
type approvalsSettings struct {
	SlackChannelID *string `json:"slack_channel_id"`
}

func (h *Handler) GetApprovalsSettings(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var channel *string
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		channel, err = q.GetOrgApprovalsSlackChannel(ctx, user.OrgID)
		return err
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch approval settings")
		return
	}
	response.OK(c, http.StatusOK, "Approval settings fetched", approvalsSettings{SlackChannelID: channel})
}

type updateApprovalsSettingsRequest struct {
	SlackChannelID string `json:"slack_channel_id"`
}

func (h *Handler) UpdateApprovalsSettings(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	var req updateApprovalsSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Expected {\"slack_channel_id\": string}")
		return
	}

	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.UpdateOrgApprovalsSlackChannel(ctx, dbgen.UpdateOrgApprovalsSlackChannelParams{
			ID: user.OrgID, ApprovalsSlackChannelID: &req.SlackChannelID,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not update approval settings")
		return
	}
	response.OK(c, http.StatusOK, "Approval settings updated", approvalsSettings{SlackChannelID: &req.SlackChannelID})
}
