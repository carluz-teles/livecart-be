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
	_, err := r.q.UpdateUser(ctx, sqlc.UpdateUserParams{
		ClerkUserID: pgtype.Text{String: params.ClerkUserID, Valid: true},
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

func (r *Repository) DeleteByClerkID(ctx context.Context, clerkUserID string) error {
	err := r.q.DeleteUserByClerkID(ctx, pgtype.Text{String: clerkUserID, Valid: true})
	if err != nil {
		return fmt.Errorf("deleting user by clerk id: %w", err)
	}
	return nil
}

// ValidateStoreAccess implements httpx.StoreAccessValidator
func (r *Repository) ValidateStoreAccess(ctx context.Context, clerkUserID, storeID string) (bool, error) {
	row, err := r.q.GetUserByClerkID(ctx, pgtype.Text{String: clerkUserID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil // User doesn't exist
		}
		return false, fmt.Errorf("validating store access: %w", err)
	}
	// Check if user's store matches the requested store
	return row.StoreID.String() == storeID, nil
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
		ID:        row.ID.String(),
		StoreID:   row.StoreID.String(),
		Email:     row.Email,
		Name:      name,
		AvatarURL: avatarURL,
		Role:      row.Role,
		StoreName: row.StoreName,
		StoreSlug: row.StoreSlug,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
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
		ID:        row.ID.String(),
		StoreID:   row.StoreID.String(),
		Email:     row.Email,
		Name:      name,
		AvatarURL: avatarURL,
		Role:      row.Role,
		StoreName: row.StoreName,
		StoreSlug: row.StoreSlug,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
}
