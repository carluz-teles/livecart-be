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
	"livecart/apps/api/internal/product/domain"
	"livecart/apps/api/lib/httpx"
	vo "livecart/apps/api/lib/valueobject"
)

type Repository struct {
	q    *sqlc.Queries
	pool *pgxpool.Pool
}

func NewRepository(q *sqlc.Queries, pool *pgxpool.Pool) *Repository {
	return &Repository{q: q, pool: pool}
}

func (r *Repository) Save(ctx context.Context, product *domain.Product) error {
	sp := product.Shipping()
	_, err := r.q.CreateProduct(ctx, sqlc.CreateProductParams{
		StoreID:             product.StoreID().ToPgUUID(),
		Name:                product.Name(),
		ExternalID:          pgtype.Text{String: product.ExternalID(), Valid: product.ExternalID() != ""},
		ExternalSource:      product.ExternalSource().String(),
		Keyword:             product.Keyword().String(),
		Price:               pgtype.Int8{Int64: product.Price().Cents(), Valid: true},
		ImageUrl:            pgtype.Text{String: product.ImageURL(), Valid: product.ImageURL() != ""},
		Stock:               pgtype.Int4{Int32: int32(product.Stock()), Valid: true},
		WeightGrams:         intPtrToInt4(sp.WeightGrams),
		HeightCm:            intPtrToInt4(sp.HeightCm),
		WidthCm:             intPtrToInt4(sp.WidthCm),
		LengthCm:            intPtrToInt4(sp.LengthCm),
		Sku:                 pgtype.Text{String: sp.SKU, Valid: sp.SKU != ""},
		PackageFormat:       packageFormatToColumn(sp.PackageFormat),
		InsuranceValueCents: int64PtrToInt8(sp.InsuranceValueCents),
	})
	if err != nil {
		return fmt.Errorf("inserting product: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id vo.ProductID, storeID vo.StoreID) (*domain.Product, error) {
	row, err := r.q.GetProductByID(ctx, sqlc.GetProductByIDParams{
		ID:      id.ToPgUUID(),
		StoreID: storeID.ToPgUUID(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("product not found")
		}
		return nil, fmt.Errorf("getting product: %w", err)
	}

	return toDomainProduct(row)
}

func (r *Repository) GetByKeyword(ctx context.Context, storeID vo.StoreID, keyword domain.Keyword) (*domain.Product, error) {
	row, err := r.q.GetProductByKeyword(ctx, sqlc.GetProductByKeywordParams{
		StoreID: storeID.ToPgUUID(),
		Keyword: keyword.String(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("product not found")
		}
		return nil, fmt.Errorf("getting product by keyword: %w", err)
	}

	return toDomainProduct(row)
}

func (r *Repository) ListByStore(ctx context.Context, storeID vo.StoreID) ([]*domain.Product, error) {
	rows, err := r.q.ListProductsByStore(ctx, storeID.ToPgUUID())
	if err != nil {
		return nil, fmt.Errorf("listing products: %w", err)
	}

	result := make([]*domain.Product, len(rows))
	for i, row := range rows {
		product, err := toDomainProduct(row)
		if err != nil {
			return nil, fmt.Errorf("converting product row: %w", err)
		}
		result[i] = product
	}
	return result, nil
}

// List returns products with filtering, pagination, and sorting
func (r *Repository) List(ctx context.Context, params ListProductsParams) (ListProductsResult, error) {
	// Build WHERE conditions
	conditions := []string{"store_id = $1"}
	args := []interface{}{params.StoreID.String()}
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

	// Shippable filter: products with all four physical fields populated
	if params.Filters.Shippable != nil {
		if *params.Filters.Shippable {
			conditions = append(conditions, "weight_grams IS NOT NULL AND height_cm IS NOT NULL AND width_cm IS NOT NULL AND length_cm IS NOT NULL")
		} else {
			conditions = append(conditions, "(weight_grams IS NULL OR height_cm IS NULL OR width_cm IS NULL OR length_cm IS NULL)")
		}
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
		SELECT id, store_id, name, external_id, external_source, keyword, price, image_url, stock, active, created_at, updated_at,
		       weight_grams, height_cm, width_cm, length_cm, sku, package_format, insurance_value_cents
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

	products := make([]*domain.Product, 0)
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
			&row.WeightGrams,
			&row.HeightCm,
			&row.WidthCm,
			&row.LengthCm,
			&row.Sku,
			&row.PackageFormat,
			&row.InsuranceValueCents,
		); err != nil {
			return ListProductsResult{}, fmt.Errorf("scanning product: %w", err)
		}
		product, err := toDomainProduct(row)
		if err != nil {
			return ListProductsResult{}, fmt.Errorf("converting product row: %w", err)
		}
		products = append(products, product)
	}

	if err := rows.Err(); err != nil {
		return ListProductsResult{}, fmt.Errorf("iterating products: %w", err)
	}

	return ListProductsResult{
		Products: products,
		Total:    total,
	}, nil
}

func (r *Repository) Update(ctx context.Context, product *domain.Product) error {
	sp := product.Shipping()
	_, err := r.q.UpdateProduct(ctx, sqlc.UpdateProductParams{
		ID:                  product.ID().ToPgUUID(),
		StoreID:             product.StoreID().ToPgUUID(),
		Name:                product.Name(),
		Price:               pgtype.Int8{Int64: product.Price().Cents(), Valid: true},
		ImageUrl:            pgtype.Text{String: product.ImageURL(), Valid: product.ImageURL() != ""},
		Stock:               pgtype.Int4{Int32: int32(product.Stock()), Valid: true},
		Active:              pgtype.Bool{Bool: product.Active(), Valid: true},
		WeightGrams:         intPtrToInt4(sp.WeightGrams),
		HeightCm:            intPtrToInt4(sp.HeightCm),
		WidthCm:             intPtrToInt4(sp.WidthCm),
		LengthCm:            intPtrToInt4(sp.LengthCm),
		Sku:                 pgtype.Text{String: sp.SKU, Valid: sp.SKU != ""},
		PackageFormat:       packageFormatToColumn(sp.PackageFormat),
		InsuranceValueCents: int64PtrToInt8(sp.InsuranceValueCents),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound("product not found")
		}
		return fmt.Errorf("updating product: %w", err)
	}

	return nil
}

func (r *Repository) GetMaxKeyword(ctx context.Context, storeID vo.StoreID) (string, error) {
	maxKw, err := r.q.GetMaxKeyword(ctx, storeID.ToPgUUID())
	if err != nil {
		return "", fmt.Errorf("getting max keyword: %w", err)
	}

	// COALESCE returns interface{} from sqlc; cast to string
	if s, ok := maxKw.(string); ok {
		return s, nil
	}
	return "0999", nil
}

func (r *Repository) Delete(ctx context.Context, id vo.ProductID, storeID vo.StoreID) error {
	result, err := r.pool.Exec(ctx, "DELETE FROM products WHERE id = $1 AND store_id = $2", id.ToPgUUID(), storeID.ToPgUUID())
	if err != nil {
		return fmt.Errorf("deleting product: %w", err)
	}

	if result.RowsAffected() == 0 {
		return httpx.ErrNotFound("product not found")
	}

	return nil
}

func (r *Repository) GetByExternalID(ctx context.Context, storeID vo.StoreID, externalSource domain.ExternalSource, externalID string) (*domain.Product, error) {
	query := `
		SELECT id, store_id, name, external_id, external_source, keyword, price, image_url, stock, active, created_at, updated_at,
		       weight_grams, height_cm, width_cm, length_cm, sku, package_format, insurance_value_cents
		FROM products
		WHERE store_id = $1 AND external_source = $2 AND external_id = $3
	`

	var row sqlc.Product
	err := r.pool.QueryRow(ctx, query, storeID.ToPgUUID(), externalSource.String(), externalID).Scan(
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
		&row.WeightGrams,
		&row.HeightCm,
		&row.WidthCm,
		&row.LengthCm,
		&row.Sku,
		&row.PackageFormat,
		&row.InsuranceValueCents,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // Not found, return nil without error for upsert logic
		}
		return nil, fmt.Errorf("getting product by external ID: %w", err)
	}

	return toDomainProduct(row)
}

func (r *Repository) GetStats(ctx context.Context, storeID vo.StoreID) (ProductStatsOutput, error) {
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
	err := r.pool.QueryRow(ctx, query, storeID.ToPgUUID()).Scan(
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

func toDomainProduct(row sqlc.Product) (*domain.Product, error) {
	id, err := vo.NewProductID(row.ID.String())
	if err != nil {
		return nil, err
	}

	storeID, err := vo.NewStoreID(row.StoreID.String())
	if err != nil {
		return nil, err
	}

	externalSource, err := domain.NewExternalSource(row.ExternalSource)
	if err != nil {
		return nil, err
	}

	var keyword domain.Keyword
	if row.Keyword != "" {
		keyword, err = domain.NewKeyword(row.Keyword)
		if err != nil {
			return nil, err
		}
	}

	var price vo.Money
	if row.Price.Valid {
		price, err = vo.NewMoney(row.Price.Int64)
		if err != nil {
			return nil, err
		}
	}

	var imageURL string
	if row.ImageUrl.Valid {
		imageURL = row.ImageUrl.String
	}

	var externalID string
	if row.ExternalID.Valid {
		externalID = row.ExternalID.String
	}

	format, err := domain.NewPackageFormat(row.PackageFormat)
	if err != nil {
		return nil, err
	}

	shipping := domain.ShippingProfile{
		WeightGrams:         int4ToIntPtr(row.WeightGrams),
		HeightCm:            int4ToIntPtr(row.HeightCm),
		WidthCm:             int4ToIntPtr(row.WidthCm),
		LengthCm:            int4ToIntPtr(row.LengthCm),
		SKU:                 textToString(row.Sku),
		PackageFormat:       format,
		InsuranceValueCents: int8ToInt64Ptr(row.InsuranceValueCents),
	}

	return domain.Reconstruct(
		id,
		storeID,
		row.Name,
		externalID,
		externalSource,
		keyword,
		price,
		imageURL,
		int(row.Stock.Int32),
		row.Active.Bool,
		shipping,
		row.CreatedAt.Time,
		row.UpdatedAt.Time,
	), nil
}

// ============================================
// pgtype helpers
// ============================================

func intPtrToInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

func int4ToIntPtr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int32)
	return &i
}

func int64PtrToInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

func int8ToInt64Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func textToString(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func packageFormatToColumn(f domain.PackageFormat) string {
	s := f.String()
	if s == "" {
		return string(domain.PackageFormatBox)
	}
	return s
}
