package checkout

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"livecart/apps/api/db/sqlc"
)

func (r *Repository) AddCartItemQuantityAtPrice(ctx context.Context, itemID string, expectedQuantity, addedQuantity int, unitPrice int64) (bool, error) {
	id, err := uuid.Parse(itemID)
	if err != nil {
		return false, fmt.Errorf("parsing cart item id: %w", err)
	}
	n, err := r.q.AddCartItemQuantityAtPrice(ctx, sqlc.AddCartItemQuantityAtPriceParams{
		ItemID: pgtype.UUID{Bytes: id, Valid: true}, ExpectedQuantity: int32(expectedQuantity),
		AddedQuantity: int32(addedQuantity), UnitPrice: unitPrice,
	})
	if err != nil {
		return false, fmt.Errorf("adding cart quantity at agreed price: %w", err)
	}
	return n > 0, nil
}
