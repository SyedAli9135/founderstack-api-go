package org

import (
	"context"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationinvitation"
)

// Invitation mirrors clerk.OrganizationInvitation, trimmed to what the
// team page needs — Postgres has no invitation table of its own (a
// pending invite doesn't become a `users` row until it's accepted and the
// Clerk webhook syncs it), so this list is read live from Clerk on every
// request rather than cached anywhere.
type Invitation struct {
	ID        string
	Email     string
	Role      string
	Status    string
	CreatedAt int64 // Clerk timestamps are milliseconds since epoch
	ExpiresAt *int64
}

// InvitationLister is Handler's second external dependency (alongside
// MembershipSyncer) — same interface-segregation/fake-for-tests reasoning:
// calling Clerk's real invitations API from a test would risk revoking or
// listing a real org's real pending invites.
type InvitationLister interface {
	List(ctx context.Context, clerkOrgID string) ([]Invitation, error)
	Revoke(ctx context.Context, clerkOrgID, invitationID, requestingClerkUserID string) error
}

type clerkInvitationLister struct {
	client *organizationinvitation.Client
}

// NewClerkInvitationLister adapts a real clerk-sdk-go client. Construct
// with organizationinvitation.NewClient(&clerk.ClientConfig{}) — same
// zero-value-falls-back-to-clerk.SetKey reasoning as
// NewClerkMembershipSyncer.
func NewClerkInvitationLister(client *organizationinvitation.Client) InvitationLister {
	return clerkInvitationLister{client: client}
}

// pendingStatus: the team page only ever needs to answer "did my invite
// actually go out, and is it still waiting" — accepted invitations are
// already visible as real members via List, and a revoked one has nothing
// actionable left to show.
var pendingStatus = []string{"pending"}

func (l clerkInvitationLister) List(ctx context.Context, clerkOrgID string) ([]Invitation, error) {
	resp, err := l.client.List(ctx, &organizationinvitation.ListParams{
		OrganizationID: clerkOrgID, Statuses: &pendingStatus,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Invitation, len(resp.OrganizationInvitations))
	for i, inv := range resp.OrganizationInvitations {
		out[i] = Invitation{
			ID: inv.ID, Email: inv.EmailAddress, Role: inv.Role, Status: inv.Status,
			CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt,
		}
	}
	return out, nil
}

func (l clerkInvitationLister) Revoke(ctx context.Context, clerkOrgID, invitationID, requestingClerkUserID string) error {
	_, err := l.client.Revoke(ctx, &organizationinvitation.RevokeParams{
		OrganizationID: clerkOrgID, ID: invitationID, RequestingUserID: clerk.String(requestingClerkUserID),
	})
	return err
}
