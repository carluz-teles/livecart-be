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

// ============================================
// User operations
// ============================================

// UpsertUser creates or updates a user in the users table
func (r *Repository) UpsertUser(ctx context.Context, clerkID, email, name, avatarURL string) (*UserRow, error) {
	row, err := r.q.UpsertUser(ctx, sqlc.UpsertUserParams{
		ClerkID:   clerkID,
		Email:     email,
		Name:      pgtype.Text{String: name, Valid: name != ""},
		AvatarUrl: pgtype.Text{String: avatarURL, Valid: avatarURL != ""},
	})
	if err != nil {
		return nil, fmt.Errorf("upserting user: %w", err)
	}

	return toUserRow(row), nil
}

// GetUserByClerkID returns a user by their Clerk ID
func (r *Repository) GetUserByClerkID(ctx context.Context, clerkID string) (*UserRow, error) {
	row, err := r.q.GetUserByClerkID(ctx, clerkID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("user not found")
		}
		return nil, fmt.Errorf("getting user: %w", err)
	}

	return toUserRow(row), nil
}

// GetUserByEmail returns a user by their email
func (r *Repository) GetUserByEmail(ctx context.Context, email string) (*UserRow, error) {
	row, err := r.q.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("user not found")
		}
		return nil, fmt.Errorf("getting user: %w", err)
	}

	return toUserRow(row), nil
}

// GetUserByID returns a user by their UUID
func (r *Repository) GetUserByID(ctx context.Context, userID string) (*UserRow, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetUserByID(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("user not found")
		}
		return nil, fmt.Errorf("getting user: %w", err)
	}

	return toUserRow(row), nil
}

// UpdateUser updates a user's info
func (r *Repository) UpdateUser(ctx context.Context, clerkID, email, name, avatarURL string) (*UserRow, error) {
	row, err := r.q.UpdateUser(ctx, sqlc.UpdateUserParams{
		ClerkID:   clerkID,
		Email:     email,
		Name:      pgtype.Text{String: name, Valid: name != ""},
		AvatarUrl: pgtype.Text{String: avatarURL, Valid: avatarURL != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("user not found")
		}
		return nil, fmt.Errorf("updating user: %w", err)
	}

	return toUserRow(row), nil
}

// DeleteUser deletes a user by their Clerk ID (cascades to memberships)
func (r *Repository) DeleteUser(ctx context.Context, clerkID string) error {
	return r.q.DeleteUser(ctx, clerkID)
}

// ============================================
// Membership operations (Single store per user)
// ============================================

// GetMembershipByUserID returns the single membership for a user (1 user = 1 store)
func (r *Repository) GetMembershipByUserID(ctx context.Context, userID string) (*MembershipRow, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetMembershipByUserID(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // No membership found
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
	userUID, err := parseUUID(params.UserID)
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
		StoreID:   storeUID,
		UserID:    userUID,
		Role:      params.Role,
		InvitedBy: invitedBy,
		InvitedAt: invitedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("creating membership: %w", err)
	}

	out := MembershipRow{
		ID:        row.ID.String(),
		StoreID:   row.StoreID.String(),
		UserID:    row.UserID.String(),
		Role:      row.Role,
		Status:    row.Status,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	return &out, nil
}

// CreateOwnerMembership creates an owner membership for a new store
func (r *Repository) CreateOwnerMembership(ctx context.Context, storeID, userID string) (*MembershipRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	userUID, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.CreateOwnerMembership(ctx, sqlc.CreateOwnerMembershipParams{
		StoreID: storeUID,
		UserID:  userUID,
	})
	if err != nil {
		return nil, fmt.Errorf("creating owner membership: %w", err)
	}

	out := MembershipRow{
		ID:        row.ID.String(),
		StoreID:   row.StoreID.String(),
		UserID:    row.UserID.String(),
		Role:      row.Role,
		Status:    row.Status,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	return &out, nil
}

// DeleteMembership removes a membership
func (r *Repository) DeleteMembership(ctx context.Context, storeID, membershipID string) error {
	sid, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	mid, err := parseUUID(membershipID)
	if err != nil {
		return err
	}

	return r.q.DeleteMembership(ctx, sqlc.DeleteMembershipParams{
		StoreID: sid,
		ID:      mid,
	})
}

// ============================================
// Access validation
// ============================================

// ValidateStoreAccess implements httpx.StoreAccessValidator
func (r *Repository) ValidateStoreAccess(ctx context.Context, clerkUserID, storeID string) (bool, error) {
	membershipID, _, _, err := r.GetStoreAccessInfo(ctx, clerkUserID, storeID)
	if err != nil {
		return false, err
	}
	return membershipID != "", nil
}

// GetStoreAccessInfo returns the user's membership info for a specific store using Clerk ID
func (r *Repository) GetStoreAccessInfo(ctx context.Context, clerkUserID, storeID string) (membershipID string, role string, userID string, err error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return "", "", "", nil // Invalid UUID means no access
	}

	row, err := r.q.ValidateStoreAccessByClerkID(ctx, sqlc.ValidateStoreAccessByClerkIDParams{
		ClerkID: clerkUserID,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", "", nil
		}
		return "", "", "", fmt.Errorf("validating store access: %w", err)
	}

	return row.ID.String(), row.Role, row.UserID.String(), nil
}

// ============================================
// Helpers
// ============================================

func toUserRow(row sqlc.User) *UserRow {
	out := &UserRow{
		ID:        row.ID.String(),
		ClerkID:   row.ClerkID,
		Email:     row.Email,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	if row.Name.Valid {
		out.Name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		out.AvatarURL = &row.AvatarUrl.String
	}
	return out
}

// Helper to convert sqlc row to MembershipRow (single membership per user)
func toMembershipRowFromSingle(row sqlc.GetMembershipByUserIDRow) MembershipRow {
	out := MembershipRow{
		ID:        row.ID.String(),
		StoreID:   row.StoreID.String(),
		UserID:    row.UserID.String(),
		Email:     row.Email,
		Role:      row.Role,
		Status:    row.Status,
		StoreName: row.StoreName,
		StoreSlug: row.StoreSlug,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	if row.Name.Valid {
		out.Name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		out.AvatarURL = &row.AvatarUrl.String
	}
	if row.StoreLogoUrl.Valid {
		out.StoreLogoURL = &row.StoreLogoUrl.String
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
