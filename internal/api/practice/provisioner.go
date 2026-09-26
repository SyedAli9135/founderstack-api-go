package practice

import (
	"context"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
)

// WorkspaceProvisioner is Handler's one external dependency, cut to the two
// Clerk calls it makes so tests inject a fake instead of creating real
// organizations in a real Clerk instance.
type WorkspaceProvisioner interface {
	Create(ctx context.Context, name, createdBy string) (string, error)
	Delete(ctx context.Context, clerkOrgID string) error
}

type clerkProvisioner struct {
	client *organization.Client
}

// NewClerkProvisioner adapts a real clerk-sdk-go client. Construct with
// organization.NewClient(&clerk.ClientConfig{}) — the nil Key falls back to
// the clerk.SetKey already configured at boot.
func NewClerkProvisioner(client *organization.Client) WorkspaceProvisioner {
	return clerkProvisioner{client: client}
}

func (p clerkProvisioner) Create(ctx context.Context, name, createdBy string) (string, error) {
	org, err := p.client.Create(ctx, &organization.CreateParams{
		Name: clerk.String(name), CreatedBy: clerk.String(createdBy),
	})
	if err != nil {
		return "", err
	}
	return org.ID, nil
}

func (p clerkProvisioner) Delete(ctx context.Context, clerkOrgID string) error {
	_, err := p.client.Delete(ctx, clerkOrgID)
	return err
}
