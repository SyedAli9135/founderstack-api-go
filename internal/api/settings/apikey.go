package settings

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
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

// Handler implements the 4 BYOK settings endpoints.
type Handler struct {
	appPool          *pgxpool.Pool
	encryptionKey    []byte
	apiKeyMockPrefix string
}

// NewHandler builds a Handler. appPool must be the app_user (RLS-enforced)
// pool — every operation here goes through tenant.WithTx, never a bare
// query. encryptionKey is the decoded ENCRYPTION_KEY (vault.DecodeKey),
// resolved once at startup so a misconfigured key fails the process at
// boot, not silently on a founder's first key submission.
func NewHandler(appPool *pgxpool.Pool, encryptionKey []byte, apiKeyMockPrefix string) *Handler {
	return &Handler{appPool: appPool, encryptionKey: encryptionKey, apiKeyMockPrefix: apiKeyMockPrefix}
}

// Register mounts all 4 routes on rg. rg's group must already have
// middleware.RequireAuth applied — these handlers assume authctx is set.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/api-key", h.SubmitAPIKey)
	rg.GET("/api-key/status", h.APIKeyStatus)
	rg.DELETE("/api-key", h.DeleteAPIKey)
	rg.GET("/api-key/providers", h.ListProviders)

	//(approval-gate notification config) — see approvals.go
	// and pushsubscription.go in this same package.
	rg.GET("/approvals", h.GetApprovalsSettings)
	rg.PUT("/approvals", h.UpdateApprovalsSettings)
	rg.POST("/push-subscription", h.SubmitPushSubscription)
	rg.DELETE("/push-subscription", h.DeletePushSubscription)
}

type submitAPIKeyRequest struct {
	// Provider defaults to "anthropic" when omitted — founderstack-web's
	// current ApiKeyForm predates multi-provider support and never sends
	// this field, so leaving it unset must reproduce the exact original
	// single-provider behavior.
	Provider string `json:"provider"`
	APIKey   string `json:"api_key" binding:"required"`
}

// resolveProvider defaults an empty/omitted provider string to Anthropic
// — see submitAPIKeyRequest.Provider's doc comment for why.
func resolveProvider(raw string) llm.ProviderID {
	if raw == "" {
		return llm.ProviderAnthropic
	}
	return llm.ProviderID(raw)
}

// keyPreview returns a short, non-secret prefix of apiKey for display —
// up to 8 raw characters, not tied to any one provider's format-prefix
// length (Gemini's "AIza" is 4 chars, Anthropic's "sk-ant-" is 7), so one
// implementation works uniformly across every Catalog entry.
func keyPreview(apiKey string) string {
	n := 8
	if len(apiKey) < n {
		n = len(apiKey)
	}
	return apiKey[:n] + "..."
}

// invalidKeyMessage builds the founder-facing error message for a
// rejected/malformed key, generalized from founderstack-api's original
// Anthropic-only wording (which this reproduces exactly for
// provider=anthropic — a wire contract the frontend's error display
// depends on).
func invalidKeyMessage(meta llm.Meta) string {
	msg := fmt.Sprintf("The provided %s API key is invalid or inactive.", meta.Name)
	if meta.KeyPrefix != "" {
		return fmt.Sprintf("%s Please ensure it starts with '%s' and has active usage permissions.", msg, meta.KeyPrefix)
	}
	return msg + " Please ensure it has active usage permissions."
}

