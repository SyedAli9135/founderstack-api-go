package integrations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
	"github.com/founderstack/api/internal/pkg/safego"
)

// RefreshInterval is how often the background job scans for
// soon-to-expire connections.
const RefreshInterval = 5 * time.Minute

// refreshWindow: refresh a token this long before it expires. It must exceed
// RefreshInterval by a comfortable margin so a token can't slip between two
// scans and be used expired.
const refreshWindow = 15 * time.Minute

// onDemandSkew: a tool call that finds the token inside this margin of expiry
// refreshes it first, so it never sends one that lapses mid-request.
const onDemandSkew = 2 * time.Minute

// IsPermanentRefreshError reports whether a refresh failed because the
// refresh token itself is dead (revoked, expired, wrong client) — the only
// case where the connection really needs the user to reconnect. A timeout, a
// 5xx or a rate limit says nothing about the token; marking the connection
// expired for one would force a needless reconnect.
func IsPermanentRefreshError(err error) bool {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return false
	}
	if re.Response != nil {
		switch sc := re.Response.StatusCode; {
		case sc == http.StatusTooManyRequests, sc == http.StatusRequestTimeout, sc >= 500:
			return false
		}
	}
	switch re.ErrorCode {
	case "invalid_grant", "invalid_client", "unauthorized_client", "access_denied":
		return true
	}
	// A 4xx with no recognisable code (some providers answer in their own shape).
	return re.Response != nil && re.Response.StatusCode >= 400 && re.Response.StatusCode < 500
}

// RunRefreshJob refreshes expiring OAuth connections every RefreshInterval
// until ctx is cancelled. Runs on systemPool (app_system, BYPASSRLS): scanning
// across every org is cross-tenant, so tenant.WithTx doesn't apply.
func RunRefreshJob(ctx context.Context, systemPool *pgxpool.Pool, encryptionKey []byte, registry *Registry) {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()

	// Catch anything that expired while the process was down, don't wait
	// a full interval.
	_ = safego.Do("integrations: refresh job", func() { refreshExpiringConnections(ctx, systemPool, encryptionKey, registry) })

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = safego.Do("integrations: refresh job", func() { refreshExpiringConnections(ctx, systemPool, encryptionKey, registry) })
		}
	}
}

func refreshExpiringConnections(ctx context.Context, systemPool *pgxpool.Pool, encryptionKey []byte, registry *Registry) {
	q := dbgen.New(systemPool)

	rows, err := q.ListExpiringConnectionsSystem(ctx, toTimestamptz(time.Now().Add(refreshWindow)))
	if err != nil {
		slog.Error("integrations: list expiring connections", "error", err)
		return
	}
	for _, row := range rows {
		refreshConnection(ctx, systemPool, encryptionKey, registry, row.ID)
	}
}

