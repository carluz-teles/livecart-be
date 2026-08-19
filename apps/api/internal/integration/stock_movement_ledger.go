package integration

// Implementação do razão de movimentos de estoque (erp.StockMovementLedger)
// sobre o sqlc. Ver internal/erp/movement_ledger.go para as regras; aqui é só
// tradução de tipos.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/erp"
)

func movementRowFromSQLC(r sqlc.ErpStockMovement) *erp.StockMovementRow {
	return &erp.StockMovementRow{
		ID:                uuidToString(r.ID),
		StoreID:           uuidToString(r.StoreID),
		CartID:            uuidToString(r.CartID),
		EventID:           uuidToString(r.EventID),
		ProductID:         uuidToString(r.ProductID),
		ExternalProductID: r.ExternalProductID,
		Direction:         r.Direction,
		Quantity:          int(r.Quantity),
		UnitPriceCents:    r.UnitPriceCents,
		IdempotencyKey:    uuidToString(r.IdempotencyKey),
		Status:            r.Status,
		ERPMovementID:     r.ErpMovementID.String,
		Attempts:          int(r.Attempts),
		LastError:         r.LastError.String,
		CreatedAt:         r.CreatedAt.Time,
	}
}

func (r *Repository) CreateERPStockMovement(ctx context.Context, p erp.CreateStockMovementParams) (*erp.StockMovementRow, error) {
	storeID, err := parseUUID(p.StoreID)
	if err != nil {
		return nil, err
	}
	cartID, err := parseUUID(p.CartID)
	if err != nil {
		return nil, err
	}
	productID, err := parseUUID(p.ProductID)
	if err != nil {
		return nil, err
	}
	var eventID pgtype.UUID
	if p.EventID != "" {
		if eventID, err = parseUUID(p.EventID); err != nil {
			return nil, err
		}
	}
	row, err := r.queries.CreateERPStockMovement(ctx, sqlc.CreateERPStockMovementParams{
		StoreID:           storeID,
		CartID:            cartID,
		EventID:           eventID,
		ProductID:         productID,
		ExternalProductID: p.ExternalProductID,
		Direction:         p.Direction,
		Quantity:          int32(p.Quantity),
		UnitPriceCents:    p.UnitPriceCents,
	})
	if err != nil {
		return nil, fmt.Errorf("creating erp stock movement: %w", err)
	}
	return movementRowFromSQLC(row), nil
}

func (r *Repository) GetERPStockMovement(ctx context.Context, id string) (*erp.StockMovementRow, error) {
	mID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.GetERPStockMovement(ctx, mID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading erp stock movement: %w", err)
	}
	return movementRowFromSQLC(row), nil
}

func (r *Repository) MarkERPStockMovementConfirmed(ctx context.Context, id, erpMovementID string) error {
	mID, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.queries.MarkERPStockMovementConfirmed(ctx, sqlc.MarkERPStockMovementConfirmedParams{
		ID:            mID,
		ErpMovementID: pgtype.Text{String: erpMovementID, Valid: erpMovementID != ""},
	})
}

func (r *Repository) MarkERPStockMovementOutcome(ctx context.Context, id, status, lastError string) error {
	mID, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.queries.MarkERPStockMovementOutcome(ctx, sqlc.MarkERPStockMovementOutcomeParams{
		ID:        mID,
		Status:    status,
		LastError: pgtype.Text{String: lastError, Valid: lastError != ""},
	})
}

func (r *Repository) ClaimERPStockMovement(ctx context.Context, id, fromStatus string) (*erp.StockMovementRow, error) {
	mID, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.ClaimERPStockMovement(ctx, sqlc.ClaimERPStockMovementParams{
		ID:         mID,
		FromStatus: fromStatus,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Outro resolver levou, o estado mudou, ou os guards de idade seguraram.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claiming erp stock movement: %w", err)
	}
	return movementRowFromSQLC(row), nil
}

func (r *Repository) ListUnresolvedERPStockMovementsByCart(ctx context.Context, cartID string) ([]erp.StockMovementRow, error) {
	cID, err := parseUUID(cartID)
	if err != nil {
		return nil, err
	}
	rows, err := r.queries.ListUnresolvedERPStockMovementsByCart(ctx, cID)
	if err != nil {
		return nil, fmt.Errorf("listing unresolved erp stock movements: %w", err)
	}
	out := make([]erp.StockMovementRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, *movementRowFromSQLC(row))
	}
	return out, nil
}
