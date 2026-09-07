package org

import (
	"context"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
)

// MembershipSyncer is Handler's one external dependency, cut to the two
// calls it makes — same interface-segregation reasoning as
// internal/core/documents' BlobStore/Embedder/VectorIndex, so tests can
// inject a fake instead of calling Clerk's real API (which would mutate a
// real org's real membership data — not something a test run should ever
// risk doing).
type MembershipSyncer interface {
	// UpdateRole is best-effort from the caller's side: Clerk rejecting an
	// unrecognized role (this app's role vocabulary is finer-grained than
	// Clerk's own default org roles) must not fail the local role change —
	// Postgres stays this app's own source of truth for enforcement either
	// way. Handler.UpdateRole logs, not propagates, a non-nil error here.
	UpdateRole(ctx context.Context, clerkOrgID, clerkUserID, role string) error
	// Remove is NOT best-effort — Handler.Remove only soft-deactivates the
	// local row after this succeeds. See Handler.Remove's own doc comment
	// for the resurrection-via-future-webhook risk this ordering avoids.
	Remove(ctx context.Context, clerkOrgID, clerkUserID string) error
}

type clerkMembershipSyncer struct {
	client *organizationmembership.Client
}

// NewClerkMembershipSyncer adapts a real clerk-sdk-go client. Construct
// with organizationmembership.NewClient(&clerk.ClientConfig{}) — the zero
// value is deliberate: BackendConfig.Key nil falls back to whatever
// clerk.SetKey(cfg.ClerkSecretKey) already configured at boot.
func NewClerkMembershipSyncer(client *organizationmembership.Client) MembershipSyncer {
	return clerkMembershipSyncer{client: client}
}

func (s clerkMembershipSyncer) UpdateRole(ctx context.Context, clerkOrgID, clerkUserID, role string) error {
	_, err := s.client.Update(ctx, &organizationmembership.UpdateParams{
		OrganizationID: clerkOrgID, UserID: clerkUserID, Role: clerk.String(role),
	})
	return err
}

func (s clerkMembershipSyncer) Remove(ctx context.Context, clerkOrgID, clerkUserID string) error {
	_, err := s.client.Delete(ctx, &organizationmembership.DeleteParams{
		OrganizationID: clerkOrgID, UserID: clerkUserID,
	})
	return err
}
