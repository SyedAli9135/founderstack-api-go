package integrations

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/db/dbgen"
)

// RefreshInterval is how often the background job scans for
// soon-to-expire connections.
const RefreshInterval = 30 * time.Minute

// refreshWindow: refresh a token this long before it expires, not only
// after — avoids a request failing mid-flight between scans.
const refreshWindow = 10 * time.Minute

// RunRefreshJob refreshes expiring OAuth connections every RefreshInterval
// until ctx is cancelled. Runs on systemPool (app_system, BYPASSRLS): scanning
// across every org is cross-tenant, so tenant.WithTx doesn't apply.
func RunRefreshJob(ctx context.Context, systemPool *pgxpool.Pool, encryptionKey []byte, registry *Registry) {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()

	// Catch anything that expired while the process was down, don't wait
	// a full interval.
	refreshExpiringConnections(ctx, systemPool, encryptionKey, registry)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshExpiringConnections(ctx, systemPool, encryptionKey, registry)
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
		provider, ok := registry.Get(row.ServiceName)
		if !ok {
			// Service no longer registered (e.g. removed from main.go) —
			// leave the row for a human, don't guess at expiring it.
			continue
		}
		refresher, ok := provider.(Refreshable)
		if !ok {
			// Shouldn't happen (only Refreshable providers ever set
			// token_expires_at), but a provider could stop implementing
			// it across a deploy while old rows still carry one.
			continue
		}

		tok, err := decodeToken(row.EncryptedCredentials, nil, encryptionKey)
		if err != nil {
			slog.Error("integrations: decode token for refresh", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			continue
		}
		if tok.RefreshToken == "" {
			// Nothing to refresh with — mark expired instead of rescanning forever.
			if _, err := q.MarkConnectionExpiredByIDSystem(ctx, row.ID); err != nil {
				slog.Error("integrations: mark expired (no refresh token)", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			}
			continue
		}

		newTok, err := refresher.RefreshAccessToken(ctx, tok.RefreshToken)
		if err != nil {
			slog.Warn("integrations: refresh failed, marking expired", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			if _, err := q.MarkConnectionExpiredByIDSystem(ctx, row.ID); err != nil {
				slog.Error("integrations: mark expired (refresh failed)", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			}
			continue
		}
		// A refresh response never re-sends provider-specific Extra — preserve
		// what the connection already had.
		if newTok.Extra == nil {
			newTok.Extra = tok.Extra
		}

		encrypted, _, err := encodeToken(*newTok, encryptionKey)
		if err != nil {
			slog.Error("integrations: encode refreshed token", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			continue
		}
		if _, err := q.UpdateConnectionTokensByIDSystem(ctx, dbgen.UpdateConnectionTokensByIDSystemParams{
			ID:                   row.ID,
			EncryptedCredentials: &encrypted,
			TokenExpiresAt:       toTimestamptz(newTok.ExpiresAt),
		}); err != nil {
			slog.Error("integrations: persist refreshed token", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
		}
	}
}
