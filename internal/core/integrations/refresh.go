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

// refreshWindow: a connection is refreshed once its token expires within
// this long, not only once it has already expired — refreshing early
// avoids a request failing mid-flight because the token happened to
// expire between the last scan and the next one.
const refreshWindow = 10 * time.Minute

// RunRefreshJob blocks, refreshing expiring OAuth connections every
// RefreshInterval until ctx is cancelled. Intended to run in its own
// goroutine, started once at startup (cmd/api/main.go) alongside the
// HTTP server, cancelled on the same shutdown signal.
//
// Runs against systemPool (app_system, BYPASSRLS) — scanning expiring
// connections across every org is inherently a cross-tenant system-context
// operation, the same reasoning as the Clerk webhook's org creation and
// this package's own ListExpiringConnectionsSystem query. Never uses
// tenant.WithTx: there is no single org to scope this to.
func RunRefreshJob(ctx context.Context, systemPool *pgxpool.Pool, encryptionKey []byte, registry *Registry) {
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()

	// Run once immediately rather than waiting a full interval after
	// startup — a connection that expired while the process was down
	// shouldn't have to wait up to RefreshInterval more to be caught.
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
			// A connection exists for a service no longer wired into the
			// registry (e.g. removed from main.go but not yet cleaned up
			// in the DB) — nothing this job can do about it; skip and
			// leave the row for a human to investigate rather than
			// guessing at expiring it.
			continue
		}
		refresher, ok := provider.(Refreshable)
		if !ok {
			// Shouldn't happen — ListExpiringConnectionsSystem only
			// returns rows with a token_expires_at set, and only
			// Refreshable providers ever populate that column (see
			// SaveConnection/toTimestamptz) — but a provider could in
			// principle stop implementing Refreshable across a deploy
			// while old rows still carry an expiry. Not this job's
			// problem to resolve; skip.
			continue
		}

		tok, err := decodeToken(row.EncryptedCredentials, nil, encryptionKey)
		if err != nil {
			slog.Error("integrations: decode token for refresh", "service", row.ServiceName, "org_id", row.OrgID, "error", err)
			continue
		}
		if tok.RefreshToken == "" {
			// Nothing to refresh with — mark expired now rather than
			// re-scanning this same row every 30 minutes forever.
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
		// A refresh response never re-sends provider-specific Extra
		// fields — preserve what the connection already had, same
		// reasoning as the Status handler's manual refresh path.
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
