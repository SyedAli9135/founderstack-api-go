package llm

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
	"github.com/founderstack/api/internal/pkg/vault"
)

// ErrNoActiveKey means the org has no valid, active key for provider —
// distinct from ErrUnknownProvider (a bad provider string) so callers
// (workflow 9's Launcher) can map it to a specific "no BYOK key
// configured" response rather than a generic error.
var ErrNoActiveKey = errors.New("llm: no active key for this provider")

// ResolveChatClient builds a usable ChatClient for orgID's active provider
// and model, dispatching across all 5 BYOK providers. Unlike GetClient
// (kept Anthropic-only for its existing callers/tests), this is what
// graph.Launcher calls.
func ResolveChatClient(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, provider ProviderID, model string) (ChatClient, error) {
	if _, ok := Catalog[provider]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}

	var encryptedKey string
	err := tenant.WithTx(ctx, appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetActiveKeyByOrgIDAndProvider(ctx, dbgen.GetActiveKeyByOrgIDAndProviderParams{
			OrgID: orgID, Provider: string(provider),
		})
		if err != nil {
			return err
		}
		encryptedKey = row.EncryptedKey
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: provider %q", ErrNoActiveKey, provider)
		}
		return nil, fmt.Errorf("llm: fetch active key: %w", err)
	}

	apiKey, err := vault.Decrypt(encryptedKey, encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("llm: decrypt key: %w", err)
	}

	switch provider {
	case ProviderAnthropic:
		return NewAnthropicChatClient(apiKey, model), nil
	case ProviderOpenAI:
		return NewOpenAICompatibleChatClient(openAIBaseURL, apiKey, model), nil
	case ProviderQwen:
		return NewOpenAICompatibleChatClient(qwenBaseURL, apiKey, model), nil
	case ProviderDeepSeek:
		return NewOpenAICompatibleChatClient(deepSeekBaseURL, apiKey, model), nil
	case ProviderGemini:
		return NewGeminiChatClient(apiKey, model), nil
	default:
		// Unreachable given the Catalog check above, but fail closed
		// rather than silently returning a nil ChatClient if Catalog ever
		// grows a provider this switch hasn't been updated for.
		return nil, fmt.Errorf("%w: %q has no ChatClient implementation", ErrUnknownProvider, provider)
	}
}
