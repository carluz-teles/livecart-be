package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"livecart/apps/api/internal/events"
)

// ReactWaitlistQueued retries a temporarily blocked FIFO head. Legacy events
// omitted store_id; resolve it through the event instead of abandoning the queue.
func (s *Service) ReactWaitlistQueued(ctx context.Context, task *asynq.Task) error {
	var envelope events.Envelope
	if err := json.Unmarshal(task.Payload(), &envelope); err != nil {
		return asynq.SkipRetry
	}
	var command struct {
		StoreID   string `json:"store_id"`
		EventID   string `json:"event_id"`
		ProductID string `json:"product_id"`
	}
	if err := json.Unmarshal(envelope.Payload, &command); err != nil || command.ProductID == "" {
		return asynq.SkipRetry
	}
	if command.StoreID == "" {
		if command.EventID == "" {
			return asynq.SkipRetry
		}
		if err := s.repo.pool.QueryRow(ctx, `SELECT store_id::text FROM live_events WHERE id=$1`, command.EventID).Scan(&command.StoreID); err != nil {
			return fmt.Errorf("resolving queued request store: %w", err)
		}
	}
	return s.ProcessWaitlistProduct(ctx, command.StoreID, command.ProductID)
}

// ReactStockReleased wakes a queue after a local release too. This catches a
// busy head even when the store has no ERP to send another stock webhook.
func (s *Service) ReactStockReleased(ctx context.Context, task *asynq.Task) error {
	var envelope events.Envelope
	if err := json.Unmarshal(task.Payload(), &envelope); err != nil {
		return asynq.SkipRetry
	}
	var command struct {
		ProductID string `json:"product_id"`
	}
	if err := json.Unmarshal(envelope.Payload, &command); err != nil || command.ProductID == "" {
		return asynq.SkipRetry
	}
	if _, err := uuid.Parse(command.ProductID); err != nil {
		return asynq.SkipRetry
	}
	var storeID string
	err := s.repo.pool.QueryRow(ctx, `SELECT store_id::text FROM products WHERE id=$1`, command.ProductID).Scan(&storeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolving released product store: %w", err)
	}
	return s.ProcessWaitlistProduct(ctx, storeID, command.ProductID)
}
