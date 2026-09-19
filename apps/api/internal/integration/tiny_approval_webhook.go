package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// Approval means externally paid under the merchant's configured workflow.
// Store that observation before acknowledging; an asynchronous goroutine can
// disappear on deploy or fail after the next status has already arrived.
type TinyApprovalCommand struct {
	StoreID       string `json:"store_id"`
	IntegrationID string `json:"integration_id"`
	OrderID       string `json:"order_id"`
	Kind          string `json:"kind"`
}

func (s *Service) enqueueTinyApproval(ctx context.Context, storeID, orderID string) error {
	row, err := s.repo.GetActiveERP(ctx, storeID)
	if httpx.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Provider != "tiny" {
		return nil
	}
	payload, err := json.Marshal(TinyApprovalCommand{StoreID: storeID, IntegrationID: row.ID, OrderID: orderID, Kind: "order_approved"})
	if err != nil {
		return err
	}
	return s.EmitEvent(ctx, events.Envelope{Name: events.ERPWebhookProcess, Source: events.SourceTiny, DedupKey: "tiny.approval:" + uuid.NewString(), Payload: payload, Metadata: map[string]string{"store_id": storeID, "integration_id": row.ID}})
}

func (s *Service) ProcessTinyApproval(ctx context.Context, command TinyApprovalCommand) error {
	if command.StoreID == "" || command.IntegrationID == "" || command.OrderID == "" || command.Kind != "order_approved" {
		return fmt.Errorf("invalid Tiny approval: %w", asynq.SkipRetry)
	}
	row, err := s.repo.GetActiveERP(ctx, command.StoreID)
	if httpx.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Provider != "tiny" || row.ID != command.IntegrationID {
		return nil
	}
	cartID, err := s.repo.FindCartByExternalOrderID(ctx, command.OrderID, command.StoreID)
	if httpx.IsNotFound(err) || errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx = logger.WithStore(ctx, command.StoreID, "")
	if err := s.ERP().ReflectApprovedPayment(ctx, command.StoreID, cartID, command.OrderID); err != nil {
		return err
	}
	logger.From(ctx, s.logger).Info("Tiny approval processing completed", zap.String("cart_id", cartID), zap.String("external_order_id", command.OrderID))
	return nil
}
