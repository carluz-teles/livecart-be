package order

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

type Service struct {
	repo   *Repository
	logger *zap.Logger
}

func NewService(repo *Repository, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		logger: logger.Named("order"),
	}
}

func (s *Service) List(ctx context.Context, input ListOrdersInput) (ListOrdersOutput, error) {
	input.Pagination.Normalize()
	input.Sorting.Normalize("created_at")

	result, err := s.repo.List(ctx, ListOrdersParams{
		StoreID:    input.StoreID,
		Search:     input.Search,
		Pagination: input.Pagination,
		Sorting:    input.Sorting,
		Filters:    input.Filters,
	})
	if err != nil {
		return ListOrdersOutput{}, err
	}

	orders := make([]OrderOutput, len(result.Orders))
	for i, row := range result.Orders {
		orders[i] = OrderOutput{
			ID:             row.ID,
			LiveSessionID:  row.EventID, // Now using EventID but keeping response field name for backwards compatibility
			LiveTitle:      row.LiveTitle,
			LivePlatform:   row.LivePlatform,
			CustomerHandle: row.PlatformHandle,
			CustomerID:     row.PlatformUserID,
			Status:         row.Status,
			PaymentStatus:  row.PaymentStatus,
			TotalItems:     row.TotalItems,
			TotalAmount:    row.TotalAmount,
			PaidAt:         row.PaidAt,
			CreatedAt:      row.CreatedAt,
			ExpiresAt:      row.ExpiresAt,
			Items:          []OrderItemOutput{}, // Items loaded separately when needed
		}
	}

	return ListOrdersOutput{
		Orders:     orders,
		Total:      result.Total,
		Pagination: input.Pagination,
	}, nil
}

func (s *Service) GetByID(ctx context.Context, id string, storeID string) (*OrderOutput, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", id))
	}

	// Check store ownership
	if row.StoreID != storeID {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", id))
	}

	// Get items
	itemRows, err := s.repo.GetItems(ctx, id)
	if err != nil {
		return nil, err
	}

	items := make([]OrderItemOutput, len(itemRows))
	var totalAmount int64
	var totalItems int
	for i, item := range itemRows {
		itemTotal := item.UnitPrice * int64(item.Quantity)
		items[i] = OrderItemOutput{
			ID:            item.ID,
			ProductID:     item.ProductID,
			ProductName:   item.ProductName,
			ProductImage:  item.ProductImage,
			Keyword:       item.ProductKeyword,
			Size:          item.Size,
			Quantity:      item.Quantity,
			UnitPrice:     item.UnitPrice,
			TotalPrice:    itemTotal,
			WeightGrams:   item.WeightGrams,
			HeightCm:      item.HeightCm,
			WidthCm:       item.WidthCm,
			LengthCm:      item.LengthCm,
			PackageFormat: item.PackageFormat,
		}
		totalAmount += itemTotal
		totalItems += item.Quantity
	}

	return &OrderOutput{
		ID:             row.ID,
		LiveSessionID:  row.EventID, // Now using EventID but keeping response field name for backwards compatibility
		LiveTitle:      row.LiveTitle,
		LivePlatform:   row.LivePlatform,
		CustomerHandle: row.PlatformHandle,
		CustomerID:     row.PlatformUserID,
		Status:         row.Status,
		PaymentStatus:  row.PaymentStatus,
		Items:          items,
		TotalItems:     totalItems,
		TotalAmount:    totalAmount,
		PaidAt:         row.PaidAt,
		CreatedAt:      row.CreatedAt,
		ExpiresAt:      row.ExpiresAt,
	}, nil
}

