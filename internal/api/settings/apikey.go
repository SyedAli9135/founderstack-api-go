// Package settings implements BYOK API key management
// (POST/GET/DELETE /api/v1/settings/api-key) — workflow 3. Every route
// here must sit behind middleware.RequireAuth.
package settings

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/llm"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
	"github.com/founderstack/api/internal/pkg/vault"
)

// kmsKeyID is stored alongside every encrypted key so a future migration
// to a real KMS can tell which rows still need re-encrypting. Not an
// actual KMS key reference today — matches the Python original's own
// hardcoded "local-fernet" placeholder, adapted for this envelope format.
const kmsKeyID = "local-aes-gcm"

// Handler implements the 3 BYOK settings endpoints.
type Handler struct {
	appPool             *pgxpool.Pool
	encryptionKey       []byte
	anthropicMockPrefix string
}

// NewHandler builds a Handler. appPool must be the app_user (RLS-enforced)
// pool — every operation here goes through tenant.WithTx, never a bare
// query. encryptionKey is the decoded ENCRYPTION_KEY (vault.DecodeKey),
// resolved once at startup so a misconfigured key fails the process at
// boot, not silently on a founder's first key submission.
func NewHandler(appPool *pgxpool.Pool, encryptionKey []byte, anthropicMockPrefix string) *Handler {
	return &Handler{appPool: appPool, encryptionKey: encryptionKey, anthropicMockPrefix: anthropicMockPrefix}
}

// Register mounts all 3 routes on rg. rg's group must already have
// middleware.RequireAuth applied — these handlers assume authctx is set.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/api-key", h.SubmitAPIKey)
	rg.GET("/api-key/status", h.APIKeyStatus)
	rg.DELETE("/api-key", h.DeleteAPIKey)
}

type submitAPIKeyRequest struct {
	APIKey string `json:"api_key" binding:"required"`
}

// invalidKeyMessage matches founderstack-api's exact wording — a wire
// contract the frontend's error display depends on, not just an internal
// implementation detail free to reword.
const invalidKeyMessage = "The provided Anthropic API key is invalid or inactive. Please ensure it starts with 'sk-ant-' and has active usage permissions."

// SubmitAPIKey validates, encrypts, and stores an Anthropic key for the
// caller's org — POST /api/v1/settings/api-key.
func (h *Handler) SubmitAPIKey(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var req submitAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, http.StatusBadRequest, "INVALID_REQUEST_BODY", "api_key is required")
		return
	}

	ctx := c.Request.Context()
	if err := llm.ValidateKey(ctx, req.APIKey, h.anthropicMockPrefix); err != nil {
		switch {
		case errors.Is(err, llm.ErrInvalidFormat), errors.Is(err, llm.ErrKeyRejected):
			response.Fail(c, http.StatusBadRequest, "INVALID_API_KEY", invalidKeyMessage)
		case errors.Is(err, llm.ErrValidationUnavailable):
			// Distinct from the two cases above and from the Python
			// original — see llm.ErrValidationUnavailable's doc comment
			// for why "Anthropic is unreachable" shouldn't be reported to
			// the founder as "your key is invalid."
			response.Fail(c, http.StatusServiceUnavailable, "VALIDATION_UNAVAILABLE",
				"Could not reach Anthropic to validate the key. Please try again shortly.")
		default:
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR",
				"The validation service encountered an error. Please try again later.")
		}
		return
	}

	encryptedKey, err := vault.Encrypt(req.APIKey, h.encryptionKey)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not securely store the key")
		return
	}
	keyPrefix := req.APIKey[:7] + "..." // safe: ValidateKey already confirmed a "sk-ant-" (7-byte) prefix

	err = tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		keyID, err := q.UpsertAnthropicKey(ctx, dbgen.UpsertAnthropicKeyParams{
			OrgID:        user.OrgID,
			KeyPrefix:    keyPrefix,
			EncryptedKey: encryptedKey,
			KmsKeyID:     kmsKeyID,
		})
		if err != nil {
			return err
		}
		return q.SetOrganizationActiveApiKey(ctx, dbgen.SetOrganizationActiveApiKeyParams{
			ID:             user.OrgID,
			ActiveApiKeyID: keyID,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not save the API key")
		return
	}

	response.OK(c, http.StatusCreated,
		"Anthropic API key has been securely encrypted and validated. Your workspace is now active.",
		gin.H{"provider": "anthropic", "key_prefix": keyPrefix},
	)
}

type apiKeyStatus struct {
	Provider   string     `json:"provider"`
	IsValid    bool       `json:"is_valid"`
	KeyPrefix  string     `json:"key_prefix"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// APIKeyStatus reports whether the org has an Anthropic key on file —
// GET /api/v1/settings/api-key/status. data is null (not a 404) when no
// key has ever been submitted, matching the Python original: "no key yet"
// is a normal state for a brand-new org, not an error.
func (h *Handler) APIKeyStatus(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	var status *apiKeyStatus
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetAnthropicKeyStatus(ctx, user.OrgID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		status = &apiKeyStatus{
			Provider:  row.Provider,
			IsValid:   row.IsValid != nil && *row.IsValid,
			KeyPrefix: row.KeyPrefix,
		}
		if row.UpdatedAt.Valid {
			status.UpdatedAt = &row.UpdatedAt.Time
		}
		if row.LastUsedAt.Valid {
			status.LastUsedAt = &row.LastUsedAt.Time
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch API key status")
		return
	}

	response.OK(c, http.StatusOK, "", status)
}

// DeleteAPIKey deactivates the org's Anthropic key —
// DELETE /api/v1/settings/api-key. Unconditional, like the Python
// original: no error if there was nothing to delete, idempotent either way.
func (h *Handler) DeleteAPIKey(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		if _, err := q.DeactivateAnthropicKey(ctx, user.OrgID); err != nil {
			return err
		}
		return q.ClearOrganizationActiveApiKey(ctx, user.OrgID)
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not remove the API key")
		return
	}

	c.Status(http.StatusNoContent)
}
