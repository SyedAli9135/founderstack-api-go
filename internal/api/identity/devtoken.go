// Package identity holds small auth-adjacent utility endpoints (currently
// just the dev-token minter).
package identity

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/devtoken"
)

// DevTokenHandler mints local test tokens for manual testing.
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

// Create mints a token for clerk_user_id. Returns 404, not 403, when
// disabled (production or DEV_TOKEN_SECRET unset) so the route's existence
// isn't disclosed. Doesn't validate clerk_user_id — a bad one fails later
// at RequireAuth like a real Clerk token would.
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
