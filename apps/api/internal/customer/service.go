package customer

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"livecart/apps/api/internal/customer/domain"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
	"livecart/apps/api/lib/query"
)

type Service struct {
	repo         *Repository
	logger       *zap.Logger
	cartCanceler CartCanceler
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("customer"),
	}
}

// UpsertForCart is a convenience method for the live cart flow: it parses the
// store UUID, calls Upsert with handle-only data (email/phone are filled in
// later by checkout) and returns the customer's UUID as a string. Satisfies
// the live.CustomerUpserter interface.
func (s *Service) UpsertForCart(ctx context.Context, storeID, platformUserID, platformHandle string) (string, error) {
	storeUUID, err := uuid.Parse(storeID)
	if err != nil {
		return "", fmt.Errorf("parsing store id: %w", err)
	}
	cust, err := s.Upsert(ctx, UpsertCustomerInput{
		StoreID:        storeUUID,
		PlatformUserID: platformUserID,
		PlatformHandle: platformHandle,
	})
	if err != nil {
		return "", err
	}
	return cust.ID().String(), nil
}

// Upsert creates a new customer or updates existing one (by store_id + platform_user_id)
// Returns the customer (existing or new) with its UUID
func (s *Service) Upsert(ctx context.Context, input UpsertCustomerInput) (*domain.Customer, error) {
	cust, err := s.repo.Upsert(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("upserting customer: %w", err)
	}

	// Group K: customer.upserted fact (best-effort, observability only).
	// Keyed on customer_id so the fact is emitted once per customer even though
	// Upsert runs on every cart touch.
	_ = events.EmitInternal(ctx, s.repo.queries, events.CustomerUpserted,
		"customer.upserted:"+cust.ID().String(), struct {
			CustomerID string `json:"customer_id"`
			StoreID    string `json:"store_id"`
		}{CustomerID: cust.ID().String(), StoreID: input.StoreID.String()})

	logger.From(ctx, s.logger).Debug("customer upserted",
		zap.String("customerId", cust.ID().String()),
		zap.String("storeId", input.StoreID.String()),
		zap.String("platformUserID", input.PlatformUserID),
		zap.String("handle", input.PlatformHandle))

	return cust, nil
}

// GetByID returns a customer by its UUID, enriched with cart totals so the
// detail drawer matches the numbers shown on the listing.
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	base, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("customer %s not found", id))
	}

	// Aggregate from the customer's carts to keep the detail view consistent
	// with the list aggregation (COUNT distinct carts, SUM line totals).
	orders, err := s.repo.ListOrders(ctx, id, 1000, 0)
	if err != nil {
		return nil, fmt.Errorf("listing customer orders: %w", err)
	}
	var totalSpent int64
	for _, o := range orders {
		totalSpent += o.TotalValue()
	}

	email := base.Email()
	phone := base.Phone()
	var name, document *string
	var address *domain.ShippingAddress

	// Enrich with the latest checkout snapshot so the drawer can show name,
	// document, the most recent shipping address and a WhatsApp-ready phone
	// even when the customers row hasn't been backfilled. customers.email and
	// customers.phone (set at checkout) win over the snapshot — that's the
	// canonical place; the snapshot only fills gaps.
	snap, err := s.repo.GetCheckoutSnapshot(ctx, id)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to load customer checkout snapshot",
			zap.String("customerId", id.String()),
			zap.Error(err),
		)
	}
	if snap != nil {
		if snap.Name != "" {
			n := snap.Name
			name = &n
		}
		if snap.Document != "" {
			d := snap.Document
			document = &d
		}
		if phone == nil && snap.Phone != "" {
			p := snap.Phone
			phone = &p
		}
		if email == nil && snap.Email != "" {
			e := snap.Email
			email = &e
		}
		if snap.Address != nil {
			addr := domain.NewShippingAddress(
				snap.Address.ZipCode,
				snap.Address.Street,
				snap.Address.Number,
				snap.Address.Complement,
				snap.Address.Neighborhood,
				snap.Address.City,
				snap.Address.State,
			)
			address = &addr
		}
	}

	return domain.Reconstruct(
		base.ID(),
		base.PlatformUserID(),
		base.Handle(),
		email,
		phone,
		name,
		document,
		len(orders),
		totalSpent,
		base.LastOrderAt(),
		base.FirstOrderAt(),
		address,
	), nil
}

// GetByPlatformUser returns a customer by store_id + platform_user_id. Returns
// nil (not an error) when the customer doesn't exist.
func (s *Service) GetByPlatformUser(ctx context.Context, storeID uuid.UUID, platformUserID string) (*domain.Customer, error) {
	cust, err := s.repo.GetByPlatformUser(ctx, storeID, platformUserID)
	if err != nil {
		return nil, err
	}
	return cust, nil
}

// List returns customers with pagination and search as domain entities.
func (s *Service) List(ctx context.Context, input ListCustomersInput) ([]*domain.Customer, query.Pagination, int, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("last_order_at")

	customers, total, err := s.repo.List(ctx, ListCustomersParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return nil, input.Pagination, 0, err
	}

	return customers, input.Pagination, total, nil
}

// GetStats returns aggregated customer statistics.
func (s *Service) GetStats(ctx context.Context, storeID string) (*domain.CustomerStats, error) {
	return s.repo.GetStats(ctx, storeID)
}

// ListOrders returns recent carts for a customer (paid + pending).
func (s *Service) ListOrders(ctx context.Context, id uuid.UUID, limit, offset int32) ([]*domain.OrderSummary, error) {
	return s.repo.ListOrders(ctx, id, limit, offset)
}

// Update updates customer fields.
func (s *Service) Update(ctx context.Context, id uuid.UUID, input UpdateCustomerInput) error {
	return s.repo.Update(ctx, id, input)
}

// UpdateLastOrder updates the last_order_at timestamp (called when cart is created/updated).
func (s *Service) UpdateLastOrder(ctx context.Context, id uuid.UUID) error {
	return s.repo.UpdateLastOrder(ctx, id)
}
