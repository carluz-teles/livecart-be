package user

import (
	"context"
	"fmt"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

// GetByClerkID returns a user by their Clerk ID
func (s *Service) GetByClerkID(ctx context.Context, clerkUserID string) (*UserOutput, error) {
	row, err := s.repo.GetByClerkID(ctx, clerkUserID)
	if err != nil {
		return nil, err
	}

	return toUserOutput(row), nil
}

// SyncUser creates a new user with store if not exists, or returns existing user
// This is the main entry point for user synchronization on first access
func (s *Service) SyncUser(ctx context.Context, input SyncUserInput) (*SyncUserOutput, error) {
	// Try to get existing user
	existing, err := s.repo.GetByClerkID(ctx, input.ClerkUserID)
	if err == nil {
		// User exists, return it
		return &SyncUserOutput{
			ID:        existing.ID,
			StoreID:   existing.StoreID,
			Email:     existing.Email,
			Name:      existing.Name,
			AvatarURL: existing.AvatarURL,
			Role:      existing.Role,
			StoreName: existing.StoreName,
			StoreSlug: existing.StoreSlug,
			CreatedAt: existing.CreatedAt,
			UpdatedAt: existing.UpdatedAt,
			IsNew:     false,
		}, nil
	}

	// If error is not "not found", return it
	if !httpx.IsNotFound(err) {
		return nil, fmt.Errorf("checking existing user: %w", err)
	}

	// User doesn't exist, create new user with store
	row, err := s.repo.CreateWithStore(ctx, CreateUserWithStoreParams{
		ClerkUserID: input.ClerkUserID,
		Email:       input.Email,
		Name:        input.Name,
		AvatarURL:   input.AvatarURL,
		StoreName:   input.StoreName,
		StoreSlug:   input.StoreSlug,
	})
	if err != nil {
		return nil, fmt.Errorf("creating user with store: %w", err)
	}

	return &SyncUserOutput{
		ID:        row.ID,
		StoreID:   row.StoreID,
		Email:     row.Email,
		Name:      row.Name,
		AvatarURL: row.AvatarURL,
		Role:      row.Role,
		StoreName: row.StoreName,
		StoreSlug: row.StoreSlug,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		IsNew:     true,
	}, nil
}

// UpdateUser updates user profile information (typically called from Clerk webhook)
func (s *Service) UpdateUser(ctx context.Context, input UpdateUserInput) (*UserOutput, error) {
	row, err := s.repo.Update(ctx, UpdateUserParams{
		ClerkUserID: input.ClerkUserID,
		Email:       input.Email,
		Name:        input.Name,
		AvatarURL:   input.AvatarURL,
	})
	if err != nil {
		return nil, err
	}

	return toUserOutput(row), nil
}

// DeleteUser removes a user by their Clerk ID (typically called from Clerk webhook)
func (s *Service) DeleteUser(ctx context.Context, clerkUserID string) error {
	return s.repo.DeleteByClerkID(ctx, clerkUserID)
}

func toUserOutput(row *UserRow) *UserOutput {
	return &UserOutput{
		ID:        row.ID,
		StoreID:   row.StoreID,
		Email:     row.Email,
		Name:      row.Name,
		AvatarURL: row.AvatarURL,
		Role:      row.Role,
		StoreName: row.StoreName,
		StoreSlug: row.StoreSlug,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}
