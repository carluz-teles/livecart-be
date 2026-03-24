package product

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
)

type Repository struct {
	q    *sqlc.Queries
	pool *pgxpool.Pool
}

func NewRepository(q *sqlc.Queries, pool *pgxpool.Pool) *Repository {
	return &Repository{q: q, pool: pool}
}

func (r *Repository) Create(ctx context.Context, params CreateProductParams) (ProductRow, error) {
	storeUID, err := parseUUID(params.StoreID)
	if err != nil {
		return ProductRow{}, err
	}

	row, err := r.q.CreateProduct(ctx, sqlc.CreateProductParams{
		StoreID:        storeUID,
		Name:           params.Name,
		ExternalID:     pgtype.Text{String: params.ExternalID, Valid: params.ExternalID != ""},
		ExternalSource: params.ExternalSource,
		Keyword:        params.Keyword,
		Price:          pgtype.Int8{Int64: params.Price, Valid: true},
		ImageUrl:       pgtype.Text{String: params.ImageURL, Valid: params.ImageURL != ""},
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

// List returns products with filtering, pagination, and sorting
func (r *Repository) List(ctx context.Context, params ListProductsParams) (ListProductsResult, error) {
	// Build WHERE conditions
	conditions := []string{"store_id = $1"}
	args := []interface{}{params.StoreID}
	argIdx := 2

	// Search filter (name or keyword)
	if params.Search != "" {
		conditions = append(conditions, fmt.Sprintf("(LOWER(name) LIKE $%d OR keyword LIKE $%d)", argIdx, argIdx))
		args = append(args, "%"+strings.ToLower(params.Search)+"%")
		argIdx++
	}

	// Status filter
	if len(params.Filters.Status) > 0 {
		statusConditions := make([]string, 0, len(params.Filters.Status))
		for _, status := range params.Filters.Status {
			if status == "active" {
				statusConditions = append(statusConditions, "active = true")
			} else if status == "inactive" {
				statusConditions = append(statusConditions, "active = false")
			}
		}
		if len(statusConditions) > 0 {
			conditions = append(conditions, "("+strings.Join(statusConditions, " OR ")+")")
		}
	}

	// External source filter
	if len(params.Filters.ExternalSource) > 0 {
		placeholders := make([]string, len(params.Filters.ExternalSource))
		for i, source := range params.Filters.ExternalSource {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, source)
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("external_source IN (%s)", strings.Join(placeholders, ", ")))
	}

	// Price range filters
	if params.Filters.PriceMin != nil {
		conditions = append(conditions, fmt.Sprintf("price >= $%d", argIdx))
		args = append(args, *params.Filters.PriceMin)
		argIdx++
	}
	if params.Filters.PriceMax != nil {
		conditions = append(conditions, fmt.Sprintf("price <= $%d", argIdx))
		args = append(args, *params.Filters.PriceMax)
		argIdx++
	}

	// Stock range filters
	if params.Filters.StockMin != nil {
		conditions = append(conditions, fmt.Sprintf("stock >= $%d", argIdx))
		args = append(args, *params.Filters.StockMin)
		argIdx++
	}
	if params.Filters.StockMax != nil {
		conditions = append(conditions, fmt.Sprintf("stock <= $%d", argIdx))
		args = append(args, *params.Filters.StockMax)
		argIdx++
	}

	// Low stock filter
	if params.Filters.HasLowStock != nil && *params.Filters.HasLowStock {
		conditions = append(conditions, "stock <= 5")
	}

	whereClause := strings.Join(conditions, " AND ")

	// Validate and build ORDER BY
	allowedSortFields := map[string]string{
		"name":       "name",
		"price":      "price",
		"stock":      "stock",
		"created_at": "created_at",
		"updated_at": "updated_at",
		"keyword":    "keyword",
	}
	sortField, ok := allowedSortFields[params.Sorting.SortBy]
	if !ok {
		sortField = "created_at"
	}
	orderClause := fmt.Sprintf("%s %s", sortField, params.Sorting.OrderSQL())

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM products WHERE %s", whereClause)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return ListProductsResult{}, fmt.Errorf("counting products: %w", err)
	}

	// Build main query with pagination
	query := fmt.Sprintf(`
		SELECT id, store_id, name, external_id, external_source, keyword, price, image_url, stock, active, created_at, updated_at
		FROM products
		WHERE %s
		ORDER BY %s
		LIMIT $%d OFFSET $%d
	`, whereClause, orderClause, argIdx, argIdx+1)

	args = append(args, params.Pagination.Limit, params.Pagination.Offset())

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return ListProductsResult{}, fmt.Errorf("listing products: %w", err)
	}
	defer rows.Close()

	products := make([]ProductRow, 0)
	for rows.Next() {
		var row sqlc.Product
		if err := rows.Scan(
			&row.ID,
			&row.StoreID,
			&row.Name,
			&row.ExternalID,
			&row.ExternalSource,
			&row.Keyword,
			&row.Price,
			&row.ImageUrl,
			&row.Stock,
			&row.Active,
			&row.CreatedAt,
			&row.UpdatedAt,
		); err != nil {
			return ListProductsResult{}, fmt.Errorf("scanning product: %w", err)
		}
		products = append(products, toProductRow(row))
	}

	if err := rows.Err(); err != nil {
		return ListProductsResult{}, fmt.Errorf("iterating products: %w", err)
	}

	return ListProductsResult{
		Products: products,
		Total:    total,
	}, nil
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

	row, err := r.q.UpdateProduct(ctx, sqlc.UpdateProductParams{
		ID:       uid,
		StoreID:  storeUID,
		Name:     params.Name,
		Price:    pgtype.Int8{Int64: params.Price, Valid: true},
		ImageUrl: pgtype.Text{String: params.ImageURL, Valid: params.ImageURL != ""},
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
	var price int64
	if row.Price.Valid {
		price = row.Price.Int64
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
		Stock:          int(row.Stock.Int32),
		Active:         row.Active.Bool,
		CreatedAt:      row.CreatedAt.Time,
		UpdatedAt:      row.UpdatedAt.Time,
	}
}

func (r *Repository) Delete(ctx context.Context, id, storeID string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return err
	}

	result, err := r.pool.Exec(ctx, "DELETE FROM products WHERE id = $1 AND store_id = $2", uid, storeUID)
	if err != nil {
		return fmt.Errorf("deleting product: %w", err)
	}

	if result.RowsAffected() == 0 {
		return httpx.ErrNotFound("product not found")
	}

	return nil
}

func (r *Repository) GetStats(ctx context.Context, storeID string) (ProductStatsOutput, error) {
	storeUID, err := parseUUID(storeID)
	if err != nil {
		return ProductStatsOutput{}, err
	}

	query := `
		SELECT
			COUNT(*) as total_products,
			COUNT(*) FILTER (WHERE active = true) as active_count,
			COUNT(*) FILTER (WHERE stock <= 5) as low_stock_count,
			COALESCE(SUM(price * stock), 0) as stock_value
		FROM products
		WHERE store_id = $1
	`

	var stats ProductStatsOutput
	err = r.pool.QueryRow(ctx, query, storeUID).Scan(
		&stats.TotalProducts,
		&stats.ActiveCount,
		&stats.LowStockCount,
		&stats.StockValue,
	)
	if err != nil {
		return ProductStatsOutput{}, fmt.Errorf("getting product stats: %w", err)
	}

	return stats, nil
}

func parseUUID(s string) (pgtype.UUID, error) {
	var uid pgtype.UUID
	if err := uid.Scan(s); err != nil {
		return uid, httpx.ErrUnprocessable("invalid uuid")
	}
	return uid, nil
}
