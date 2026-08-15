package integrations

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/integrations"
)

// Handler implements integration-connection endpoints.
type Handler struct {
	appPool       *pgxpool.Pool
	encryptionKey []byte
	registry      *integrations.Registry
	stateManager  *integrations.StateManager
	frontendURL   string
}

// NewHandler builds a Handler. registry must already be wired with one
// provider per non-callback catalog entry —
// Register itself never imports internal/core/integrations/providers.
func NewHandler(appPool *pgxpool.Pool, encryptionKey []byte, registry *integrations.Registry, stateManager *integrations.StateManager, frontendURL string) *Handler {
	return &Handler{
		appPool:       appPool,
		encryptionKey: encryptionKey,
		registry:      registry,
		stateManager:  stateManager,
		frontendURL:   frontendURL,
	}
}

// Register mounts the 5 authenticated routes on rg. rg's group must
// already have middleware.RequireAuth applied.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("", h.ListIntegrations)
	rg.POST("/:service/connect", h.Connect)
	rg.POST("/:service/api-key", h.SubmitAPIKey)
	rg.DELETE("/:service", h.Disconnect)
	rg.GET("/:service/status", h.Status)
}

// RegisterCallback mounts the one unauthenticated route on rg — the
// OAuth provider redirects the founder's browser here directly, with no
// JWT attached, so org_id/service come from state.StateManager.Verify
// instead of authctx.
func (h *Handler) RegisterCallback(rg *gin.RouterGroup) {
	rg.GET("/:service/callback", h.Callback)
}

