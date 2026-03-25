package member

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/member/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Repository struct {
	q *sqlc.Queries
}

func NewRepository(q *sqlc.Queries) *Repository {
	return &Repository{q: q}
}

func (r *Repository) List(ctx context.Context, storeID string) ([]*domain.Member, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.GetStoreMembers(ctx, storeUID)
	if err != nil {
		return nil, fmt.Errorf("listing store members: %w", err)
	}

	members := make([]*domain.Member, len(rows))
	for i, row := range rows {
		members[i], err = r.toDomainMember(row)
		if err != nil {
			return nil, fmt.Errorf("converting member row: %w", err)
		}
	}

	return members, nil
}

func (r *Repository) GetByID(ctx context.Context, storeID, memberID string) (*domain.Member, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	memberUID, err := parseUUID(memberID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetMembershipByID(ctx, memberUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("member not found")
		}
		return nil, fmt.Errorf("getting member: %w", err)
	}

	// Verify the member belongs to the store
	if row.StoreID != storeUID {
		return nil, httpx.ErrNotFound("member not found")
	}

	return r.toDomainMemberFromGetByID(row)
}

func (r *Repository) UpdateRole(ctx context.Context, storeID, memberID, role string) (*domain.Member, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	memberUID, err := parseUUID(memberID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.UpdateMembershipRole(ctx, sqlc.UpdateMembershipRoleParams{
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

	return r.toDomainMemberFromUpdate(row)
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

	err = r.q.DeleteMembership(ctx, sqlc.DeleteMembershipParams{
		StoreID: storeUID,
		ID:      memberUID,
	})
	if err != nil {
		return fmt.Errorf("removing member: %w", err)
	}

	return nil
}

// toDomainMember converts a SQLC row to a domain Member.
func (r *Repository) toDomainMember(row sqlc.GetStoreMembersRow) (*domain.Member, error) {
	id, err := vo.NewMemberID(row.ID.String())
	if err != nil {
		return nil, err
	}

	storeID, err := vo.NewStoreID(row.StoreID.String())
	if err != nil {
		return nil, err
	}

	userID := row.UserID.String()

	email, err := vo.NewEmail(row.Email)
	if err != nil {
		return nil, err
	}

	role, err := vo.NewRole(row.Role)
	if err != nil {
		return nil, err
	}

	status, err := domain.NewMemberStatus(row.Status)
	if err != nil {
		return nil, err
	}

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

	var joinedAt *time.Time
	if row.CreatedAt.Valid {
		joinedAt = &row.CreatedAt.Time
	}

	return domain.Reconstruct(
		id,
		storeID,
		userID,
		email,
		name,
		avatarURL,
		role,
		status,
		nil, // invitedBy not available in this query
		joinedAt,
		invitedAt,
	), nil
}

// toDomainMemberFromUpdate converts an UpdateMembershipRole row to a domain Member.
func (r *Repository) toDomainMemberFromUpdate(row sqlc.UpdateMembershipRoleRow) (*domain.Member, error) {
	id, err := vo.NewMemberID(row.ID.String())
	if err != nil {
		return nil, err
	}

	storeID, err := vo.NewStoreID(row.StoreID.String())
	if err != nil {
		return nil, err
	}

	userID := row.UserID.String()

	// We don't have email in the update response, use empty for now
	email, _ := vo.NewEmail("unknown@placeholder.com")

	role, err := vo.NewRole(row.Role)
	if err != nil {
		return nil, err
	}

	status, err := domain.NewMemberStatus(row.Status)
	if err != nil {
		return nil, err
	}

	var joinedAt *time.Time
	if row.CreatedAt.Valid {
		joinedAt = &row.CreatedAt.Time
	}

	return domain.Reconstruct(
		id,
		storeID,
		userID,
		email,
		nil,
		nil,
		role,
		status,
		nil,
		joinedAt,
		nil,
	), nil
}

// toDomainMemberFromGetByID converts a GetMembershipByID row to a domain Member.
func (r *Repository) toDomainMemberFromGetByID(row sqlc.GetMembershipByIDRow) (*domain.Member, error) {
	id, err := vo.NewMemberID(row.ID.String())
	if err != nil {
		return nil, err
	}

	storeID, err := vo.NewStoreID(row.StoreID.String())
	if err != nil {
		return nil, err
	}

	userID := row.UserID.String()

	email, err := vo.NewEmail(row.Email)
	if err != nil {
		return nil, err
	}

	role, err := vo.NewRole(row.Role)
	if err != nil {
		return nil, err
	}

	status, err := domain.NewMemberStatus(row.Status)
	if err != nil {
		return nil, err
	}

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

	var joinedAt *time.Time
	if row.CreatedAt.Valid {
		joinedAt = &row.CreatedAt.Time
	}

	return domain.Reconstruct(
		id,
		storeID,
		userID,
		email,
		name,
		avatarURL,
		role,
		status,
		nil, // invitedBy - not needed for this use case
		joinedAt,
		invitedAt,
	), nil
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}
