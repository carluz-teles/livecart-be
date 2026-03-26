package httpx

import "context"

// MembershipData provides membership information for cross-service lookups
// Used by invitation service to check user's current membership
type MembershipData interface {
	GetID() string
	GetStoreID() string
	GetStoreName() string
	GetRole() string
}

// MembershipLookup interface for looking up user memberships
// Used by invitation service to enforce 1 user = 1 store rule
type MembershipLookup interface {
	// GetMembershipByUserID returns nil if user has no membership
	GetMembershipByUserID(ctx context.Context, userID string) (MembershipData, error)
	DeleteMembershipByUserID(ctx context.Context, userID string) error
}
