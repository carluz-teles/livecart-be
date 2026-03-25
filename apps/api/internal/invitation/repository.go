package invitation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/invitation/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Repository struct {
	q *sqlc.Queries
}

func NewRepository(q *sqlc.Queries) *Repository {
	return &Repository{q: q}
}

func (r *Repository) Save(ctx context.Context, inv *domain.Invitation) error {
	_, err := r.q.CreateInvitation(ctx, sqlc.CreateInvitationParams{
		StoreID:   inv.StoreID().ToPgUUID(),
		Email:     inv.Email().String(),
		Role:      inv.Role().String(),
		Token:     inv.Token().String(),
		InvitedBy: inv.InvitedBy().ToPgUUID(),
		ExpiresAt: pgtype.Timestamptz{Time: inv.ExpiresAt(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("creating invitation: %w", err)
	}
	return nil
}

func (r *Repository) GetByToken(ctx context.Context, token string) (*domain.Invitation, error) {
	row, err := r.q.GetInvitationByToken(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("invitation not found")
		}
		return nil, fmt.Errorf("getting invitation by token: %w", err)
	}

	return r.toDomainInvitationFull(row)
}

func (r *Repository) GetByEmail(ctx context.Context, storeID vo.StoreID, email vo.Email) (*domain.Invitation, error) {
	row, err := r.q.GetInvitationByEmail(ctx, sqlc.GetInvitationByEmailParams{
		StoreID: storeID.ToPgUUID(),
		Email:   email.String(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("invitation not found")
		}
		return nil, fmt.Errorf("getting invitation by email: %w", err)
	}

	return r.toDomainInvitationBasic(row)
}

func (r *Repository) GetByID(ctx context.Context, storeID vo.StoreID, id vo.InvitationID) (*domain.Invitation, error) {
	invitations, err := r.ListByStore(ctx, storeID)
	if err != nil {
		return nil, err
	}

	for _, inv := range invitations {
		if inv.ID().Equals(id) {
			return inv, nil
		}
	}

	return nil, httpx.ErrNotFound("invitation not found")
}

func (r *Repository) ListByStore(ctx context.Context, storeID vo.StoreID) ([]*domain.Invitation, error) {
	rows, err := r.q.ListStoreInvitations(ctx, storeID.ToPgUUID())
	if err != nil {
		return nil, fmt.Errorf("listing invitations: %w", err)
	}

	result := make([]*domain.Invitation, len(rows))
	for i, row := range rows {
		result[i], err = r.toDomainInvitationList(row)
		if err != nil {
			return nil, fmt.Errorf("converting invitation row: %w", err)
		}
	}

	return result, nil
}

func (r *Repository) Accept(ctx context.Context, id vo.InvitationID) error {
	_, err := r.q.AcceptInvitation(ctx, id.ToPgUUID())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound("invitation not found or already accepted")
		}
		return fmt.Errorf("accepting invitation: %w", err)
	}
	return nil
}

func (r *Repository) Revoke(ctx context.Context, storeID vo.StoreID, id vo.InvitationID) error {
	err := r.q.RevokeInvitation(ctx, sqlc.RevokeInvitationParams{
		StoreID: storeID.ToPgUUID(),
		ID:      id.ToPgUUID(),
	})
	if err != nil {
		return fmt.Errorf("revoking invitation: %w", err)
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, storeID vo.StoreID, id vo.InvitationID) error {
	err := r.q.DeleteInvitation(ctx, sqlc.DeleteInvitationParams{
		StoreID: storeID.ToPgUUID(),
		ID:      id.ToPgUUID(),
	})
	if err != nil {
		return fmt.Errorf("deleting invitation: %w", err)
	}
	return nil
}

func (r *Repository) AddUserToStore(ctx context.Context, storeID vo.StoreID, userID string, role vo.Role, invitedBy vo.MemberID) error {
	userUID, err := parseUUID(userID)
	if err != nil {
		return err
	}

	_, err = r.q.CreateMembership(ctx, sqlc.CreateMembershipParams{
		StoreID:   storeID.ToPgUUID(),
		UserID:    userUID,
		Role:      role.String(),
		InvitedBy: invitedBy.ToPgUUID(),
		InvitedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("adding user to store: %w", err)
	}
	return nil
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}

// toDomainInvitationFull converts GetInvitationByToken row to domain Invitation.
func (r *Repository) toDomainInvitationFull(row sqlc.GetInvitationByTokenRow) (*domain.Invitation, error) {
	id, err := vo.NewInvitationID(row.ID.String())
	if err != nil {
		return nil, err
	}

	storeID, err := vo.NewStoreID(row.StoreID.String())
	if err != nil {
		return nil, err
	}

	email, err := vo.NewEmail(row.Email)
	if err != nil {
		return nil, err
	}

	role, err := vo.NewRole(row.Role)
	if err != nil {
		return nil, err
	}

	token, err := domain.NewInvitationToken(row.Token)
	if err != nil {
		return nil, err
	}

	status, err := domain.NewInvitationStatus(row.Status)
	if err != nil {
		return nil, err
	}

	invitedBy, err := vo.NewMemberID(row.InvitedBy.String())
	if err != nil {
		return nil, err
	}

	var inviterName *string
	if row.InviterName.Valid {
		inviterName = &row.InviterName.String
	}

	return domain.Reconstruct(
		id,
		storeID,
		email,
		role,
		token,
		status,
		invitedBy,
		inviterName,
		row.StoreName,
		row.StoreSlug,
		row.ExpiresAt.Time,
		nil,
		row.CreatedAt.Time,
	), nil
}

// toDomainInvitationBasic converts StoreInvitation to domain Invitation.
func (r *Repository) toDomainInvitationBasic(row sqlc.StoreInvitation) (*domain.Invitation, error) {
	id, err := vo.NewInvitationID(row.ID.String())
	if err != nil {
		return nil, err
	}

	storeID, err := vo.NewStoreID(row.StoreID.String())
	if err != nil {
		return nil, err
	}

	email, err := vo.NewEmail(row.Email)
	if err != nil {
		return nil, err
	}

	role, err := vo.NewRole(row.Role)
	if err != nil {
		return nil, err
	}

	token, err := domain.NewInvitationToken(row.Token)
	if err != nil {
		return nil, err
	}

	status, err := domain.NewInvitationStatus(row.Status)
	if err != nil {
		return nil, err
	}

	invitedBy, err := vo.NewMemberID(row.InvitedBy.String())
	if err != nil {
		return nil, err
	}

	var acceptedAt *time.Time
	if row.AcceptedAt.Valid {
		acceptedAt = &row.AcceptedAt.Time
	}

	return domain.Reconstruct(
		id,
		storeID,
		email,
		role,
		token,
		status,
		invitedBy,
		nil,
		"",
		"",
		row.ExpiresAt.Time,
		acceptedAt,
		row.CreatedAt.Time,
	), nil
}

// toDomainInvitationList converts ListStoreInvitations row to domain Invitation.
func (r *Repository) toDomainInvitationList(row sqlc.ListStoreInvitationsRow) (*domain.Invitation, error) {
	id, err := vo.NewInvitationID(row.ID.String())
	if err != nil {
		return nil, err
	}

	storeID, err := vo.NewStoreID(row.StoreID.String())
	if err != nil {
		return nil, err
	}

	email, err := vo.NewEmail(row.Email)
	if err != nil {
		return nil, err
	}

	role, err := vo.NewRole(row.Role)
	if err != nil {
		return nil, err
	}

	token, err := domain.NewInvitationToken(row.Token)
	if err != nil {
		return nil, err
	}

	status, err := domain.NewInvitationStatus(row.Status)
	if err != nil {
		return nil, err
	}

	invitedBy, err := vo.NewMemberID(row.InvitedBy.String())
	if err != nil {
		return nil, err
	}

	var inviterName *string
	if row.InviterName.Valid {
		inviterName = &row.InviterName.String
	}

	var acceptedAt *time.Time
	if row.AcceptedAt.Valid {
		acceptedAt = &row.AcceptedAt.Time
	}

	return domain.Reconstruct(
		id,
		storeID,
		email,
		role,
		token,
		status,
		invitedBy,
		inviterName,
		"",
		"",
		row.ExpiresAt.Time,
		acceptedAt,
		row.CreatedAt.Time,
	), nil
}
