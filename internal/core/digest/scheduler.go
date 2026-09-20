package digest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/core/notify"
	"github.com/founderstack/api/internal/db/dbgen"
)

// RunScheduler fires on the top of every hour (per spec), aligned via an
// initial one-off timer rather than a plain 1h ticker so it doesn't drift
// to whatever minute the process happened to boot at. systemPool
// (BYPASSRLS) is required: scanning every org's digest_send_hour is
// inherently cross-tenant, same reasoning as workflows.RunScheduler.
func RunScheduler(ctx context.Context, systemPool *pgxpool.Pool, email notify.EmailSender, tokens *notify.DigestTokenSigner, appBaseURL string) {
	tick(ctx, systemPool, email, tokens, appBaseURL)

	timer := time.NewTimer(time.Until(nextTopOfHour(time.Now())))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			tick(ctx, systemPool, email, tokens, appBaseURL)
			timer.Reset(time.Until(nextTopOfHour(time.Now())))
		}
	}
}

func nextTopOfHour(t time.Time) time.Time {
	return t.Truncate(time.Hour).Add(time.Hour)
}

func tick(ctx context.Context, systemPool *pgxpool.Pool, email notify.EmailSender, tokens *notify.DigestTokenSigner, appBaseURL string) {
	q := dbgen.New(systemPool)

	due, err := q.ListOrgsDueForDigest(ctx)
	if err != nil {
		slog.Error("digest: list orgs due for digest", "error", err)
		return
	}
	if len(due) == 0 {
		return
	}

	sent := 0
	for _, org := range due {
		if err := sendDigest(ctx, q, email, tokens, appBaseURL, org.ID, org.Name, org.DigestTimezone); err != nil {
			slog.Error("digest: send failed", "org_id", org.ID.String(), "error", err)
			continue
		}
		if err := q.MarkDigestSent(ctx, org.ID); err != nil {
			slog.Error("digest: mark sent failed", "org_id", org.ID.String(), "error", err)
			continue
		}
		sent++
	}
	slog.Info("digest: sent daily digests", "due", len(due), "sent", sent)
}

func sendDigest(ctx context.Context, q *dbgen.Queries, email notify.EmailSender, tokens *notify.DigestTokenSigner, appBaseURL string, orgID pgtype.UUID, orgName, tz string) error {
	recipients, err := q.ListDigestRecipients(ctx, orgID)
	if err != nil {
		return fmt.Errorf("digest: list recipients: %w", err)
	}
	if len(recipients) == 0 {
		return nil // nobody to send to -- not an error, still counts as "sent" for the daily guard
	}

	payload, err := BuildPayload(ctx, q, orgID, orgName, tz)
	if err != nil {
		return err
	}

	token := tokens.Sign(uuid.UUID(orgID.Bytes))
	unsubscribeURL := fmt.Sprintf("%s/api/v1/settings/digest/unsubscribe?token=%s", appBaseURL, token)

	subject, textBody, htmlBody, err := RenderEmail(payload, unsubscribeURL)
	if err != nil {
		return err
	}

	for _, to := range recipients {
		if err := email.Send(ctx, to, subject, textBody, htmlBody); err != nil {
			slog.Warn("digest: send to recipient failed", "org_id", orgID.String(), "to", to, "err", err)
		}
	}

	writeAuditLog(ctx, q, orgID, len(recipients))
	return nil
}

// writeAuditLog is best-effort: a failed audit write must not undo an
// email that already went out, so it's only logged, never propagated.
func writeAuditLog(ctx context.Context, q *dbgen.Queries, orgID pgtype.UUID, recipientCount int) {
	metadata, err := json.Marshal(map[string]any{"recipient_count": recipientCount})
	if err != nil {
		return
	}
	status := "success"
	resourceType := "organization"
	if err := q.InsertAuditLog(ctx, dbgen.InsertAuditLogParams{
		OrgID: orgID, ActorType: "system", Action: "digest.sent",
		ResourceType: &resourceType, ResourceID: orgID, Status: &status, MetadataInfo: metadata,
	}); err != nil {
		slog.Warn("digest: write audit log failed", "org_id", orgID.String(), "err", err)
	}
}
