// Package notify deliberately depends on nothing in internal/core/graph
// — graph.RunDeps holds a *Notifier, not the other way around, so this
// package takes explicit primitive arguments instead of a RunDeps value.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/core/llm"
	coremcp "github.com/founderstack/api/internal/core/mcp"
	"github.com/founderstack/api/internal/db/dbgen"
	"github.com/founderstack/api/internal/db/tenant"
)

// The one place the 24h auto-expiry window lives — both
// approvalgate.go's expires_at column and this package's action-token
// expiry reference it, so the two can never drift apart.
const ApprovalTTL = 24 * time.Hour

// A nil *Notifier is valid — RunDeps.Notifier is nil-checked before use
// so tests building RunDeps by hand don't need one.
type Notifier struct {
	Email  EmailSender
	Push   *WebPushSender
	Tokens *ActionTokenSigner
	// Builds ApproveURL/RejectURL — see PushPayload's doc comment for why
	// the server builds the full URL, not the service worker.
	apiBaseURL string
}

func New(email EmailSender, push *WebPushSender, tokens *ActionTokenSigner, apiBaseURL string) *Notifier {
	return &Notifier{Email: email, Push: push, Tokens: tokens, apiBaseURL: apiBaseURL}
}

// Fires all 3 channels concurrently, each independently
// logged-not-propagated. Called via `go` from writeApprovalGate, so ctx
// is a fresh context.Background(), not the run's own (which may already
// be cancelled by the time a slow HTTP call would check it).
func (n *Notifier) NotifyApprovalRequired(ctx context.Context, appPool *pgxpool.Pool, gateway *coremcp.Gateway, orgID pgtype.UUID, approvalID uuid.UUID, riskLevel string, calls []llm.ToolCall) {
	if n == nil {
		return
	}
	summary := summarizeToolCalls(calls)
	expiresAt := time.Now().Add(ApprovalTTL)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		channel, err := lookupSlackChannel(ctx, appPool, orgID)
		if err != nil {
			slog.Warn("notify: look up org's approvals Slack channel failed", "org_id", orgID, "err", err)
			return
		}
		if channel == "" {
			return
		}
		text := fmt.Sprintf("🔴 Needs approval (%s): %s", riskLevel, summary)
		sendSlackApproval(ctx, gateway, orgID, channel, text)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		n.notifyApprovers(ctx, appPool, orgID, approvalID, summary)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		n.notifyPushSubscribers(ctx, appPool, orgID, approvalID, riskLevel, summary, expiresAt)
	}()

	wg.Wait()
}

func (n *Notifier) notifyApprovers(ctx context.Context, appPool *pgxpool.Pool, orgID pgtype.UUID, approvalID uuid.UUID, summary string) {
	if n.Email == nil {
		return
	}
	approvers, err := listApproverEmails(ctx, appPool, orgID)
	if err != nil {
		slog.Warn("notify: list approvers for org failed", "org_id", orgID, "err", err)
		return
	}
	for _, approver := range approvers {
		subject := "Approval needed: " + summary
		body := fmt.Sprintf("An agent run needs your approval.\n\n%s\n\nApproval ID: %s\nOpen the app to approve or reject.", summary, approvalID)
		if err := n.Email.Send(ctx, approver.email, subject, body); err != nil {
			slog.Warn("notify: send approval email failed", "org_id", orgID, "to", approver.email, "err", err)
		}
	}
}

func (n *Notifier) notifyPushSubscribers(ctx context.Context, appPool *pgxpool.Pool, orgID pgtype.UUID, approvalID uuid.UUID, riskLevel, summary string, expiresAt time.Time) {
	if n.Push == nil {
		return
	}
	subs, err := listPushSubscriptions(ctx, appPool, orgID)
	if err != nil {
		slog.Warn("notify: list push subscriptions for org failed", "org_id", orgID, "err", err)
		return
	}
	for _, sub := range subs {
		var approveURL, rejectURL string
		if n.Tokens != nil {
			token := n.Tokens.Sign(approvalID, sub.userID, expiresAt)
			if token != "" {
				approveURL = fmt.Sprintf("%s/api/v1/approvals/%s/approve?action_token=%s", n.apiBaseURL, approvalID, token)
				rejectURL = fmt.Sprintf("%s/api/v1/approvals/%s/reject?action_token=%s", n.apiBaseURL, approvalID, token)
			}
		}
		n.Push.SendToSubscription(ctx, PushSubscription{
			Endpoint: sub.endpoint, P256dhKey: sub.p256dhKey, AuthKey: sub.authKey,
		}, PushPayload{
			Title: "Approval needed", Body: fmt.Sprintf("(%s) %s", riskLevel, summary),
			ApprovalID: approvalID.String(), ApproveURL: approveURL, RejectURL: rejectURL,
		})
	}
}

func summarizeToolCalls(calls []llm.ToolCall) string {
	if len(calls) == 0 {
		return "an action"
	}
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Name
	}
	if len(names) == 1 {
		return names[0]
	}
	return fmt.Sprintf("%d actions (%v)", len(names), names)
}

func lookupSlackChannel(ctx context.Context, appPool *pgxpool.Pool, orgID pgtype.UUID) (string, error) {
	var channel *string
	err := tenant.WithTx(ctx, appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		channel, err = q.GetOrgApprovalsSlackChannel(ctx, orgID)
		return err
	})
	if err != nil || channel == nil {
		return "", err
	}
	return *channel, nil
}

type approverEmail struct{ email string }

func listApproverEmails(ctx context.Context, appPool *pgxpool.Pool, orgID pgtype.UUID) ([]approverEmail, error) {
	var rows []dbgen.ListApproverEmailsForOrgRow
	err := tenant.WithTx(ctx, appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListApproverEmailsForOrg(ctx, orgID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]approverEmail, len(rows))
	for i, r := range rows {
		out[i] = approverEmail{email: r.Email}
	}
	return out, nil
}

type pushSub struct {
	userID                       uuid.UUID
	endpoint, p256dhKey, authKey string
}

func listPushSubscriptions(ctx context.Context, appPool *pgxpool.Pool, orgID pgtype.UUID) ([]pushSub, error) {
	var rows []dbgen.ListPushSubscriptionsForOrgRow
	err := tenant.WithTx(ctx, appPool, orgID, func(ctx context.Context, q *dbgen.Queries) error {
		var err error
		rows, err = q.ListPushSubscriptionsForOrg(ctx, orgID)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]pushSub, len(rows))
	for i, r := range rows {
		out[i] = pushSub{userID: r.UserID.Bytes, endpoint: r.Endpoint, p256dhKey: r.P256dhKey, authKey: r.AuthKey}
	}
	return out, nil
}
