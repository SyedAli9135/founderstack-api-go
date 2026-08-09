// Package identity holds small auth-adjacent utility endpoints — currently
// just the dev-token minter, mirroring founderstack-api's
// app/api/v1/endpoints/identity.py grouping under /api/v1/auth.
package identity

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

// DevTokenHandler mints local test tokens for manual (Postman etc.)
// testing — see internal/pkg/devtoken's doc comment for why this can't be
// the simple "mint an unsigned JWT" trick the Python original uses.
type DevTokenHandler struct {
	cfg *config.Config
}

func NewDevTokenHandler(cfg *config.Config) *DevTokenHandler {
	return &DevTokenHandler{cfg: cfg}
}

// Register mounts POST /dev-token on rg.
func (h *DevTokenHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/dev-token", h.Create)
}

type devTokenRequest struct {
	ClerkUserID string `json:"clerk_user_id" binding:"required"`
}

// Create mints a token for clerk_user_id. Responds 404 (not 403 —
// unauthenticated callers shouldn't learn this route exists at all) when
// disabled: production, or DEV_TOKEN_SECRET unset. No check that
// clerk_user_id refers to a real synced user — a mismatched one fails
// exactly like a real Clerk token would, at middleware.RequireAuth's
// USER_NOT_SYNCHRONIZED step, not here.
func (h *DevTokenHandler) Create(c *gin.Context) {
	if h.cfg.IsProduction() || h.cfg.DevTokenSecret.IsEmpty() {
		response.Fail(c, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}

	var req devTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "clerk_user_id is required")
		return
	}

	token, err := devtoken.Sign(h.cfg.DevTokenSecret.Expose(), req.ClerkUserID)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not mint dev token")
		return
	}

	response.OK(c, http.StatusOK, "Dev token minted — for local testing only, never valid in production.",
		gin.H{"token": token},
	)
}