// refreshConnection claims one connection (row lock, SKIP LOCKED) and
// refreshes it inside that transaction. With several instances running this
// job, only one refreshes a given connection: a second would present a
// refresh token the first has already used, which providers that rotate
// refresh tokens answer with invalid_grant — breaking a healthy connection.
func refreshConnection(ctx context.Context, pool *pgxpool.Pool, encryptionKey []byte, registry *Registry, id pgtype.UUID) {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.LockConnectionForRefreshSystem(ctx, dbgen.LockConnectionForRefreshSystemParams{
			ID: id, TokenExpiresAt: toTimestamptz(time.Now().Add(refreshWindow)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // another instance has it, or it was just refreshed
		}
		if err != nil {
			return err
		}

		provider, ok := registry.Get(row.ServiceName)
		if !ok {
			// Service no longer registered (e.g. removed from main.go) —
			// leave the row for a human, don't guess at expiring it.
			return nil
		}
		refresher, ok := provider.(Refreshable)
		if !ok {
			return nil
		}
		tok, err := decodeToken(row.EncryptedCredentials, nil, encryptionKey)
		if err != nil {
			slog.Error("integrations: decode token for refresh", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			return nil
		}
		if tok.RefreshToken == "" {
			// Nothing to refresh with — mark expired instead of rescanning forever.
			_, err := q.MarkConnectionExpiredByIDSystem(ctx, row.ID)
			return err
		}

		newTok, err := refresher.RefreshAccessToken(ctx, tok.RefreshToken)
		if err != nil {
			if IsPermanentRefreshError(err) {
				slog.Warn("integrations: refresh token rejected, marking expired", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
				_, mErr := q.MarkConnectionExpiredByIDSystem(ctx, row.ID)
				return mErr
			}
			// Transient (timeout, 5xx, rate limit): the token is fine; try again next tick.
			slog.Warn("integrations: refresh failed, will retry", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			return nil
		}
		// A refresh response never re-sends provider-specific Extra — preserve
		// what the connection already had.
		if newTok.Extra == nil {
			newTok.Extra = tok.Extra
		}
		encrypted, _, err := encodeToken(*newTok, encryptionKey)
		if err != nil {
			slog.Error("integrations: encode refreshed token", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			return nil
		}
		_, err = q.UpdateConnectionTokensByIDSystem(ctx, dbgen.UpdateConnectionTokensByIDSystemParams{
			ID: row.ID, EncryptedCredentials: &encrypted, TokenExpiresAt: toTimestamptz(newTok.ExpiresAt),
		})
		return err
	})
	if err != nil {
		slog.Error("integrations: refresh connection", "connection_id", id.String(), "error", err)
	}
}

// GetFreshIntegrationToken is GetIntegrationToken for a caller about to use
// the token: if it is expired or about to be, it is refreshed first, so a tool
// call doesn't fail for the minutes between two scans of the background job.
// Concurrent callers queue on the connection's row lock, so the refresh token
// is presented once.
//
// A connection whose refresh token is dead is marked expired and reported as
// ErrTokenUnavailable (the reconnect prompt); a transient refresh failure
// returns the stored token, since it may well still be accepted.
func GetFreshIntegrationToken(ctx context.Context, pool *pgxpool.Pool, encryptionKey []byte, registry *Registry, orgID pgtype.UUID, service string) (Token, error) {
	var result Token
	var unavailable bool
	err := tenant.WithTx(ctx, pool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		row, err := q.GetConnectionByOrgServiceForUpdate(ctx, dbgen.GetConnectionByOrgServiceForUpdateParams{OrgID: orgID, ServiceName: service})
		if err != nil {
			return err
		}
		if row.IsActive == nil || !*row.IsActive || derefOr(row.OauthStatus, "pending") != "connected" {
			unavailable = true
			return nil
		}
		tok, err := decodeToken(row.EncryptedCredentials, row.OauthScopes, encryptionKey)
		if err != nil {
			return err
		}
		result = tok
		if tok.ExpiresAt.IsZero() || time.Until(tok.ExpiresAt) > onDemandSkew || tok.RefreshToken == "" || registry == nil {
			return nil
		}
		provider, ok := registry.Get(service)
		if !ok {
			return nil
		}
		refresher, ok := provider.(Refreshable)
		if !ok {
			return nil
		}

		newTok, err := refresher.RefreshAccessToken(ctx, tok.RefreshToken)
		if err != nil {
			if IsPermanentRefreshError(err) {
				slog.Warn("integrations: refresh token rejected on demand, marking expired", "service", service, "org_id", orgID, "error", err)
				unavailable = true
				_, mErr := q.MarkConnectionExpired(ctx, dbgen.MarkConnectionExpiredParams{OrgID: orgID, ServiceName: service})
				return mErr
			}
			slog.Warn("integrations: on-demand refresh failed, using the stored token", "service", service, "org_id", orgID, "error", err)
			return nil
		}
		if newTok.Extra == nil {
			newTok.Extra = tok.Extra
		}
		newTok.Scopes = tok.Scopes
		encrypted, _, err := encodeToken(*newTok, encryptionKey)
		if err != nil {
			return err
		}
		if _, err := q.UpdateConnectionTokens(ctx, dbgen.UpdateConnectionTokensParams{
			OrgID: orgID, ServiceName: service, EncryptedCredentials: &encrypted, TokenExpiresAt: toTimestamptz(newTok.ExpiresAt),
		}); err != nil {
			return err
		}
		result = *newTok
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotConnected
	}
	if err != nil {
		return Token{}, err
	}
	if unavailable {
		return Token{}, ErrTokenUnavailable
	}
	return result, nil
}
