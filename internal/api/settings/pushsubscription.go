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

// Mirrors the browser's PushSubscription.toJSON() shape — "p256dh"/"auth" are the Push
// API's own key names.
type submitPushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (h *Handler) SubmitPushSubscription(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	var req submitPushSubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Expected a browser PushSubscription (endpoint, keys.p256dh, keys.auth)")
		return
	}

	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.UpsertPushSubscription(ctx, dbgen.UpsertPushSubscriptionParams{
			OrgID: user.OrgID, UserID: user.ID, Endpoint: req.Endpoint,
			P256dhKey: req.Keys.P256dh, AuthKey: req.Keys.Auth,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not save push subscription")
		return
	}
	response.OK(c, http.StatusOK, "Push subscription saved", nil)
}

type deletePushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
}

func (h *Handler) DeletePushSubscription(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	var req deletePushSubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Endpoint == "" {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Expected {\"endpoint\": string}")
		return
	}

	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		return q.DeletePushSubscription(ctx, dbgen.DeletePushSubscriptionParams{
			OrgID: user.OrgID, UserID: user.ID, Endpoint: req.Endpoint,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not delete push subscription")
		return
	}
	response.OK(c, http.StatusOK, "Push subscription deleted", nil)
}
