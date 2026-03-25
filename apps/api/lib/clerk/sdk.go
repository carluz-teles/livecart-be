package clerk

import (
	"context"
	"os"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"github.com/clerk/clerk-sdk-go/v2/organizationinvitation"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"
)

// SDK wraps the Clerk SDK for organization management
type SDK struct{}

// NewSDK creates a new Clerk SDK wrapper and initializes the API key
func NewSDK() *SDK {
	clerk.SetKey(os.Getenv("CLERK_SECRET_KEY"))
	return &SDK{}
}

// CreateOrganization creates a new organization in Clerk
// The creator automatically becomes an admin of the organization
func (s *SDK) CreateOrganization(ctx context.Context, name, slug, creatorUserID string) (*clerk.Organization, error) {
	return organization.Create(ctx, &organization.CreateParams{
		Name:      clerk.String(name),
		Slug:      clerk.String(slug),
		CreatedBy: clerk.String(creatorUserID),
	})
}

// UpdateOrganization updates an organization's name and slug
func (s *SDK) UpdateOrganization(ctx context.Context, orgID, name, slug string) (*clerk.Organization, error) {
	return organization.Update(ctx, orgID, &organization.UpdateParams{
		Name: clerk.String(name),
		Slug: clerk.String(slug),
	})
}

// GetOrganization retrieves an organization by ID
func (s *SDK) GetOrganization(ctx context.Context, orgID string) (*clerk.Organization, error) {
	return organization.Get(ctx, orgID)
}

// InviteMember sends an invitation to join the organization
// Clerk automatically sends the invitation email
func (s *SDK) InviteMember(ctx context.Context, orgID, email, role, inviterUserID, redirectURL string) (*clerk.OrganizationInvitation, error) {
	return organizationinvitation.Create(ctx, &organizationinvitation.CreateParams{
		OrganizationID: orgID,
		EmailAddress:   clerk.String(email),
		Role:           clerk.String(role), // "org:admin" or "org:member"
		InviterUserID:  clerk.String(inviterUserID),
		RedirectURL:    clerk.String(redirectURL),
	})
}

// ListInvitations lists all invitations for an organization
func (s *SDK) ListInvitations(ctx context.Context, orgID string) (*clerk.OrganizationInvitationList, error) {
	statuses := []string{"pending"}
	return organizationinvitation.List(ctx, &organizationinvitation.ListParams{
		OrganizationID: orgID,
		Statuses:       &statuses,
	})
}

// RevokeInvitation revokes a pending invitation
func (s *SDK) RevokeInvitation(ctx context.Context, orgID, invitationID string) (*clerk.OrganizationInvitation, error) {
	return organizationinvitation.Revoke(ctx, &organizationinvitation.RevokeParams{
		OrganizationID: orgID,
		ID:             invitationID,
	})
}

// ListMembers lists all members of an organization
func (s *SDK) ListMembers(ctx context.Context, orgID string) (*clerk.OrganizationMembershipList, error) {
	params := organizationmembership.ListParams{
		OrganizationID: orgID,
	}
	params.Limit = clerk.Int64(100)
	return organizationmembership.List(ctx, &params)
}

// UpdateMemberRole updates a member's role in the organization
func (s *SDK) UpdateMemberRole(ctx context.Context, orgID, userID, role string) (*clerk.OrganizationMembership, error) {
	return organizationmembership.Update(ctx, &organizationmembership.UpdateParams{
		OrganizationID: orgID,
		UserID:         userID,
		Role:           clerk.String(role), // "org:admin" or "org:member"
	})
}

// RemoveMember removes a member from the organization
func (s *SDK) RemoveMember(ctx context.Context, orgID, userID string) (*clerk.OrganizationMembership, error) {
	return organizationmembership.Delete(ctx, &organizationmembership.DeleteParams{
		OrganizationID: orgID,
		UserID:         userID,
	})
}

// GetUser retrieves user details by ID
func (s *SDK) GetUser(ctx context.Context, userID string) (*clerk.User, error) {
	return user.Get(ctx, userID)
}
