package productgroup

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/productgroup/domain"
	productdomain "livecart/apps/api/internal/product/domain"
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

func (r *Repository) Pool() *pgxpool.Pool { return r.pool }

func (r *Repository) GetByID(ctx context.Context, id vo.ID, storeID vo.StoreID) (*domain.Group, error) {
	row, err := r.q.GetProductGroupByID(ctx, sqlc.GetProductGroupByIDParams{
		ID: id.ToPgUUID(), StoreID: storeID.ToPgUUID(),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.ErrNotFound("product group not found")
		}
		return nil, fmt.Errorf("getting product group: %w", err)
	}
	return rowToGroup(row)
}

func (r *Repository) GetByExternalID(ctx context.Context, storeID vo.StoreID, source productdomain.ExternalSource, externalID string) (*domain.Group, error) {
	row, err := r.q.GetProductGroupByExternalID(ctx, sqlc.GetProductGroupByExternalIDParams{
		StoreID:        storeID.ToPgUUID(),
		ExternalSource: source.String(),
		ExternalID:     pgtype.Text{String: externalID, Valid: externalID != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting group by external id: %w", err)
	}
	return rowToGroup(row)
}

func (r *Repository) ListByStore(ctx context.Context, storeID vo.StoreID, limit, offset int) ([]GroupSummaryResponse, int, error) {
	total, err := r.q.CountProductGroupsByStore(ctx, storeID.ToPgUUID())
	if err != nil {
		return nil, 0, fmt.Errorf("counting groups: %w", err)
	}
	rows, err := r.q.ListProductGroupsByStore(ctx, sqlc.ListProductGroupsByStoreParams{
		StoreID: storeID.ToPgUUID(),
		Limit:   int32(limit),
		Offset:  int32(offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("listing groups: %w", err)
	}
	out := make([]GroupSummaryResponse, len(rows))
	for i, row := range rows {
		out[i] = GroupSummaryResponse{
			ID:             pgUUIDToString(row.ID),
			Name:           row.Name,
			Description:    textOrEmpty(row.Description),
			ExternalID:     textOrEmpty(row.ExternalID),
			ExternalSource: row.ExternalSource,
			VariantsCount:  int(row.VariantsCount),
			CreatedAt:      row.CreatedAt.Time,
			UpdatedAt:      row.UpdatedAt.Time,
		}
	}
	return out, int(total), nil
}

func (r *Repository) Update(ctx context.Context, g *domain.Group) error {
	_, err := r.q.UpdateProductGroup(ctx, sqlc.UpdateProductGroupParams{
		ID:          g.ID().ToPgUUID(),
		StoreID:     g.StoreID().ToPgUUID(),
		Name:        g.Name(),
		Description: pgtype.Text{String: g.Description(), Valid: g.Description() != ""},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound("product group not found")
		}
		return fmt.Errorf("updating group: %w", err)
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, id vo.ID, storeID vo.StoreID) error {
	return r.q.DeleteProductGroup(ctx, sqlc.DeleteProductGroupParams{
		ID: id.ToPgUUID(), StoreID: storeID.ToPgUUID(),
	})
}

// LoadOptions returns all options + their values for the group.
func (r *Repository) LoadOptions(ctx context.Context, groupID vo.ID) ([]domain.Option, error) {
	options, err := r.q.ListProductOptionsByGroup(ctx, groupID.ToPgUUID())
	if err != nil {
		return nil, fmt.Errorf("listing options: %w", err)
	}
	values, err := r.q.ListProductOptionValuesByGroup(ctx, groupID.ToPgUUID())
	if err != nil {
		return nil, fmt.Errorf("listing option values: %w", err)
	}

	byOption := make(map[string][]domain.OptionValue)
	for _, v := range values {
		key := pgUUIDToString(v.OptionID)
		byOption[key] = append(byOption[key], domain.OptionValue{
			ID:       mustIDFromPg(v.ID),
			OptionID: mustIDFromPg(v.OptionID),
			Value:    v.Value,
			Position: int(v.Position),
		})
	}

	out := make([]domain.Option, len(options))
	for i, o := range options {
		out[i] = domain.Option{
			ID:       mustIDFromPg(o.ID),
			GroupID:  mustIDFromPg(o.GroupID),
			Name:     o.Name,
			Position: int(o.Position),
			Values:   byOption[pgUUIDToString(o.ID)],
		}
	}
	return out, nil
}

func (r *Repository) ListGroupImages(ctx context.Context, groupID vo.ID) ([]ImageResponse, error) {
	rows, err := r.q.ListProductGroupImagesByGroup(ctx, groupID.ToPgUUID())
	if err != nil {
		return nil, fmt.Errorf("listing group images: %w", err)
	}
	out := make([]ImageResponse, len(rows))
	for i, row := range rows {
		out[i] = ImageResponse{ID: pgUUIDToString(row.ID), URL: row.Url, Position: int(row.Position)}
	}
	return out, nil
}

func (r *Repository) AddGroupImage(ctx context.Context, groupID vo.ID, url string, position int) (ImageResponse, error) {
	row, err := r.q.CreateProductGroupImage(ctx, sqlc.CreateProductGroupImageParams{
		GroupID:  groupID.ToPgUUID(),
		Url:      url,
		Position: int32(position),
	})
	if err != nil {
		return ImageResponse{}, fmt.Errorf("inserting group image: %w", err)
	}
	return ImageResponse{ID: pgUUIDToString(row.ID), URL: row.Url, Position: int(row.Position)}, nil
}

func (r *Repository) DeleteGroupImage(ctx context.Context, imageID vo.ID, groupID vo.ID) error {
	return r.q.DeleteProductGroupImage(ctx, sqlc.DeleteProductGroupImageParams{
		ID: imageID.ToPgUUID(), GroupID: groupID.ToPgUUID(),
	})
}

func (r *Repository) ListVariantsByGroup(ctx context.Context, groupID vo.ID) ([]sqlc.Product, error) {
	return r.q.ListProductsByGroup(ctx, groupID.ToPgUUID())
}

func (r *Repository) ListVariantOptionsByGroup(ctx context.Context, groupID vo.ID) ([]sqlc.ListVariantOptionsByGroupRow, error) {
	return r.q.ListVariantOptionsByGroup(ctx, groupID.ToPgUUID())
}

func (r *Repository) ListVariantImagesByGroup(ctx context.Context, groupID vo.ID) ([]sqlc.ProductImage, error) {
	return r.q.ListProductImagesByGroup(ctx, groupID.ToPgUUID())
}

// ============================================
// helpers
// ============================================

func rowToGroup(row sqlc.ProductGroup) (*domain.Group, error) {
	id, err := vo.NewID(pgUUIDToString(row.ID))
	if err != nil {
		return nil, err
	}
	storeID, err := vo.NewStoreID(pgUUIDToString(row.StoreID))
	if err != nil {
		return nil, err
	}
	source, err := productdomain.NewExternalSource(row.ExternalSource)
	if err != nil {
		return nil, err
	}
	return domain.Reconstruct(
		id, storeID,
		row.Name,
		textOrEmpty(row.Description),
		textOrEmpty(row.ExternalID),
		source,
		row.CreatedAt.Time,
		row.UpdatedAt.Time,
	), nil
}

func mustIDFromPg(p pgtype.UUID) vo.ID {
	id, _ := vo.NewID(pgUUIDToString(p))
	return id
}

func pgUUIDToString(p pgtype.UUID) string {
	if !p.Valid {
		return ""
	}
	// pgtype.UUID stores raw bytes; format as canonical uuid
	return fmt.Sprintf("%x-%x-%x-%x-%x", p.Bytes[0:4], p.Bytes[4:6], p.Bytes[6:8], p.Bytes[8:10], p.Bytes[10:16])
}

func textOrEmpty(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}
