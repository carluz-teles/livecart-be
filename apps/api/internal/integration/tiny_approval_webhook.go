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
	"livecart/apps/api/internal/cartedit"
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
	row, err := s.tinyWebhookOwner(ctx, storeID)
	if err != nil {
		return err
	}
	if row == nil {
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
	row, err := s.tinyWebhookConsumerOwner(ctx, command.StoreID, command.IntegrationID)
	if err != nil {
		return err
	}
	if row == nil {
		return nil
	}
	cartID, err := s.repo.FindCartByExternalOrderID(ctx, command.OrderID, command.StoreID)
	if httpx.IsNotFound(err) || errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	state, err := cartedit.Read(ctx, s.repo.pool, cartID)
	if err != nil {
		return err
	}
	deferred, err := s.repo.tinyApprovalDeferred(ctx, cartID, command)
	if err != nil {
		return err
	}
	if state.Pending || deferred {
		return s.repo.deferTinyApproval(ctx, cartID, command)
	}
	ctx = logger.WithStore(ctx, command.StoreID, "")
	if err := s.ERP().ReflectApprovedPayment(ctx, command.StoreID, cartID, command.OrderID); err != nil {
		var domainErr *httpx.ServiceError
		if errors.As(err, &domainErr) && domainErr.Reason == string(httpx.CodeCartERPSyncPending) {
			return s.repo.deferTinyApproval(ctx, cartID, command)
		}
		return err
	}
	logger.From(ctx, s.logger).Info("Tiny approval processing completed", zap.String("cart_id", cartID), zap.String("external_order_id", command.OrderID))
	return nil
}
