package member

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

func (r *Repository) List(ctx context.Context, storeID string) ([]MemberRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.GetStoreMembers(ctx, storeUID)
	if err != nil {
		return nil, fmt.Errorf("listing store members: %w", err)
	}

	result := make([]MemberRow, len(rows))
	for i, row := range rows {
		var name, avatarURL *string
		if row.Name.Valid {
			name = &row.Name.String
		}
		if row.AvatarUrl.Valid {
			avatarURL = &row.AvatarUrl.String
		}
		var invitedAt *time.Time
		if row.InvitedAt.Valid {
			invitedAt = &row.InvitedAt.Time
		}

		result[i] = MemberRow{
			ID:        row.ID.String(),
			StoreID:   row.StoreID.String(),
			Email:     row.Email,
			Name:      name,
			AvatarURL: avatarURL,
			Role:      row.Role,
			Status:    row.Status,
			JoinedAt:  row.CreatedAt.Time,
			InvitedAt: invitedAt,
		}
	}

	return result, nil
}

func (r *Repository) UpdateRole(ctx context.Context, storeID, memberID, role string) (*MemberRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	memberUID, err := parseUUID(memberID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.UpdateUserRole(ctx, sqlc.UpdateUserRoleParams{
		StoreID: storeUID,
		ID:      memberUID,
		Role:    role,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("member not found")
		}
		return nil, fmt.Errorf("updating member role: %w", err)
	}

	var name, avatarURL *string
	if row.Name.Valid {
		name = &row.Name.String
	}
	if row.AvatarUrl.Valid {
		avatarURL = &row.AvatarUrl.String
	}

	return &MemberRow{
		ID:        row.ID.String(),
		StoreID:   row.StoreID.String(),
		Email:     row.Email,
		Name:      name,
		AvatarURL: avatarURL,
		Role:      row.Role,
		Status:    row.Status,
		JoinedAt:  row.CreatedAt.Time,
	}, nil
}

func (r *Repository) Remove(ctx context.Context, storeID, memberID string) error {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	memberUID, err := parseUUID(memberID)
	if err != nil {
		return err
	}

	err = r.q.RemoveUserFromStore(ctx, sqlc.RemoveUserFromStoreParams{
		StoreID: storeUID,
		ID:      memberUID,
	})
	if err != nil {
		return fmt.Errorf("removing member: %w", err)
	}

	return nil
}

func (r *Repository) GetByID(ctx context.Context, storeID, memberID string) (*MemberRow, error) {
	members, err := r.List(ctx, storeID)
	if err != nil {
		return nil, err
	}

	for _, m := range members {
		if m.ID == memberID {
			return &m, nil
		}
	}

	return nil, httpx.ErrNotFound("member not found")
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}
