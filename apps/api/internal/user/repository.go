package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
)

type Repository struct {
	q *sqlc.Queries
}

func NewRepository(q *sqlc.Queries) *Repository {
	return &Repository{q: q}
}

func (r *Repository) GetByClerkID(ctx context.Context, clerkUserID string) (*UserRow, error) {
	row, err := r.q.GetUserByClerkID(ctx, pgtype.Text{String: clerkUserID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("user not found")
		}
		return nil, fmt.Errorf("getting user by clerk id: %w", err)
	}

	return toUserRow(row), nil
}

func (r *Repository) GetUserStores(ctx context.Context, clerkUserID string) ([]UserStoreOutput, error) {
	rows, err := r.q.GetUserStores(ctx, pgtype.Text{String: clerkUserID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("getting user stores: %w", err)
	}

	stores := make([]UserStoreOutput, len(rows))
	for i, row := range rows {
		stores[i] = UserStoreOutput{
			ID:        row.ID.String(),
			StoreID:   row.StoreID.String(),
			Role:      row.Role,
			Status:    row.Status,
			StoreName: row.StoreName,
			StoreSlug: row.StoreSlug,
			CreatedAt: row.CreatedAt.Time,
		}
	}

	return stores, nil
}

func (r *Repository) CreateWithStore(ctx context.Context, params CreateUserWithStoreParams) (*UserRow, error) {
	row, err := r.q.CreateUserWithStore(ctx, sqlc.CreateUserWithStoreParams{
		Name:        params.StoreName,
		Slug:        params.StoreSlug,
		ClerkUserID: pgtype.Text{String: params.ClerkUserID, Valid: true},
		Email:       params.Email,
		Name_2:      pgtype.Text{String: params.Name, Valid: params.Name != ""},
		AvatarUrl:   pgtype.Text{String: params.AvatarURL, Valid: params.AvatarURL != ""},
	})
	if err != nil {
		return nil, fmt.Errorf("creating user with store: %w", err)
	}

	return toCreateUserRow(row), nil
}

func (r *Repository) Update(ctx context.Context, params UpdateUserParams) (*UserRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return nil, err
	}

	_, err = r.q.UpdateUser(ctx, sqlc.UpdateUserParams{
		ClerkUserID: pgtype.Text{String: params.ClerkUserID, Valid: true},
		StoreID:     storeUID,
		Email:       params.Email,
		Name:        pgtype.Text{String: params.Name, Valid: params.Name != ""},
		AvatarUrl:   pgtype.Text{String: params.AvatarURL, Valid: params.AvatarURL != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("user not found")
		}
		return nil, fmt.Errorf("updating user: %w", err)
	}

	// UpdateUser doesn't return store info, so we need to fetch it
	return r.GetByClerkID(ctx, params.ClerkUserID)
}

func (r *Repository) UpdateAllStores(ctx context.Context, params UpdateUserParams) error {
	err := r.q.UpdateUserAllStores(ctx, sqlc.UpdateUserAllStoresParams{
		ClerkUserID: pgtype.Text{String: params.ClerkUserID, Valid: true},
		Email:       params.Email,
		Name:        pgtype.Text{String: params.Name, Valid: params.Name != ""},
		AvatarUrl:   pgtype.Text{String: params.AvatarURL, Valid: params.AvatarURL != ""},
	})
	if err != nil {
		return fmt.Errorf("updating user in all stores: %w", err)
	}
	return nil
}

func (r *Repository) DeleteByClerkID(ctx context.Context, clerkUserID string) error {
	err := r.q.DeleteUserByClerkID(ctx, pgtype.Text{String: clerkUserID, Valid: true})
	if err != nil {
		return fmt.Errorf("deleting user by clerk id: %w", err)
	}
	return nil
}

// ValidateStoreAccess implements httpx.StoreAccessValidator
func (r *Repository) ValidateStoreAccess(ctx context.Context, clerkUserID, storeID string) (bool, error) {
	storeUserID, _, err := r.GetStoreAccessInfo(ctx, clerkUserID, storeID)
	if err != nil {
		return false, err
	}
	return storeUserID != "", nil
}

// GetStoreAccessInfo returns the user's store_user info for a specific store
func (r *Repository) GetStoreAccessInfo(ctx context.Context, clerkUserID, storeID string) (storeUserID string, role string, err error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return "", "", nil // Invalid UUID means no access
	}

	row, err := r.q.ValidateStoreAccess(ctx, sqlc.ValidateStoreAccessParams{
		ClerkUserID: pgtype.Text{String: clerkUserID, Valid: true},
		StoreID:     storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("validating store access: %w", err)
	}

	return row.ID.String(), row.Role, nil
}

func toUserRow(row sqlc.GetUserByClerkIDRow) *UserRow {
	var name, avatarURL *string
	if row.Name.Valid {
		name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		avatarURL = &row.AvatarUrl.String
	}

	return &UserRow{
		ID:                 row.ID.String(),
		StoreID:            row.StoreID.String(),
		Email:              row.Email,
		Name:               name,
		AvatarURL:          avatarURL,
		Role:               row.Role,
		Status:             row.Status,
		StoreName:          row.StoreName,
		StoreSlug:          row.StoreSlug,
		OnboardingComplete: row.OnboardingComplete,
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}
}

func toCreateUserRow(row sqlc.CreateUserWithStoreRow) *UserRow {
	var name, avatarURL *string
	if row.Name.Valid {
		name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		avatarURL = &row.AvatarUrl.String
	}

	return &UserRow{
		ID:                 row.ID.String(),
		StoreID:            row.StoreID.String(),
		Email:              row.Email,
		Name:               name,
		AvatarURL:          avatarURL,
		Role:               row.Role,
		Status:             row.Status,
		StoreName:          row.StoreName,
		StoreSlug:          row.StoreSlug,
		OnboardingComplete: row.OnboardingComplete,
		CreatedAt:          row.CreatedAt.Time,
		UpdatedAt:          row.UpdatedAt.Time,
	}
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}
