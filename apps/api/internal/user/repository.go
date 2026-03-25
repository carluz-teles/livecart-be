package user

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// GetMembershipsByClerkID returns all memberships for a clerk user
func (r *Repository) GetMembershipsByClerkID(ctx context.Context, clerkUserID string) ([]MembershipRow, error) {
	rows, err := r.q.GetMembershipsByClerkID(ctx, pgtype.Text{String: clerkUserID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("getting memberships: %w", err)
	}

	memberships := make([]MembershipRow, len(rows))
	for i, row := range rows {
		memberships[i] = toMembershipRowFromList(row)
	}
	return memberships, nil
}

// GetMembershipByClerkIDAndStore returns a specific membership
func (r *Repository) GetMembershipByClerkIDAndStore(ctx context.Context, clerkUserID, storeID string) (*MembershipRow, error) {
	uid, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetMembershipByClerkIDAndStore(ctx, sqlc.GetMembershipByClerkIDAndStoreParams{
		ClerkUserID: pgtype.Text{String: clerkUserID, Valid: true},
		StoreID:     uid,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("membership not found")
		}
		return nil, fmt.Errorf("getting membership: %w", err)
	}

	out := toMembershipRowFromSingle(row)
	return &out, nil
}

// CreateMembership creates a new membership
func (r *Repository) CreateMembership(ctx context.Context, params CreateMembershipParams) (*MembershipRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return nil, err
	}

	var invitedBy pgtype.UUID
	var invitedAt pgtype.Timestamptz
	if params.InvitedBy != nil {
		inviterUID, err := parseUUID(*params.InvitedBy)
		if err != nil {
			return nil, err
		}
		invitedBy = inviterUID
		invitedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}

	row, err := r.q.CreateMembership(ctx, sqlc.CreateMembershipParams{
		StoreID:     storeUID,
		ClerkUserID: pgtype.Text{String: params.ClerkUserID, Valid: true},
		Email:       params.Email,
		Name:        pgtype.Text{String: params.Name, Valid: params.Name != ""},
		AvatarUrl:   pgtype.Text{String: params.AvatarURL, Valid: params.AvatarURL != ""},
		Role:        params.Role,
		InvitedBy:   invitedBy,
		InvitedAt:   invitedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("creating membership: %w", err)
	}

	out := MembershipRow{
		ID:          row.ID.String(),
		StoreID:     row.StoreID.String(),
		ClerkUserID: row.ClerkUserID.String,
		Email:       row.Email,
		Role:        row.Role,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
	if row.Name.Valid {
		out.Name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		out.AvatarURL = &row.AvatarUrl.String
	}
	if row.LastAccessedAt.Valid {
		out.LastAccessedAt = &row.LastAccessedAt.Time
	}
	return &out, nil
}

// CreateOwnerMembership creates an owner membership for a new store
func (r *Repository) CreateOwnerMembership(ctx context.Context, storeID, clerkUserID, email, name, avatarURL string) (*MembershipRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.CreateOwnerMembership(ctx, sqlc.CreateOwnerMembershipParams{
		StoreID:     storeUID,
		ClerkUserID: pgtype.Text{String: clerkUserID, Valid: true},
		Email:       email,
		Name:        pgtype.Text{String: name, Valid: name != ""},
		AvatarUrl:   pgtype.Text{String: avatarURL, Valid: avatarURL != ""},
	})
	if err != nil {
		return nil, fmt.Errorf("creating owner membership: %w", err)
	}

	out := MembershipRow{
		ID:          row.ID.String(),
		StoreID:     row.StoreID.String(),
		ClerkUserID: row.ClerkUserID.String,
		Email:       row.Email,
		Role:        row.Role,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
	if row.Name.Valid {
		out.Name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		out.AvatarURL = &row.AvatarUrl.String
	}
	return &out, nil
}

// UpdateMembershipLastAccessed updates the last accessed timestamp
func (r *Repository) UpdateMembershipLastAccessed(ctx context.Context, clerkUserID, storeID string) error {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	return r.q.UpdateMembershipLastAccessed(ctx, sqlc.UpdateMembershipLastAccessedParams{
		ClerkUserID: pgtype.Text{String: clerkUserID, Valid: true},
		StoreID:     storeUID,
	})
}

// UpdateMembership updates user info for a specific membership
func (r *Repository) UpdateMembership(ctx context.Context, clerkUserID, storeID, email, name, avatarURL string) (*MembershipRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.UpdateMembership(ctx, sqlc.UpdateMembershipParams{
		ClerkUserID: pgtype.Text{String: clerkUserID, Valid: true},
		StoreID:     storeUID,
		Email:       email,
		Name:        pgtype.Text{String: name, Valid: name != ""},
		AvatarUrl:   pgtype.Text{String: avatarURL, Valid: avatarURL != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("membership not found")
		}
		return nil, fmt.Errorf("updating membership: %w", err)
	}

	out := MembershipRow{
		ID:          row.ID.String(),
		StoreID:     row.StoreID.String(),
		ClerkUserID: row.ClerkUserID.String,
		Email:       row.Email,
		Role:        row.Role,
		Status:      row.Status,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
	if row.Name.Valid {
		out.Name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		out.AvatarURL = &row.AvatarUrl.String
	}
	if row.LastAccessedAt.Valid {
		out.LastAccessedAt = &row.LastAccessedAt.Time
	}
	return &out, nil
}

// UpdateMembershipAllStores updates user info across all memberships (for Clerk webhook)
func (r *Repository) UpdateMembershipAllStores(ctx context.Context, clerkUserID, email, name, avatarURL string) error {
	return r.q.UpdateMembershipAllStores(ctx, sqlc.UpdateMembershipAllStoresParams{
		ClerkUserID: pgtype.Text{String: clerkUserID, Valid: true},
		Email:       email,
		Name:        pgtype.Text{String: name, Valid: name != ""},
		AvatarUrl:   pgtype.Text{String: avatarURL, Valid: avatarURL != ""},
	})
}

// DeleteMembershipsByClerkID deletes all memberships for a clerk user
func (r *Repository) DeleteMembershipsByClerkID(ctx context.Context, clerkUserID string) error {
	return r.q.DeleteMembershipsByClerkID(ctx, pgtype.Text{String: clerkUserID, Valid: true})
}

// ValidateStoreAccess implements httpx.StoreAccessValidator
func (r *Repository) ValidateStoreAccess(ctx context.Context, clerkUserID, storeID string) (bool, error) {
	membershipID, _, err := r.GetStoreAccessInfo(ctx, clerkUserID, storeID)
	if err != nil {
		return false, err
	}
	return membershipID != "", nil
}

// GetStoreAccessInfo returns the user's membership info for a specific store
func (r *Repository) GetStoreAccessInfo(ctx context.Context, clerkUserID, storeID string) (membershipID string, role string, err error) {
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

// Helper to convert sqlc row to MembershipRow (from list query)
func toMembershipRowFromList(row sqlc.GetMembershipsByClerkIDRow) MembershipRow {
	out := MembershipRow{
		ID:          row.ID.String(),
		StoreID:     row.StoreID.String(),
		ClerkUserID: row.ClerkUserID.String,
		Email:       row.Email,
		Role:        row.Role,
		Status:      row.Status,
		StoreName:   row.StoreName,
		StoreSlug:   row.StoreSlug,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
	if row.Name.Valid {
		out.Name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		out.AvatarURL = &row.AvatarUrl.String
	}
	if row.ClerkOrgID.Valid {
		out.ClerkOrgID = row.ClerkOrgID.String
	}
	if row.LastAccessedAt.Valid {
		out.LastAccessedAt = &row.LastAccessedAt.Time
	}
	return out
}

// Helper to convert sqlc row to MembershipRow (from single query)
func toMembershipRowFromSingle(row sqlc.GetMembershipByClerkIDAndStoreRow) MembershipRow {
	out := MembershipRow{
		ID:          row.ID.String(),
		StoreID:     row.StoreID.String(),
		ClerkUserID: row.ClerkUserID.String,
		Email:       row.Email,
		Role:        row.Role,
		Status:      row.Status,
		StoreName:   row.StoreName,
		StoreSlug:   row.StoreSlug,
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}
	if row.Name.Valid {
		out.Name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		out.AvatarURL = &row.AvatarUrl.String
	}
	if row.ClerkOrgID.Valid {
		out.ClerkOrgID = row.ClerkOrgID.String
	}
	if row.LastAccessedAt.Valid {
		out.LastAccessedAt = &row.LastAccessedAt.Time
	}
	return out
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}
