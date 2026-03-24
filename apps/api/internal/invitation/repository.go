package invitation

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

func (r *Repository) Create(ctx context.Context, storeID, email, role, token string, invitedBy string, expiresAt time.Time) (*InvitationRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}
	inviterUID, err := parseUUID(invitedBy)
	if err != nil {
		return nil, err
	}

	row, err := r.q.CreateInvitation(ctx, sqlc.CreateInvitationParams{
		StoreID:   storeUID,
		Email:     email,
		Role:      role,
		Token:     token,
		InvitedBy: inviterUID,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("creating invitation: %w", err)
	}

	return &InvitationRow{
		ID:        row.ID.String(),
		StoreID:   row.StoreID.String(),
		Email:     row.Email,
		Role:      row.Role,
		Token:     row.Token,
		InvitedBy: row.InvitedBy.String(),
		Status:    row.Status,
		ExpiresAt: row.ExpiresAt.Time,
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

func (r *Repository) GetByToken(ctx context.Context, token string) (*InvitationRow, error) {
	row, err := r.q.GetInvitationByToken(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("invitation not found")
		}
		return nil, fmt.Errorf("getting invitation by token: %w", err)
	}

	var inviterName *string
	if row.InviterName.Valid {
		inviterName = &row.InviterName.String
	}

	return &InvitationRow{
		ID:          row.ID.String(),
		StoreID:     row.StoreID.String(),
		Email:       row.Email,
		Role:        row.Role,
		Token:       row.Token,
		InvitedBy:   row.InvitedBy.String(),
		Status:      row.Status,
		InviterName: inviterName,
		StoreName:   row.StoreName,
		StoreSlug:   row.StoreSlug,
		ExpiresAt:   row.ExpiresAt.Time,
		CreatedAt:   row.CreatedAt.Time,
	}, nil
}

func (r *Repository) GetByEmail(ctx context.Context, storeID, email string) (*InvitationRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetInvitationByEmail(ctx, sqlc.GetInvitationByEmailParams{
		StoreID: storeUID,
		Email:   email,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("invitation not found")
		}
		return nil, fmt.Errorf("getting invitation by email: %w", err)
	}

	return &InvitationRow{
		ID:        row.ID.String(),
		StoreID:   row.StoreID.String(),
		Email:     row.Email,
		Role:      row.Role,
		Token:     row.Token,
		InvitedBy: row.InvitedBy.String(),
		Status:    row.Status,
		ExpiresAt: row.ExpiresAt.Time,
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

func (r *Repository) ListByStore(ctx context.Context, storeID string) ([]InvitationRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListStoreInvitations(ctx, storeUID)
	if err != nil {
		return nil, fmt.Errorf("listing invitations: %w", err)
	}

	result := make([]InvitationRow, len(rows))
	for i, row := range rows {
		var inviterName *string
		if row.InviterName.Valid {
			inviterName = &row.InviterName.String
		}
		var acceptedAt *time.Time
		if row.AcceptedAt.Valid {
			acceptedAt = &row.AcceptedAt.Time
		}

		result[i] = InvitationRow{
			ID:          row.ID.String(),
			StoreID:     row.StoreID.String(),
			Email:       row.Email,
			Role:        row.Role,
			Token:       row.Token,
			InvitedBy:   row.InvitedBy.String(),
			Status:      row.Status,
			InviterName: inviterName,
			ExpiresAt:   row.ExpiresAt.Time,
			AcceptedAt:  acceptedAt,
			CreatedAt:   row.CreatedAt.Time,
		}
	}

	return result, nil
}

func (r *Repository) Accept(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	_, err = r.q.AcceptInvitation(ctx, uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound("invitation not found or already accepted")
		}
		return fmt.Errorf("accepting invitation: %w", err)
	}

	return nil
}

func (r *Repository) Revoke(ctx context.Context, storeID, id string) error {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	err = r.q.RevokeInvitation(ctx, sqlc.RevokeInvitationParams{
		StoreID: storeUID,
		ID:      uid,
	})
	if err != nil {
		return fmt.Errorf("revoking invitation: %w", err)
	}

	return nil
}

func (r *Repository) Delete(ctx context.Context, storeID, id string) error {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	err = r.q.DeleteInvitation(ctx, sqlc.DeleteInvitationParams{
		StoreID: storeUID,
		ID:      uid,
	})
	if err != nil {
		return fmt.Errorf("deleting invitation: %w", err)
	}

	return nil
}

func (r *Repository) AddUserToStore(ctx context.Context, storeID, clerkUserID, email, name, avatarURL, role, invitedBy string) error {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}
	inviterUID, err := parseUUID(invitedBy)
	if err != nil {
		return err
	}

	_, err = r.q.AddUserToStore(ctx, sqlc.AddUserToStoreParams{
		StoreID:     storeUID,
		ClerkUserID: pgtype.Text{String: clerkUserID, Valid: true},
		Email:       email,
		Name:        pgtype.Text{String: name, Valid: name != ""},
		AvatarUrl:   pgtype.Text{String: avatarURL, Valid: avatarURL != ""},
		Role:        role,
		InvitedBy:   inviterUID,
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