type integrationView struct {
	Service     string   `json:"service"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	AuthType    string   `json:"auth_type"`
	Status      string   `json:"status"`
	ConnectedAt *string  `json:"connected_at,omitempty"`
	Scopes      []string `json:"scopes,omitempty"`
}

// ListIntegrations merges the static catalog with the org's actual
// connections — GET /api/v1/integrations.
func (h *Handler) ListIntegrations(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	summaries, err := integrations.ListConnections(c.Request.Context(), h.appPool, user.OrgID)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch integrations")
		return
	}
	byService := make(map[string]integrations.ConnectionSummary, len(summaries))
	for _, s := range summaries {
		byService[s.ServiceName] = s
	}

	views := make([]integrationView, 0, len(integrations.Catalog))
	for service, meta := range integrations.Catalog {
		view := integrationView{
			Service:  service,
			Name:     meta.Name,
			Category: meta.Category,
			AuthType: string(meta.AuthType),
			Status:   "not_connected",
		}
		if conn, found := byService[service]; found && conn.IsActive {
			view.Status = conn.OAuthStatus
			view.Scopes = conn.Scopes
			if !conn.ConnectedAt.IsZero() {
				s := conn.ConnectedAt.Format(timeFormat)
				view.ConnectedAt = &s
			}
		}
		views = append(views, view)
	}

	response.OK(c, http.StatusOK, "", views)
}

const timeFormat = "2006-01-02T15:04:05Z07:00"

// Connect starts an OAuth flow — POST /api/v1/integrations/{service}/connect.
func (h *Handler) Connect(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	service := c.Param("service")
	if _, known := integrations.Catalog[service]; !known {
		response.Fail(c, http.StatusNotFound, "UNKNOWN_SERVICE", "No such integration: "+service)
		return
	}
	provider, ok := h.registry.Get(service)
	if !ok {
		response.Fail(c, http.StatusNotFound, "UNKNOWN_SERVICE", "No such integration: "+service)
		return
	}
	oauthProvider, ok := provider.(integrations.OAuthProvider)
	if !ok {
		response.Fail(c, http.StatusBadRequest, "NOT_OAUTH_SERVICE", service+" is connected via an API key, not OAuth — use POST .../api-key")
		return
	}

	state, err := h.stateManager.Generate(c.Request.Context(), user.OrgID.String(), service)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not start the connection")
		return
	}

	response.OK(c, http.StatusOK, "", gin.H{"redirect_url": oauthProvider.GetAuthURL(state)})
}

// Callback completes an OAuth flow — GET /api/v1/integrations/{service}/callback.
// No RequireAuth: the provider redirects the browser here directly with
// no JWT, so org_id/service are recovered from state, not authctx.
func (h *Handler) Callback(c *gin.Context) {
	service := c.Param("service")
	code := c.Query("code")
	state := c.Query("state")
	if providerErr := c.Query("error"); providerErr != "" {
		c.Redirect(http.StatusFound, h.frontendURL+"/integrations?error="+service)
		return
	}
	if code == "" || state == "" {
		response.Fail(c, http.StatusBadRequest, "MISSING_CALLBACK_PARAMS", "code and state are required")
		return
	}

	orgIDStr, verifiedService, err := h.stateManager.Verify(c.Request.Context(), state)
	if err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_STATE", "This connection attempt has expired or is invalid — please try connecting again")
		return
	}
	if verifiedService != service {
		response.Fail(c, http.StatusBadRequest, "SERVICE_MISMATCH", "Callback service does not match the authorization request")
		return
	}

	var orgID pgtype.UUID
	if err := orgID.Scan(orgIDStr); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_STATE", "This connection attempt has expired or is invalid — please try connecting again")
		return
	}

	provider, ok := h.registry.Get(service)
	if !ok {
		response.Fail(c, http.StatusNotFound, "UNKNOWN_SERVICE", "No such integration: "+service)
		return
	}
	oauthProvider, ok := provider.(integrations.OAuthProvider)
	if !ok {
		response.Fail(c, http.StatusBadRequest, "NOT_OAUTH_SERVICE", service+" is not an OAuth integration")
		return
	}

	ctx := c.Request.Context()
	tok, err := oauthProvider.ExchangeCode(ctx, code)
	if err != nil {
		slog.Error("integration oauth exchange failed", "service", service, "error", err)
		c.Redirect(http.StatusFound, h.frontendURL+"/integrations?error="+service)
		return
	}

	displayName := service
	if meta, known := integrations.Catalog[service]; known {
		displayName = meta.Name
	}
	if err := integrations.SaveConnection(ctx, h.appPool, h.encryptionKey, orgID, service, displayName, "oauth", "connected", *tok); err != nil {
		slog.Error("integration save connection failed", "service", service, "error", err)
		c.Redirect(http.StatusFound, h.frontendURL+"/integrations?error="+service)
		return
	}

	c.Redirect(http.StatusFound, h.frontendURL+"/integrations?connected="+service)
}

type submitAPIKeyRequest struct {
	Key string `json:"key" binding:"required"`
}

// SubmitAPIKey validates and stores a pasted key/PAT —
// POST /api/v1/integrations/{service}/api-key (Stripe, GitHub).
func (h *Handler) SubmitAPIKey(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	service := c.Param("service")
	var req submitAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "key is required")
		return
	}

	provider, ok := h.registry.Get(service)
	if !ok {
		response.Fail(c, http.StatusNotFound, "UNKNOWN_SERVICE", "No such integration: "+service)
		return
	}
	keyProvider, ok := provider.(integrations.KeyProvider)
	if !ok {
		response.Fail(c, http.StatusBadRequest, "NOT_KEY_SERVICE", service+" is connected via OAuth, not an API key — use POST .../connect")
		return
	}

	ctx := c.Request.Context()
	if err := keyProvider.ValidateKey(ctx, req.Key); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_KEY", "That key could not be validated: "+err.Error())
		return
	}

	displayName := service
	if meta, known := integrations.Catalog[service]; known {
		displayName = meta.Name
	}
	tok := integrations.Token{AccessToken: req.Key}
	if err := integrations.SaveConnection(ctx, h.appPool, h.encryptionKey, user.OrgID, service, displayName, "manual", "connected", tok); err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not save the key")
		return
	}

	response.OK(c, http.StatusCreated, "", gin.H{"status": "connected"})
}

// Disconnect revokes (best-effort) and deactivates a connection —
// DELETE /api/v1/integrations/{service}.
func (h *Handler) Disconnect(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	service := c.Param("service")
	ctx := c.Request.Context()

	conn, err := integrations.GetConnection(ctx, h.appPool, h.encryptionKey, user.OrgID, service)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch the connection")
		return
	}
	if conn != nil {
		if provider, ok := h.registry.Get(service); ok {
			if revocable, ok := provider.(integrations.Revocable); ok {
				if err := revocable.RevokeToken(ctx, conn.Token.AccessToken); err != nil {
					// Best-effort: the provider being unreachable or already
					// having invalidated the token shouldn't block the founder
					// from disconnecting on their end.
					slog.Warn("integration revoke failed, deactivating locally anyway", "service", service, "error", err)
				}
			}
		}
	}

	if err := integrations.RevokeConnection(ctx, h.appPool, user.OrgID, service); err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not disconnect")
		return
	}

	c.Status(http.StatusNoContent)
}

type statusResponse struct {
	Status string `json:"status"`
}

// Status re-validates a connection (refreshing if possible) —
// GET /api/v1/integrations/{service}/status.
func (h *Handler) Status(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	service := c.Param("service")
	ctx := c.Request.Context()

	conn, err := integrations.GetConnection(ctx, h.appPool, h.encryptionKey, user.OrgID, service)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.OK(c, http.StatusOK, "", statusResponse{Status: "not_connected"})
			return
		}
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch the connection")
		return
	}
	if !conn.IsActive {
		// Explicitly revoked (DELETE .../{service}) — report that as-is
		// rather than asking the provider to validate a token the founder
		// already disconnected. A provider whose ValidateToken doesn't
		// itself detect revocation (not guaranteed for every API) would
		// otherwise report "connected" again here.
		response.OK(c, http.StatusOK, "", statusResponse{Status: conn.OAuthStatus})
		return
	}

	provider, ok := h.registry.Get(service)
	if !ok {
		response.Fail(c, http.StatusNotFound, "UNKNOWN_SERVICE", "No such integration: "+service)
		return
	}
	validator, ok := provider.(integrations.TokenValidator)
	if !ok {
		// No cheap "is this still good" call for this provider — report
		// whatever mcp_connections already says rather than guessing.
		response.OK(c, http.StatusOK, "", statusResponse{Status: conn.OAuthStatus})
		return
	}

	if err := validator.ValidateToken(ctx, conn.Token.AccessToken); err == nil {
		response.OK(c, http.StatusOK, "", statusResponse{Status: "connected"})
		return
	}

	if refresher, ok := provider.(integrations.Refreshable); ok && conn.Token.RefreshToken != "" {
		newTok, err := refresher.RefreshAccessToken(ctx, conn.Token.RefreshToken)
		if err == nil {
			// A refresh response never re-sends provider-specific Extra
			// fields — preserve whatever the existing connection already had.
			if newTok.Extra == nil {
				newTok.Extra = conn.Token.Extra
			}
			if updateErr := integrations.UpdateTokens(ctx, h.appPool, h.encryptionKey, user.OrgID, service, *newTok); updateErr == nil {
				response.OK(c, http.StatusOK, "", statusResponse{Status: "connected"})
				return
			}
		}
	}

	if err := integrations.MarkExpired(ctx, h.appPool, user.OrgID, service); err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not update connection status")
		return
	}
	response.OK(c, http.StatusOK, "", statusResponse{Status: "expired"})
}
