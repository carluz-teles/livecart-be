package user

import (
	"context"

	"go.uber.org/zap"

	"livecart/apps/api/lib/clerk"
)

type Service struct {
	repo     *Repository
	clerkSDK *clerk.SDK
	logger   *zap.Logger
}

func NewService(repo *Repository, clerkSDK *clerk.SDK, logger *zap.Logger) *Service {
	return &Service{
		repo:     repo,
		clerkSDK: clerkSDK,
		logger:   logger.Named("user"),
	}
}

// SyncUser returns all memberships for a clerk user
// Does NOT create store automatically - user must go through onboarding
func (s *Service) SyncUser(ctx context.Context, input SyncUserInput) (*SyncUserOutput, error) {
	// Get all memberships for this clerk user
	memberships, err := s.repo.GetMembershipsByClerkID(ctx, input.ClerkUserID)
	if err != nil {
		return nil, err
	}

	// Convert to output format
	membershipOutputs := make([]MembershipOutput, len(memberships))
	for i, m := range memberships {
		membershipOutputs[i] = MembershipOutput{
			ID:             m.ID,
			StoreID:        m.StoreID,
			StoreName:      m.StoreName,
			StoreSlug:      m.StoreSlug,
			ClerkOrgID:     m.ClerkOrgID,
			Role:           m.Role,
			Status:         m.Status,
			Email:          m.Email,
			Name:           m.Name,
			AvatarURL:      m.AvatarURL,
			LastAccessedAt: m.LastAccessedAt,
			CreatedAt:      m.CreatedAt,
			UpdatedAt:      m.UpdatedAt,
		}
	}

	// Determine state and last accessed store
	state := "no_store"
	var lastAccessedStoreID *string

	if len(memberships) > 0 {
		state = "ready"
		// First membership is ordered by last_accessed_at DESC, so it's the most recent
		lastAccessedStoreID = &memberships[0].StoreID
	}

	return &SyncUserOutput{
		ClerkUserID:         input.ClerkUserID,
		Memberships:         membershipOutputs,
		LastAccessedStoreID: lastAccessedStoreID,
		State:               state,
	}, nil
}

// GetMembership returns a specific membership for a clerk user and store
func (s *Service) GetMembership(ctx context.Context, clerkUserID, storeID string) (*MembershipOutput, error) {
	m, err := s.repo.GetMembershipByClerkIDAndStore(ctx, clerkUserID, storeID)
	if err != nil {
		return nil, err
	}

	return &MembershipOutput{
		ID:             m.ID,
		StoreID:        m.StoreID,
		StoreName:      m.StoreName,
		StoreSlug:      m.StoreSlug,
		ClerkOrgID:     m.ClerkOrgID,
		Role:           m.Role,
		Status:         m.Status,
		Email:          m.Email,
		Name:           m.Name,
		AvatarURL:      m.AvatarURL,
		LastAccessedAt: m.LastAccessedAt,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}, nil
}

// SelectStore updates the last accessed store for a user
func (s *Service) SelectStore(ctx context.Context, clerkUserID, storeID string) error {
	return s.repo.UpdateMembershipLastAccessed(ctx, clerkUserID, storeID)
}

// GetUserStores returns all memberships (stores) for a clerk user
func (s *Service) GetUserStores(ctx context.Context, clerkUserID string) ([]MembershipOutput, error) {
	memberships, err := s.repo.GetMembershipsByClerkID(ctx, clerkUserID)
	if err != nil {
		return nil, err
	}

	outputs := make([]MembershipOutput, len(memberships))
	for i, m := range memberships {
		outputs[i] = MembershipOutput{
			ID:             m.ID,
			StoreID:        m.StoreID,
			StoreName:      m.StoreName,
			StoreSlug:      m.StoreSlug,
			ClerkOrgID:     m.ClerkOrgID,
			Role:           m.Role,
			Status:         m.Status,
			Email:          m.Email,
			Name:           m.Name,
			AvatarURL:      m.AvatarURL,
			LastAccessedAt: m.LastAccessedAt,
			CreatedAt:      m.CreatedAt,
			UpdatedAt:      m.UpdatedAt,
		}
	}

	return outputs, nil
}

// UpdateUserAllStores updates user info across all memberships (for Clerk webhook)
func (s *Service) UpdateUserAllStores(ctx context.Context, clerkUserID, email, name, avatarURL string) error {
	return s.repo.UpdateMembershipAllStores(ctx, clerkUserID, email, name, avatarURL)
}

// DeleteUser removes all memberships for a clerk user (typically called from Clerk webhook)
func (s *Service) DeleteUser(ctx context.Context, clerkUserID string) error {
	return s.repo.DeleteMembershipsByClerkID(ctx, clerkUserID)
}

// CreateOwnerMembership creates an owner membership for a new store
func (s *Service) CreateOwnerMembership(ctx context.Context, storeID, clerkUserID, email, name, avatarURL string) (*MembershipOutput, error) {
	m, err := s.repo.CreateOwnerMembership(ctx, storeID, clerkUserID, email, name, avatarURL)
	if err != nil {
		return nil, err
	}

	return &MembershipOutput{
		ID:        m.ID,
		StoreID:   m.StoreID,
		Role:      m.Role,
		Status:    m.Status,
		Email:     m.Email,
		Name:      m.Name,
		AvatarURL: m.AvatarURL,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}, nil
}

// GetActiveStoreID returns the last accessed store ID for a user
// Returns empty string if user has no memberships
func (s *Service) GetActiveStoreID(ctx context.Context, clerkUserID string) (string, error) {
	memberships, err := s.repo.GetMembershipsByClerkID(ctx, clerkUserID)
	if err != nil {
		return "", err
	}
	if len(memberships) == 0 {
		return "", nil
	}
	// First membership is the most recently accessed
	return memberships[0].StoreID, nil
}

// CreateMembership creates a new membership (for accepting invitations)
func (s *Service) CreateMembership(ctx context.Context, params CreateMembershipParams) (*MembershipOutput, error) {
	m, err := s.repo.CreateMembership(ctx, params)
	if err != nil {
		return nil, err
	}

	return &MembershipOutput{
		ID:        m.ID,
		StoreID:   m.StoreID,
		Role:      m.Role,
		Status:    m.Status,
		Email:     m.Email,
		Name:      m.Name,
		AvatarURL: m.AvatarURL,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}, nil
}

// MembershipCreatorAdapter implements store.MembershipCreator interface
type MembershipCreatorAdapter struct {
	service *Service
}

// NewMembershipCreatorAdapter creates a new adapter for the store service to use
func NewMembershipCreatorAdapter(service *Service) *MembershipCreatorAdapter {
	return &MembershipCreatorAdapter{service: service}
}

// CreateOwnerMembership implements store.MembershipCreator
func (a *MembershipCreatorAdapter) CreateOwnerMembership(ctx context.Context, storeID, clerkUserID, email, name, avatarURL string) (string, error) {
	m, err := a.service.CreateOwnerMembership(ctx, storeID, clerkUserID, email, name, avatarURL)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}
