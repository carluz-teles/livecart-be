package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
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

// Create inserts a new catalog and returns it.
func (r *Repository) Create(ctx context.Context, storeID vo.StoreID, name string) (Catalog, error) {
	row, err := r.q.CreateCatalog(ctx, sqlc.CreateCatalogParams{
		StoreID: storeID.ToPgUUID(),
		Name:    name,
	})
	if err != nil {
		return Catalog{}, fmt.Errorf("inserting catalog: %w", err)
	}
	return toCatalog(row, 0), nil
}

// GetByID returns a single catalog scoped to the store.
func (r *Repository) GetByID(ctx context.Context, id vo.ID, storeID vo.StoreID) (Catalog, error) {
	row, err := r.q.GetCatalogByID(ctx, sqlc.GetCatalogByIDParams{
		ID:      id.ToPgUUID(),
		StoreID: storeID.ToPgUUID(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Catalog{}, httpx.ErrNotFound("catalog not found")
		}
		return Catalog{}, fmt.Errorf("getting catalog: %w", err)
	}
	return toCatalog(row, 0), nil
}

// List returns all catalogs of a store with their product counts.
func (r *Repository) List(ctx context.Context, storeID vo.StoreID) ([]Catalog, error) {
	rows, err := r.q.ListCatalogsByStore(ctx, storeID.ToPgUUID())
	if err != nil {
		return nil, fmt.Errorf("listing catalogs: %w", err)
	}
	out := make([]Catalog, len(rows))
	for i, row := range rows {
		out[i] = Catalog{
			ID:           formatPgUUID(row.ID),
			StoreID:      formatPgUUID(row.StoreID),
			Name:         row.Name,
			ProductCount: int(row.ProductCount),
			CreatedAt:    row.CreatedAt.Time,
			UpdatedAt:    row.UpdatedAt.Time,
		}
	}
	return out, nil
}

// Update renames a catalog scoped to the store.
func (r *Repository) Update(ctx context.Context, id vo.ID, storeID vo.StoreID, name string) (Catalog, error) {
	row, err := r.q.UpdateCatalog(ctx, sqlc.UpdateCatalogParams{
		ID:      id.ToPgUUID(),
		StoreID: storeID.ToPgUUID(),
		Name:    name,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Catalog{}, httpx.ErrNotFound("catalog not found")
		}
		return Catalog{}, fmt.Errorf("updating catalog: %w", err)
	}
	return toCatalog(row, 0), nil
}

// Delete removes a catalog scoped to the store.
func (r *Repository) Delete(ctx context.Context, id vo.ID, storeID vo.StoreID) error {
	n, err := r.q.DeleteCatalog(ctx, sqlc.DeleteCatalogParams{
		ID:      id.ToPgUUID(),
		StoreID: storeID.ToPgUUID(),
	})
	if err != nil {
		return fmt.Errorf("deleting catalog: %w", err)
	}
	if n == 0 {
		return httpx.ErrNotFound("catalog not found")
	}
	return nil
}

// SetProducts replaces the whole membership of a catalog in one transaction,
// preserving the given order as the display position.
func (r *Repository) SetProducts(ctx context.Context, catalogID vo.ID, productIDs []vo.ProductID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := r.q.WithTx(tx)
	if err := qtx.ClearCatalogProducts(ctx, catalogID.ToPgUUID()); err != nil {
		return fmt.Errorf("clearing catalog products: %w", err)
	}
	for i, pid := range productIDs {
		if err := qtx.AddCatalogProduct(ctx, sqlc.AddCatalogProductParams{
			CatalogID: catalogID.ToPgUUID(),
			ProductID: pid.ToPgUUID(),
			Position:  int32(i),
		}); err != nil {
			return fmt.Errorf("adding catalog product: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// ListProducts returns the products of a catalog (store-scoped), in display order.
func (r *Repository) ListProducts(ctx context.Context, catalogID vo.ID, storeID vo.StoreID) ([]CatalogProductView, error) {
	rows, err := r.q.ListCatalogProducts(ctx, sqlc.ListCatalogProductsParams{
		CatalogID: catalogID.ToPgUUID(),
		StoreID:   storeID.ToPgUUID(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing catalog products: %w", err)
	}
	out := make([]CatalogProductView, len(rows))
	for i, row := range rows {
		out[i] = CatalogProductView{
			ID:       formatPgUUID(row.ID),
			Name:     row.Name,
			Code:     row.Keyword,
			Price:    row.Price.Int64,
			ImageURL: textOrEmpty(row.ImageUrl),
			Stock:    int(row.Stock.Int32),
			Active:   row.Active.Bool,
			Position: int(row.Position),
		}
	}
	return out, nil
}

// SetEventCatalog associates (catalogID non-nil) or clears (nil) the catalog of an
// event, scoped to the store. Returns ErrNotFound when the event doesn't exist.
func (r *Repository) SetEventCatalog(ctx context.Context, eventID vo.ID, storeID vo.StoreID, catalogID *vo.ID) error {
	var cid pgtype.UUID
	if catalogID != nil {
		cid = catalogID.ToPgUUID()
	}
	n, err := r.q.SetLiveEventCatalog(ctx, sqlc.SetLiveEventCatalogParams{
		CatalogID: cid,
		ID:        eventID.ToPgUUID(),
		StoreID:   storeID.ToPgUUID(),
	})
	if err != nil {
		return fmt.Errorf("setting event catalog: %w", err)
	}
	if n == 0 {
		return httpx.ErrNotFound("event not found")
	}
	return nil
}

// GetEventCatalog returns the catalog associated with an event (store-scoped),
// or ErrNotFound when the event has no catalog.
func (r *Repository) GetEventCatalog(ctx context.Context, eventID vo.ID, storeID vo.StoreID) (Catalog, error) {
	row, err := r.q.GetLiveEventCatalog(ctx, sqlc.GetLiveEventCatalogParams{
		ID:      eventID.ToPgUUID(),
		StoreID: storeID.ToPgUUID(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Catalog{}, httpx.ErrNotFound("event has no catalog")
		}
		return Catalog{}, fmt.Errorf("getting event catalog: %w", err)
	}
	return toCatalog(row, 0), nil
}

// GetPublicCatalogByEvent returns the catalog + active products for the buyer page.
func (r *Repository) GetPublicCatalogByEvent(ctx context.Context, eventID vo.ID) (Catalog, []CatalogProductView, error) {
	cat, err := r.q.GetCatalogByEventPublic(ctx, eventID.ToPgUUID())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Catalog{}, nil, httpx.ErrNotFound("catalog not found")
		}
		return Catalog{}, nil, fmt.Errorf("getting public catalog: %w", err)
	}
	rows, err := r.q.ListCatalogProductsByEventPublic(ctx, eventID.ToPgUUID())
	if err != nil {
		return Catalog{}, nil, fmt.Errorf("listing public catalog products: %w", err)
	}
	products := make([]CatalogProductView, len(rows))
	for i, row := range rows {
		products[i] = CatalogProductView{
			ID:       formatPgUUID(row.ID),
			Name:     row.Name,
			Code:     row.Keyword,
			Price:    row.Price.Int64,
			ImageURL: textOrEmpty(row.ImageUrl),
			Stock:    int(row.Stock.Int32),
			Active:   true,
			Position: int(row.Position),
		}
	}
	return toCatalog(cat, len(products)), products, nil
}

func toCatalog(row sqlc.Catalog, productCount int) Catalog {
	return Catalog{
		ID:           formatPgUUID(row.ID),
		StoreID:      formatPgUUID(row.StoreID),
		Name:         row.Name,
		ProductCount: productCount,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}
}

func textOrEmpty(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func formatPgUUID(p pgtype.UUID) string {
	if !p.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", p.Bytes[0:4], p.Bytes[4:6], p.Bytes[6:8], p.Bytes[8:10], p.Bytes[10:16])
}
