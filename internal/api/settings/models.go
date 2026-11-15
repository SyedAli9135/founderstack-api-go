package settings

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/api/authctx"
	"github.com/founderstack/api/internal/api/response"
	"github.com/founderstack/api/internal/core/llm"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
	"github.com/founderstack/api/internal/pkg/vault"
)

const modelCacheTTL = 10 * time.Minute

type modelCacheKey struct {
	org      pgtype.UUID
	provider llm.ProviderID
}

type modelCacheEntry struct {
	// encryptedKey ties the entry to the exact stored key: replacing the key
	// changes the ciphertext, so a stale list can't outlive its key.
	encryptedKey string
	models       []llm.ModelInfo
	expires      time.Time
}

// modelCache keeps provider model lists for a few minutes so opening the agent
// form doesn't call the provider every time. In-process only — a stale or
// missing entry just costs one extra provider call.
type modelCache struct {
	mu sync.Mutex
	m  map[modelCacheKey]modelCacheEntry
}

func (c *modelCache) get(k modelCacheKey, encryptedKey string) ([]llm.ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || e.encryptedKey != encryptedKey || time.Now().After(e.expires) {
		return nil, false
	}
	return e.models, true
}

func (c *modelCache) put(k modelCacheKey, encryptedKey string, models []llm.ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[modelCacheKey]modelCacheEntry)
	}
	c.m[k] = modelCacheEntry{encryptedKey: encryptedKey, models: models, expires: time.Now().Add(modelCacheTTL)}
}

type modelsResponse struct {
	Provider    string          `json:"provider"`
	Models      []llm.ModelInfo `json:"models"`
	Recommended string          `json:"recommended"`
}

// ListModels returns the chat models the org's stored key for :provider can
// actually use, straight from the provider. Agents pick from this list, so a
// model the provider has retired is never offered.
//
// GET /api/v1/settings/api-key/providers/:provider/models
func (h *Handler) ListModels(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	provider := llm.ProviderID(c.Param("provider"))
	if _, known := llm.Catalog[provider]; !known {
		response.Fail(c, http.StatusBadRequest, "UNKNOWN_PROVIDER", "Unsupported LLM provider: "+string(provider))
		return
	}

	var encryptedKey string
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetActiveKeyByOrgIDAndProvider(ctx, dbgen.GetActiveKeyByOrgIDAndProviderParams{OrgID: user.OrgID, Provider: string(provider)})
		if err != nil {
			return err
		}
		encryptedKey = row.EncryptedKey
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, http.StatusBadRequest, "NO_API_KEY", "No active key for this provider. Add one in LLM Providers first.")
		return
	}
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read the provider key")
		return
	}

	ck := modelCacheKey{org: user.OrgID, provider: provider}
	models, hit := h.modelCache.get(ck, encryptedKey)
	if !hit {
		apiKey, err := vault.Decrypt(encryptedKey, h.encryptionKey)
		if err != nil {
			response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read the provider key")
			return
		}
		if h.apiKeyMockPrefix != "" && strings.HasPrefix(apiKey, h.apiKeyMockPrefix) {
			// Mock keys never reach a provider, so offer a fixed list instead.
			models = []llm.ModelInfo{{ID: llm.DefaultModel(provider), DisplayName: llm.DefaultModel(provider)}}
		} else if models, err = llm.ListModels(c.Request.Context(), provider, apiKey); err != nil {
			switch {
			case errors.Is(err, llm.ErrKeyRejected):
				response.Fail(c, http.StatusBadRequest, "KEY_REJECTED", "The provider rejected the stored key. Replace it in LLM Providers.")
			default:
				response.Fail(c, http.StatusServiceUnavailable, "PROVIDER_UNAVAILABLE", "Could not reach the provider to list models. Try again, or type a model name.")
			}
			return
		}
		h.modelCache.put(ck, encryptedKey, models)
	}

	response.OK(c, http.StatusOK, "", modelsResponse{
		Provider: string(provider), Models: models, Recommended: llm.PreferredModel(provider, models),
	})
}

type testModelRequest struct {
	Model string `json:"model"`
}

type testModelResponse struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// TestModel sends one tiny real request on :provider with the chosen model, so
// a founder learns before building a workflow whether the model answers. The
// provider's model list alone isn't proof: it can include models that refuse
// every call or stall. Costs a few tokens on the org's own key.
//
// POST /api/v1/settings/api-key/providers/:provider/models/test
func (h *Handler) TestModel(c *gin.Context) {
	user, ok := authctx.FromContext(c)
	if !ok {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Missing auth context")
		return
	}
	if !user.CanModifyAgents() {
		response.Fail(c, http.StatusForbidden, "FORBIDDEN", "Only owners and admins can test models")
		return
	}
	provider := llm.ProviderID(c.Param("provider"))
	if _, known := llm.Catalog[provider]; !known {
		response.Fail(c, http.StatusBadRequest, "UNKNOWN_PROVIDER", "Unsupported LLM provider: "+string(provider))
		return
	}
	var req testModelRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Model) == "" || len(req.Model) > 100 {
		response.Fail(c, http.StatusBadRequest, "INVALID_MODEL", "model is required (at most 100 characters)")
		return
	}

	var encryptedKey string
	err := tenant.WithTx(c.Request.Context(), h.appPool, user.OrgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetActiveKeyByOrgIDAndProvider(ctx, dbgen.GetActiveKeyByOrgIDAndProviderParams{OrgID: user.OrgID, Provider: string(provider)})
		if err != nil {
			return err
		}
		encryptedKey = row.EncryptedKey
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		response.Fail(c, http.StatusBadRequest, "NO_API_KEY", "No active key for this provider. Add one in LLM Providers first.")
		return
	}
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read the provider key")
		return
	}
	apiKey, err := vault.Decrypt(encryptedKey, h.encryptionKey)
	if err != nil {
		response.Fail(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "Could not read the provider key")
		return
	}
	if h.apiKeyMockPrefix != "" && strings.HasPrefix(apiKey, h.apiKeyMockPrefix) {
		response.OK(c, http.StatusOK, "", testModelResponse{OK: true})
		return
	}

	elapsed, probeErr := llm.ProbeModel(c.Request.Context(), provider, apiKey, strings.TrimSpace(req.Model))
	out := testModelResponse{OK: probeErr == nil, LatencyMS: elapsed.Milliseconds()}
	if probeErr != nil {
		out.Error = probeErr.Error()
	}
	response.OK(c, http.StatusOK, "", out)
}