func (s *Service) GetDetailByID(ctx context.Context, id string, storeID string) (*OrderDetailOutput, error) {
	// Get base order
	orderOutput, err := s.GetByID(ctx, id, storeID)
	if err != nil {
		return nil, err
	}

	// Get order detail row for event_id, platform_user_id, and all the extra
	// context captured at checkout (customer, shipping address, store origin).
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Get customer comments for this event
	commentRows, err := s.repo.GetCustomerComments(ctx, row.EventID, row.PlatformUserID)
	if err != nil {
		s.logger.Warn("failed to get customer comments", zap.Error(err))
		commentRows = []CommentRow{}
	}

	comments := make([]CommentOutput, len(commentRows))
	for i, c := range commentRows {
		comments[i] = CommentOutput{
			ID:        c.ID,
			Text:      c.Text,
			CreatedAt: c.CreatedAt,
		}
	}

	out := &OrderDetailOutput{
		OrderOutput: *orderOutput,
		Comments:    comments,
	}

	// Customer: only expose when there's any data to surface.
	if row.CustomerName != "" || row.CustomerEmail != "" || row.CustomerDocument != "" || row.CustomerPhone != "" {
		out.Customer = &OrderCustomerOutput{
			Name:     row.CustomerName,
			Email:    row.CustomerEmail,
			Document: row.CustomerDocument,
			Phone:    row.CustomerPhone,
		}
	}

	// Shipping address (parsed from JSONB): require at least a zip code.
	if row.ShippingAddressZip != "" {
		out.ShippingAddress = &OrderShippingAddressOutput{
			ZipCode:      row.ShippingAddressZip,
			Street:       row.ShippingAddressStreet,
			Number:       row.ShippingAddressNumber,
			Complement:   row.ShippingAddressComplement,
			Neighborhood: row.ShippingAddressNeighborhood,
			City:         row.ShippingAddressCity,
			State:        row.ShippingAddressState,
		}
	}

	// Shipping selection (the freight option the customer picked).
	if row.ShippingServiceID != "" {
		out.Shipping = &OrderShippingOutput{
			Provider:      row.ShippingProvider,
			ServiceID:     row.ShippingServiceID,
			ServiceName:   row.ShippingServiceName,
			Carrier:       row.ShippingCarrier,
			CostCents:     row.ShippingCostCents,
			RealCostCents: row.ShippingCostRealCents,
			DeadlineDays:  row.ShippingDeadlineDays,
			FreeShipping:  row.EventFreeShipping,
		}
	}

	// Store info (always present; derived from the cart's event_id → store_id).
	out.Store = &OrderStoreOutput{
		ID:       row.StoreID,
		Name:     row.StoreName,
		LogoURL:  row.StoreLogoURL,
		Document: row.StoreCNPJ,
		Email:    row.StoreEmail,
		Phone:    row.StorePhone,
		Address: OrderShippingAddressOutput{
			ZipCode:      row.StoreAddressZip,
			Street:       row.StoreAddressStreet,
			Number:       row.StoreAddressNumber,
			Complement:   row.StoreAddressComplement,
			Neighborhood: row.StoreAddressDistrict,
			City:         row.StoreAddressCity,
			State:        row.StoreAddressState,
		},
		PackageWeightGrams: row.StoreDefaultPkgWeightGrams,
		PackageFormat:      row.StoreDefaultPkgFormat,
	}

	// Shipment (may be absent). Events are loaded in a follow-up query.
	shipment, serr := s.repo.GetShipmentForOrder(ctx, id)
	if serr != nil {
		s.logger.Warn("failed to load shipment for order", zap.Error(serr))
	} else if shipment != nil {
		events, eerr := s.repo.ListShipmentEvents(ctx, shipment.ID)
		if eerr != nil {
			s.logger.Warn("failed to list shipment events", zap.Error(eerr))
			events = []OrderShipmentEventRecord{}
		}
		out.Shipment = &OrderShipmentOutput{
			OrderShipmentRecord: *shipment,
			Events:              events,
		}
	}

	return out, nil
}

func (s *Service) Update(ctx context.Context, input UpdateOrderInput) (*OrderOutput, error) {
	// First verify the order exists and belongs to the store
	row, err := s.repo.GetByID(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", input.ID))
	}
	if row.StoreID != input.StoreID {
		return nil, httpx.ErrNotFound(fmt.Sprintf("order %s not found", input.ID))
	}

	// Update status if provided
	if input.Status != nil {
		if err := s.repo.UpdateStatus(ctx, input.ID, *input.Status); err != nil {
			return nil, err
		}
	}

	// Update payment status if provided
	if input.PaymentStatus != nil {
		if err := s.repo.UpdatePaymentStatus(ctx, input.ID, *input.PaymentStatus); err != nil {
			return nil, err
		}
	}

	// Return updated order
	return s.GetByID(ctx, input.ID, input.StoreID)
}

func (s *Service) GetStats(ctx context.Context, storeID string) (*OrderStatsOutput, error) {
	stats, err := s.repo.GetStats(ctx, storeID)
	if err != nil {
		return nil, err
	}
	return stats, nil
}
