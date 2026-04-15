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
			ID:           item.ID,
			ProductID:    item.ProductID,
			ProductName:  item.ProductName,
			ProductImage: item.ProductImage,
			Keyword:      item.ProductKeyword,
			Size:         item.Size,
			Quantity:     item.Quantity,
			UnitPrice:    item.UnitPrice,
			TotalPrice:   itemTotal,
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

	// Get order detail row for event_id and platform_user_id
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

	return &OrderDetailOutput{
		OrderOutput: *orderOutput,
		Comments:    comments,
	}, nil
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
