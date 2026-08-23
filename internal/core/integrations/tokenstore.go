package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
	"github.com/founderstack/api/internal/pkg/vault"
)

// ErrNotConnected means the org has never connected this service —
// GetIntegrationToken's normal "nothing to fetch" case, distinct from a
// connection that exists but has expired (ErrTokenUnavailable).
var ErrNotConnected = errors.New("integrations: service not connected for this org")

// ErrTokenUnavailable means a connection row exists but isn't currently
// usable (revoked, or expired with no refresh available yet) — the MCP
// gateway should surface this as "reconnect needed," not retry blindly.
var ErrTokenUnavailable = errors.New("integrations: connection is not active")

// Connection is a decrypted, org-scoped integration connection as read
// back from mcp_connections.
type Connection struct {
	ID          pgtype.UUID
	ServiceName string
	OAuthStatus string
	IsActive    bool
	Scopes      []string
	Token       Token
}

// ConnectionSummary is the lightweight (no decryption) shape used to
// build GET /api/v1/integrations' merged catalog+status list.
type ConnectionSummary struct {
	ServiceName string
	OAuthStatus string
	IsActive    bool
	Scopes      []string
	ConnectedAt time.Time
}

// tokenEnvelope is the JSON shape encrypted into
// mcp_connections.encrypted_credentials. Scopes live in their own jsonb
// column (oauth_scopes), not here, so they stay readable without
// decrypting anything.
type tokenEnvelope struct {
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time         `json:"expires_at,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}

// SaveConnection encrypts tok and upserts it for orgID/service.
// credentialProvider records how the credential was obtained ("oauth" for
// the OAuth providers, "manual" for pasted keys/PATs/bot tokens — the
// column's existing default). displayName is shown on the integration
// card; oauthStatus is almost always "connected" here except for a
// refresh failure path that wants to write "expired" atomically with new
// (or unchanged) token data.
func SaveConnection(ctx context.Context, pool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, service, displayName, credentialProvider, oauthStatus string, tok Token) error {
	encrypted, scopesJSON, err := encodeToken(tok, encryptionKey)
	if err != nil {
		return err
	}

	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.UpsertConnection(ctx, dbgen.UpsertConnectionParams{
			OrgID:                orgID,
			ServiceName:          service,
			DisplayName:          &displayName,
			CredentialProvider:   &credentialProvider,
			EncryptedCredentials: &encrypted,
			OauthStatus:          &oauthStatus,
			OauthScopes:          scopesJSON,
			TokenExpiresAt:       toTimestamptz(tok.ExpiresAt),
		})
		return err
	})
}

// GetConnection fetches and decrypts orgID's connection for service.
// Returns pgx.ErrNoRows (propagated, not wrapped) if no such connection
// exists — callers check errors.Is(err, pgx.ErrNoRows), same convention
// as settings.APIKeyStatus.
func GetConnection(ctx context.Context, pool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, service string) (*Connection, error) {
	var conn *Connection
	err := tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetConnectionByOrgService(ctx, dbgen.GetConnectionByOrgServiceParams{OrgID: orgID, ServiceName: service})
		if err != nil {
			return err
		}
		tok, err := decodeToken(row.EncryptedCredentials, row.OauthScopes, encryptionKey)
		if err != nil {
			return err
		}
		conn = &Connection{
			ID:          row.ID,
			ServiceName: row.ServiceName,
			OAuthStatus: derefOr(row.OauthStatus, "pending"),
			IsActive:    row.IsActive != nil && *row.IsActive,
			Scopes:      tok.Scopes,
			Token:       tok,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// ListConnections returns every connection orgID has (any status), for
// GET /api/v1/integrations to merge against the static Catalog. Doesn't
// decrypt anything — the list view never needs the token itself.
func ListConnections(ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID) ([]ConnectionSummary, error) {
	var summaries []ConnectionSummary
	err := tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		rows, err := q.ListConnectionsByOrg(ctx, orgID)
		if err != nil {
			return err
		}
		summaries = make([]ConnectionSummary, 0, len(rows))
		for _, row := range rows {
			var scopes []string
			if len(row.OauthScopes) > 0 {
				_ = json.Unmarshal(row.OauthScopes, &scopes)
			}
			summaries = append(summaries, ConnectionSummary{
				ServiceName: row.ServiceName,
				OAuthStatus: derefOr(row.OauthStatus, "pending"),
				IsActive:    row.IsActive != nil && *row.IsActive,
				Scopes:      scopes,
				ConnectedAt: fromTimestamptz(row.CreatedAt),
			})
		}
		return nil
	})
	return summaries, err
}

// RevokeConnection marks orgID's connection to service inactive/revoked —
// DELETE /api/v1/integrations/{service}.
func RevokeConnection(ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, service string) error {
	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.RevokeConnection(ctx, dbgen.RevokeConnectionParams{OrgID: orgID, ServiceName: service})
		return err
	})
}

// MarkExpired flips orgID's connection to service to oauth_status =
// 'expired' — used by GET .../status when validation fails and no
// refresh is possible.
func MarkExpired(ctx context.Context, pool *pgxpool.Pool, orgID pgtype.UUID, service string) error {
	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.MarkConnectionExpired(ctx, dbgen.MarkConnectionExpiredParams{OrgID: orgID, ServiceName: service})
		return err
	})
}

// UpdateTokens overwrites orgID's stored credentials for service after a
// successful refresh — used by GET .../status's refresh path.
func UpdateTokens(ctx context.Context, pool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, service string, tok Token) error {
	encrypted, _, err := encodeToken(tok, encryptionKey)
	if err != nil {
		return err
	}
	return tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		_, err := q.UpdateConnectionTokens(ctx, dbgen.UpdateConnectionTokensParams{
			OrgID:                orgID,
			ServiceName:          service,
			EncryptedCredentials: &encrypted,
			TokenExpiresAt:       toTimestamptz(tok.ExpiresAt),
		})
		return err
	})
}

// GetIntegrationToken is the shared lookup MCP tool handlers use to get a
// usable, decrypted Token for orgID's connection to service (workflow 5).
// Deliberately collapses "never connected" and "row exists but not
// usable" into two distinct sentinel errors rather than one generic
// failure, since the MCP gateway needs to tell the founder "connect X"
// apart from "reconnect X." Returns the full Token, not just AccessToken
// — the MCP Gateway needs Token.Extra too (Discord's `webhook.incoming`
// grant, e.g., has no usable AccessToken at all for posting a message;
// the webhook URL in Extra is the only credential that works).
func GetIntegrationToken(ctx context.Context, pool *pgxpool.Pool, encryptionKey []byte, orgID pgtype.UUID, service string) (Token, error) {
	conn, err := GetConnection(ctx, pool, encryptionKey, orgID, service)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Token{}, ErrNotConnected
		}
		return Token{}, err
	}
	if !conn.IsActive || conn.OAuthStatus != "connected" {
		return Token{}, ErrTokenUnavailable
	}
	return conn.Token, nil
}

func encodeToken(tok Token, key []byte) (encrypted string, scopesJSON []byte, err error) {
	plaintext, err := json.Marshal(tokenEnvelope{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		Extra:        tok.Extra,
	})
	if err != nil {
		return "", nil, fmt.Errorf("integrations: marshal token: %w", err)
	}
	encrypted, err = vault.Encrypt(string(plaintext), key)
	if err != nil {
		return "", nil, fmt.Errorf("integrations: encrypt token: %w", err)
	}
	scopesJSON, err = json.Marshal(tok.Scopes)
	if err != nil {
		return "", nil, fmt.Errorf("integrations: marshal scopes: %w", err)
	}
	return encrypted, scopesJSON, nil
}

func decodeToken(encrypted *string, scopesJSON []byte, key []byte) (Token, error) {
	if encrypted == nil || *encrypted == "" {
		return Token{}, errors.New("integrations: connection has no stored credentials")
	}
	plaintext, err := vault.Decrypt(*encrypted, key)
	if err != nil {
		return Token{}, fmt.Errorf("integrations: decrypt token: %w", err)
	}
	var env tokenEnvelope
	if err := json.Unmarshal([]byte(plaintext), &env); err != nil {
		return Token{}, fmt.Errorf("integrations: unmarshal token: %w", err)
	}
	tok := Token{
		AccessToken:  env.AccessToken,
		RefreshToken: env.RefreshToken,
		ExpiresAt:    env.ExpiresAt,
		Extra:        env.Extra,
	}
	if len(scopesJSON) > 0 {
		_ = json.Unmarshal(scopesJSON, &tok.Scopes)
	}
	return tok, nil
}

func toTimestamptz(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func fromTimestamptz(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func derefOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}
