package product

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

func (r *Repository) Create(ctx context.Context, params CreateProductParams) (ProductRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return ProductRow{}, err
	}

	var price pgtype.Numeric
	if params.Price != "" {
		if err := price.Scan(params.Price); err != nil {
			return ProductRow{}, httpx.ErrUnprocessable("invalid price")
		}
	}

	row, err := r.q.CreateProduct(ctx, sqlc.CreateProductParams{
		StoreID:        storeUID,
		Name:           params.Name,
		ExternalID:     pgtype.Text{String: params.ExternalID, Valid: params.ExternalID != ""},
		ExternalSource: params.ExternalSource,
		Keyword:        params.Keyword,
		Price:          price,
		ImageUrl:       pgtype.Text{String: params.ImageURL, Valid: params.ImageURL != ""},
		Sizes:          params.Sizes,
		Stock:          pgtype.Int4{Int32: int32(params.Stock), Valid: true},
	})
	if err != nil {
		return ProductRow{}, fmt.Errorf("inserting product: %w", err)
	}

	return toProductRow(row), nil
}

func (r *Repository) GetByID(ctx context.Context, id, storeID string) (*ProductRow, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetProductByID(ctx, sqlc.GetProductByIDParams{
		ID:      uid,
		StoreID: storeUID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("product not found")
		}
		return nil, fmt.Errorf("getting product: %w", err)
	}

	out := toProductRow(row)
	return &out, nil
}

func (r *Repository) GetByKeyword(ctx context.Context, params GetByKeywordParams) (*ProductRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return nil, err
	}

	row, err := r.q.GetProductByKeyword(ctx, sqlc.GetProductByKeywordParams{
		StoreID: storeUID,
		Keyword: params.Keyword,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("product not found")
		}
		return nil, fmt.Errorf("getting product by keyword: %w", err)
	}

	out := toProductRow(row)
	return &out, nil
}

func (r *Repository) ListByStore(ctx context.Context, storeID string) ([]ProductRow, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return nil, err
	}

	rows, err := r.q.ListProductsByStore(ctx, storeUID)
	if err != nil {
		return nil, fmt.Errorf("listing products: %w", err)
	}

	result := make([]ProductRow, len(rows))
	for i, row := range rows {
		result[i] = toProductRow(row)
	}
	return result, nil
}

func (r *Repository) Update(ctx context.Context, params UpdateProductParams) (ProductRow, error) {
	uid, err := parseUUID(params.ID)
	if err != nil {
		return ProductRow{}, err
	}
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return ProductRow{}, err
	}

	var price pgtype.Numeric
	if params.Price != "" {
		if err := price.Scan(params.Price); err != nil {
			return ProductRow{}, httpx.ErrUnprocessable("invalid price")
		}
	}

	row, err := r.q.UpdateProduct(ctx, sqlc.UpdateProductParams{
		ID:       uid,
		StoreID:  storeUID,
		Name:     params.Name,
		Price:    price,
		ImageUrl: pgtype.Text{String: params.ImageURL, Valid: params.ImageURL != ""},
		Sizes:    params.Sizes,
		Stock:    pgtype.Int4{Int32: int32(params.Stock), Valid: true},
		Active:   pgtype.Bool{Bool: params.Active, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProductRow{}, httpx.ErrNotFound("product not found")
		}
		return ProductRow{}, fmt.Errorf("updating product: %w", err)
	}

	return toProductRow(row), nil
}

func (r *Repository) GetMaxKeyword(ctx context.Context, storeID string) (string, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return "", err
	}

	maxKw, err := r.q.GetMaxKeyword(ctx, storeUID)
	if err != nil {
		return "", fmt.Errorf("getting max keyword: %w", err)
	}

	// COALESCE returns interface{} from sqlc; cast to string
	if s, ok := maxKw.(string); ok {
		return s, nil
	}
	return "0999", nil
}

func toProductRow(row sqlc.Product) ProductRow {
	var price string
	if row.Price.Valid {
		// pgtype.Numeric stores value in Int + Exp fields
		price = fmt.Sprintf("%v", row.Price)
	}
	var imageURL string
	if row.ImageUrl.Valid {
		imageURL = row.ImageUrl.String
	}
	var externalID string
	if row.ExternalID.Valid {
		externalID = row.ExternalID.String
	}

	return ProductRow{
		ID:             row.ID.String(),
		StoreID:        row.StoreID.String(),
		Name:           row.Name,
		ExternalID:     externalID,
		ExternalSource: row.ExternalSource,
		Keyword:        row.Keyword,
		Price:          price,
		ImageURL:       imageURL,
		Sizes:          row.Sizes,
		Stock:          int(row.Stock.Int32),
		Active:         row.Active.Bool,
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}
