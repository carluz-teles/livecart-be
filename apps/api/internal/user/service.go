package user

import (
	"context"

	"go.uber.org/zap"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("user"),
	}
}

// SyncUser creates/updates user and returns all memberships
// This is the main sync endpoint called on every login
func (s *Service) SyncUser(ctx context.Context, input SyncUserInput) (*SyncUserOutput, error) {
	// Upsert user in users table (creates if doesn't exist, updates if exists)
	user, err := s.repo.UpsertUser(ctx, input.ClerkUserID, input.Email, input.Name, input.AvatarURL)
	if err != nil {
		return nil, err
	}

	s.logger.Debug("user synced",
		zap.String("user_id", user.ID),
		zap.String("clerk_id", user.ClerkID),
		zap.String("email", user.Email),
	)

	// Get all memberships for this user
	memberships, err := s.repo.GetMembershipsByUserID(ctx, user.ID)
	if err != nil {
		return nil, err
	}

	// Convert to output format
	membershipOutputs := make([]MembershipOutput, len(memberships))
	for i, m := range memberships {
		membershipOutputs[i] = MembershipOutput{
			ID:             m.ID,
			StoreID:        m.StoreID,
			UserID:         m.UserID,
			StoreName:      m.StoreName,
			StoreSlug:      m.StoreSlug,
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
		UserID:              user.ID,
		ClerkUserID:         user.ClerkID,
		Email:               user.Email,
		Name:                user.Name,
		AvatarURL:           user.AvatarURL,
		Memberships:         membershipOutputs,
		LastAccessedStoreID: lastAccessedStoreID,
		State:               state,
	}, nil
}

// GetUserByClerkID returns a user by their Clerk ID
func (s *Service) GetUserByClerkID(ctx context.Context, clerkID string) (*UserInfo, error) {
	user, err := s.repo.GetUserByClerkID(ctx, clerkID)
	if err != nil {
		return nil, err
	}

	return &UserInfo{
		ID:        user.ID,
		ClerkID:   user.ClerkID,
		Email:     user.Email,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
		CreatedAt: user.CreatedAt,
		UpdatedAt: user.UpdatedAt,
	}, nil
}

// GetUserByEmail returns a user by their email
func (s *Service) GetUserByEmail(ctx context.Context, email string) (*UserInfo, error) {
	user, err := s.repo.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}

	return &UserInfo{
		ID:        user.ID,
		ClerkID:   user.ClerkID,
		Email:     user.Email,
		Name:      user.Name,
		AvatarURL: user.AvatarURL,
		CreatedAt: user.CreatedAt,
		UpdatedAt: user.UpdatedAt,
	}, nil
}

// GetMembership returns a specific membership for a user and store
func (s *Service) GetMembership(ctx context.Context, userID, storeID string) (*MembershipOutput, error) {
	m, err := s.repo.GetMembershipByUserIDAndStore(ctx, userID, storeID)
	if err != nil {
		return nil, err
	}

	return &MembershipOutput{
		ID:             m.ID,
		StoreID:        m.StoreID,
		UserID:         m.UserID,
		StoreName:      m.StoreName,
		StoreSlug:      m.StoreSlug,
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
func (s *Service) SelectStore(ctx context.Context, userID, storeID string) error {
	return s.repo.UpdateMembershipLastAccessed(ctx, userID, storeID)
}

// GetUserStores returns all memberships (stores) for a user
func (s *Service) GetUserStores(ctx context.Context, userID string) ([]MembershipOutput, error) {
	memberships, err := s.repo.GetMembershipsByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}

	outputs := make([]MembershipOutput, len(memberships))
	for i, m := range memberships {
		outputs[i] = MembershipOutput{
			ID:             m.ID,
			StoreID:        m.StoreID,
			UserID:         m.UserID,
			StoreName:      m.StoreName,
			StoreSlug:      m.StoreSlug,
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

// UpdateUser updates user info in the users table (for Clerk webhook)
func (s *Service) UpdateUser(ctx context.Context, clerkID, email, name, avatarURL string) error {
	_, err := s.repo.UpdateUser(ctx, clerkID, email, name, avatarURL)
	return err
}

// DeleteUser removes a user and all their memberships (typically called from Clerk webhook)
func (s *Service) DeleteUser(ctx context.Context, clerkID string) error {
	return s.repo.DeleteUser(ctx, clerkID)
}

// CreateOwnerMembership creates an owner membership for a new store
func (s *Service) CreateOwnerMembership(ctx context.Context, storeID, userID string) (*MembershipOutput, error) {
	m, err := s.repo.CreateOwnerMembership(ctx, storeID, userID)
	if err != nil {
		return nil, err
	}

	return &MembershipOutput{
		ID:        m.ID,
		StoreID:   m.StoreID,
		UserID:    m.UserID,
		Role:      m.Role,
		Status:    m.Status,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}, nil
}

// GetActiveStoreID returns the last accessed store ID for a user
// Returns empty string if user has no memberships
func (s *Service) GetActiveStoreID(ctx context.Context, userID string) (string, error) {
	memberships, err := s.repo.GetMembershipsByUserID(ctx, userID)
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
		UserID:    m.UserID,
		Role:      m.Role,
		Status:    m.Status,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}, nil
}

// GetUserIDByClerkID is a helper to get user's internal UUID from Clerk ID
func (s *Service) GetUserIDByClerkID(ctx context.Context, clerkID string) (string, error) {
	user, err := s.repo.GetUserByClerkID(ctx, clerkID)
	if err != nil {
		return "", err
	}
	return user.ID, nil
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
func (a *MembershipCreatorAdapter) CreateOwnerMembership(ctx context.Context, storeID, userID string) (string, error) {
	m, err := a.service.CreateOwnerMembership(ctx, storeID, userID)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

// UserLookupAdapter implements store.UserLookup and invitation.UserLookup interfaces
type UserLookupAdapter struct {
	service *Service
}

// NewUserLookupAdapter creates a new adapter for user lookup
func NewUserLookupAdapter(service *Service) *UserLookupAdapter {
	return &UserLookupAdapter{service: service}
}

// GetUserIDByClerkID implements store.UserLookup and invitation.UserLookup
func (a *UserLookupAdapter) GetUserIDByClerkID(ctx context.Context, clerkUserID string) (string, error) {
	return a.service.GetUserIDByClerkID(ctx, clerkUserID)
}

// GetUserByEmail implements invitation.UserLookup
func (a *UserLookupAdapter) GetUserByEmail(ctx context.Context, email string) (string, error) {
	user, err := a.service.GetUserByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	return user.ID, nil
}