// SubmitAPIKey validates, encrypts, and stores a key for the caller's org
// — POST /api/v1/settings/api-key. Also marks the submitted provider as
// the org's active LLM provider, same "whichever key you submit becomes
// active" behavior as the original Anthropic-only endpoint, now
// provider-selectable.
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

	providerID := resolveProvider(req.Provider)
	meta, ok := llm.Catalog[providerID]
	if !ok {
		response.Fail(c, http.StatusBadRequest, "UNKNOWN_PROVIDER", "Unsupported LLM provider: "+string(providerID))
		return
	}

	ctx := c.Request.Context()
	if err := llm.ValidateKey(ctx, providerID, req.APIKey, h.apiKeyMockPrefix); err != nil {
		switch {
		case errors.Is(err, llm.ErrInvalidFormat), errors.Is(err, llm.ErrKeyRejected):
			response.Fail(c, http.StatusBadRequest, "INVALID_API_KEY", invalidKeyMessage(meta))
		case errors.Is(err, llm.ErrValidationUnavailable):
			// Distinct from the two cases above and from the Python
			// original — see llm.ErrValidationUnavailable's doc comment
			// for why "the provider is unreachable" shouldn't be reported
			// to the founder as "your key is invalid."
			response.Fail(c, http.StatusServiceUnavailable, "VALIDATION_UNAVAILABLE",
				fmt.Sprintf("Could not reach %s to validate the key. Please try again shortly.", meta.Name))
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
	keyPrefix := keyPreview(req.APIKey)
	llmProvider := string(providerID)

	err = tenant.WithTx(ctx, h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		keyID, err := q.UpsertAPIKey(ctx, dbgen.UpsertAPIKeyParams{
			OrgID:        user.OrgID,
			Provider:     llmProvider,
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
			LlmProvider:    &llmProvider,
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not save the API key")
		return
	}

	response.OK(c, http.StatusCreated,
		fmt.Sprintf("%s API key has been securely encrypted and validated. Your workspace is now active.", meta.Name),
		gin.H{"provider": llmProvider, "key_prefix": keyPrefix},
	)
}

type apiKeyStatus struct {
	Provider   string     `json:"provider"`
	IsValid    bool       `json:"is_valid"`
	KeyPrefix  string     `json:"key_prefix"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// APIKeyStatus reports whether the org has a key on file for the given
// provider — GET /api/v1/settings/api-key/status?provider=openai. provider
// defaults to "anthropic" when omitted, matching the original
// single-provider endpoint's shape exactly for existing callers. data is
// null (not a 404) when no key has ever been submitted for that provider,
// matching the Python original: "no key yet" is a normal state, not an
// error.
func (h *Handler) APIKeyStatus(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	providerID := resolveProvider(c.Query("provider"))
	if _, ok := llm.Catalog[providerID]; !ok {
		response.Fail(c, http.StatusBadRequest, "UNKNOWN_PROVIDER", "Unsupported LLM provider: "+string(providerID))
		return
	}

	var status *apiKeyStatus
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetKeyStatusByProvider(ctx, dbgen.GetKeyStatusByProviderParams{
			OrgID:    user.OrgID,
			Provider: string(providerID),
		})
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

// DeleteAPIKey deactivates the org's key for the given provider —
// DELETE /api/v1/settings/api-key?provider=openai. provider defaults to
// "anthropic" when omitted, matching the original endpoint's shape.
// Unconditional, like the Python original: no error if there was nothing
// to delete, idempotent either way.
func (h *Handler) DeleteAPIKey(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	providerID := resolveProvider(c.Query("provider"))
	if _, ok := llm.Catalog[providerID]; !ok {
		response.Fail(c, http.StatusBadRequest, "UNKNOWN_PROVIDER", "Unsupported LLM provider: "+string(providerID))
		return
	}

	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		if _, err := q.DeactivateKeyByProvider(ctx, dbgen.DeactivateKeyByProviderParams{
			OrgID:    user.OrgID,
			Provider: string(providerID),
		}); err != nil {
			return err
		}
		// Only clears the org's active pointer if providerID was the
		// active provider — deleting a non-active provider's key must
		// not clobber a different, still-active provider. See the
		// query's own doc comment.
		return q.ClearOrganizationActiveApiKeyForProvider(ctx, dbgen.ClearOrganizationActiveApiKeyForProviderParams{
			ID:          user.OrgID,
			LlmProvider: (*string)(&providerID),
		})
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not remove the API key")
		return
	}

	c.Status(http.StatusNoContent)
}

type providerStatusView struct {
	Provider      string     `json:"provider"`
	Name          string     `json:"name"`
	KeyPrefixHint string     `json:"key_prefix_hint"`
	IsConfigured  bool       `json:"is_configured"`
	IsValid       bool       `json:"is_valid"`
	IsActive      bool       `json:"is_active"`
	KeyPrefix     *string    `json:"key_prefix,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
}

// ListProviders merges the static llm.Catalog (every LLM provider BYOK
// supports) with the org's actual stored keys — GET
// /api/v1/settings/api-key/providers. Mirrors
// internal/api/integrations.Handler.ListIntegrations' catalog+status merge
// pattern: every provider always appears, even ones the org has never
// configured, so the frontend can render a full picker/status grid (and
// know which provider is "active") from one request instead of one
// GET .../status call per provider.
func (h *Handler) ListProviders(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}

	byProvider := make(map[string]dbgen.ListKeyStatusesRow)
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		rows, err := q.ListKeyStatuses(ctx, user.OrgID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			byProvider[row.Provider] = row
		}
		return nil
	})
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not fetch provider status")
		return
	}

	views := make([]providerStatusView, 0, len(llm.Catalog))
	for provider, meta := range llm.Catalog {
		view := providerStatusView{
			Provider:      string(provider),
			Name:          meta.Name,
			KeyPrefixHint: meta.KeyPrefix,
		}
		if row, found := byProvider[string(provider)]; found {
			view.IsConfigured = true
			view.IsValid = row.IsValid != nil && *row.IsValid
			view.IsActive = row.IsActive
			keyPrefix := row.KeyPrefix
			view.KeyPrefix = &keyPrefix
			if row.UpdatedAt.Valid {
				t := row.UpdatedAt.Time
				view.UpdatedAt = &t
			}
			if row.LastUsedAt.Valid {
				t := row.LastUsedAt.Time
				view.LastUsedAt = &t
			}
		}
		views = append(views, view)
	}
	// map iteration order is randomized — sort for a stable response so
	// the frontend's provider grid doesn't reorder itself on every fetch.
	sort.Slice(views, func(i, j int) bool { return views[i].Provider < views[j].Provider })

	response.OK(c, http.StatusOK, "", views)
}
