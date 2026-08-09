// Package llm builds and validates per-org Anthropic clients from BYOK
// keys stored in api_key_registry. Mirrors founderstack-api's
// app/services/anthropic_service.py + app/core/llm.py — same dual-path
// validation (mock prefix vs a real, cheap API call), same idea of "one
// function that hands a handler a ready-to-use client for an org" — but
// stateless: the Python original caches resolved clients in-process for 5
// minutes with invalidation-on-rotation logic; nothing in this codebase
// calls GetClient yet (agent execution is workflow 9), so that complexity
// isn't added until there's a real caller and measured need for it.
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
	// ErrInvalidFormat means the key doesn't even look like an Anthropic
	// key — rejected before spending a network call on it.
	ErrInvalidFormat = errors.New("llm: key does not start with sk-ant-")
	// ErrKeyRejected means Anthropic's API was reached and it rejected the
	// key (bad credentials) — maps to a 400, this is the founder's problem.
	ErrKeyRejected = errors.New("llm: Anthropic rejected the API key")
	// ErrValidationUnavailable means the key's shape is fine but Anthropic's
	// API couldn't be reached to confirm it (network error, 5xx, timeout) —
	// maps to a 502/503, this is NOT the founder's problem. The Python
	// original conflates this into the same "invalid key" response
	// (validate_key's try/except returns False for any exception, including
	// a network failure); distinguishing it here is a deliberate
	// improvement, not a behavior a frontend depends on being wrong.
	ErrValidationUnavailable = errors.New("llm: could not reach Anthropic to validate the key")
)

// anthropicKeyPrefix is Anthropic's real key format prefix — distinct from
// the configurable mock prefix (ANTHROPIC_API_KEY_MOCK_PREFIX), which is
// itself always a "sk-ant-..." string in this codebase's .env
// (sk-ant-test-) so the format check applies to both paths uniformly.
const anthropicKeyPrefix = "sk-ant-"

// ValidateKey checks an Anthropic API key is real and active. mockPrefix
// (ANTHROPIC_API_KEY_MOCK_PREFIX) short-circuits to success without any
// network call — used in local dev/tests to avoid depending on a real
// Anthropic account.
func ValidateKey(ctx context.Context, apiKey, mockPrefix string) error {
	return validateKey(ctx, apiKey, mockPrefix, verifyAgainstAnthropic)
}

// verify is the network-calling half of validateKey, extracted so the
// dispatch logic below (format check -> mock-prefix short-circuit ->
// verify) is unit-testable with a fake verify — including edge cases like
// an empty mockPrefix — without depending on Anthropic's API being
// reachable for the test suite to pass.
type verify func(ctx context.Context, apiKey string) error

func validateKey(ctx context.Context, apiKey, mockPrefix string, verifyFn verify) error {
	if !strings.HasPrefix(apiKey, anthropicKeyPrefix) {
		return ErrInvalidFormat
	}
	// mockPrefix == "" must never match: strings.HasPrefix(s, "") is true
	// for every s, which would silently disable real validation entirely
	// if ANTHROPIC_API_KEY_MOCK_PREFIX were ever left blank.
	if mockPrefix != "" && strings.HasPrefix(apiKey, mockPrefix) {
		return nil
	}
	return verifyFn(ctx, apiKey)
}

// verifyAgainstAnthropic is the real, network-calling verify — a cheap,
// side-effect-free call, same as the Python original's
// client.models.list(limit=1), just enough to know the key works.
func verifyAgainstAnthropic(ctx context.Context, apiKey string) error {
	client := newClient(apiKey)
	_, err := client.Models.List(ctx, anthropic.ModelListParams{Limit: anthropic.Int(1)})
	if err == nil {
		return nil
	}

	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == 401 {
		return fmt.Errorf("%w: %w", ErrKeyRejected, err)
	}
	return fmt.Errorf("%w: %w", ErrValidationUnavailable, err)
}

// GetClient resolves org's active Anthropic key (via tenant.WithTx —
// app_user, RLS-scoped to org) and returns a ready-to-use client. Returns
// pgx.ErrNoRows (unwrapped, check with errors.Is) if the org has no active
// key on file.
func GetClient(ctx context.Context, appPool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID) (anthropic.Client, error) {
	var encryptedKey string
	err := tenant.WithTx(ctx, appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetActiveAnthropicKeyByOrgID(ctx, orgID)
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
