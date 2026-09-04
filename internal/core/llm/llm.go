package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
	"github.com/founderstack/api/internal/pkg/vault"
)

var (
	// ErrUnknownProvider means the caller passed a ProviderID not present
	// in Catalog — rejected before any format check or network call.
	ErrUnknownProvider = errors.New("llm: unknown LLM provider")
	// ErrInvalidFormat means the key doesn't even look right for the given
	// provider — rejected before spending a network call on it.
	ErrInvalidFormat = errors.New("llm: key does not match the provider's expected format")
	// ErrKeyRejected means the provider's API was reached and it rejected
	// the key (bad credentials) — maps to a 400, this is the founder's
	// problem.
	ErrKeyRejected = errors.New("llm: provider rejected the API key")
	// ErrValidationUnavailable means the key's shape is fine but the
	// provider's API couldn't be reached to confirm it (network error,
	// 5xx, timeout) — maps to a 502/503, not the founder's problem.
	ErrValidationUnavailable = errors.New("llm: could not reach the provider to validate the key")
)

// verify is the network-calling half of validateKey, extracted so the
// dispatch logic below (mock-prefix short-circuit -> format check ->
// verify) is unit-testable with a fake verify per provider — including
// edge cases like an empty mockPrefix — without depending on any
// provider's API being reachable for the test suite to pass.
type verify func(ctx context.Context, apiKey string) error

// ValidateKey checks apiKey is a real, active key for provider. mockPrefix
// (API_KEY_MOCK_PREFIX) short-circuits to success without any network call
// — used in local dev/tests to avoid depending on a real account with any
// of the 5 providers.
func ValidateKey(ctx context.Context, provider ProviderID, apiKey, mockPrefix string) error {
	meta, ok := Catalog[provider]
	if !ok {
		return ErrUnknownProvider
	}
	verifyFn, ok := verifiers[provider]
	if !ok {
		return ErrUnknownProvider
	}
	return validateKey(ctx, apiKey, meta.KeyPrefix, mockPrefix, verifyFn)
}

// validateKey checks the mock prefix before the format check, since one
// shared mock prefix can't satisfy every provider's real key shape at once
// (Anthropic's "sk-ant-", Gemini's "AIza", ...).
//
// mockPrefix == "" must never match: strings.HasPrefix(s, "") is true for
// every s, which would silently disable real validation if
// API_KEY_MOCK_PREFIX were ever left blank.
func validateKey(ctx context.Context, apiKey, keyPrefix, mockPrefix string, verifyFn verify) error {
	if mockPrefix != "" && strings.HasPrefix(apiKey, mockPrefix) {
		return nil
	}
	if keyPrefix != "" && !strings.HasPrefix(apiKey, keyPrefix) {
		return ErrInvalidFormat
	}
	return verifyFn(ctx, apiKey)
}

// GetClient resolves org's active Anthropic key (via tenant.WithTx —
// app_user, RLS-scoped to org) and returns a ready-to-use client. Returns
// pgx.ErrNoRows (unwrapped, check with errors.Is) if the org has no active
// Anthropic key on file. Anthropic-only by design — see the package doc
// comment.
func GetClient(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID) (anthropic.Client, error) {
	var encryptedKey string
	err := tenant.WithTx(ctx, appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetActiveKeyByOrgIDAndProvider(ctx, dbgen.GetActiveKeyByOrgIDAndProviderParams{
			OrgID:    orgID,
			Provider: string(ProviderAnthropic),
		})
		if err != nil {
			return err
		}
		encryptedKey = row.EncryptedKey
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return anthropic.Client{}, err
		}
		return anthropic.Client{}, fmt.Errorf("llm: fetch key: %w", err)
	}

	apiKey, err := vault.Decrypt(encryptedKey, encryptionKey)
	if err != nil {
		return anthropic.Client{}, fmt.Errorf("llm: decrypt key: %w", err)
	}

	return newClient(apiKey), nil
}

// newClient builds a client scoped to exactly this key — never the
// process's ambient ANTHROPIC_API_KEY env var, if one happened to be set.
// In a multi-tenant server that would be a serious bug (one org's client
// silently using another credential), not a convenience default.
func newClient(apiKey string) anthropic.Client {
	return anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithoutEnvironmentDefaults(),
	)
}
