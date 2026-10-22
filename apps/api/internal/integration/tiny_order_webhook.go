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

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

type TinyOrderWebhookCommand struct {
	StoreID       string          `json:"store_id"`
	IntegrationID string          `json:"integration_id"`
	OrderID       string          `json:"order_id"`
	Kind          string          `json:"kind"`
	EventType     string          `json:"event_type"`
	Status        string          `json:"status"`
	Number        string          `json:"number"`
	InvoiceID     string          `json:"invoice_id"`
	Payload       json.RawMessage `json:"payload"`
}

func (s *Service) enqueueTinyOrderWebhook(ctx context.Context, command TinyOrderWebhookCommand) error {
	row, err := s.tinyWebhookOwner(ctx, command.StoreID)
	if err != nil || row == nil {
		return err
	}
	_, err = s.repo.FindCartByExternalOrderID(ctx, command.OrderID, command.StoreID)
	if httpx.IsNotFound(err) || errors.Is(err, pgx.ErrNoRows) {
		return nil // Orders from other sales channels stay outside LiveCart.
	}
	if err != nil {
		return fmt.Errorf("resolving Tiny order webhook cart: %w", err)
	}
	command.IntegrationID = row.ID
	command.Kind = "order_observation"
	payload, err := json.Marshal(command)
	if err != nil {
		return fmt.Errorf("encoding Tiny order webhook: %w", err)
	}
	// Tiny has no delivery ID. Two saves with identical statuses can carry
	// different items; the idempotent consumer reads the current ERP order.
	return s.EmitEvent(ctx, events.Envelope{
		Name: events.ERPWebhookProcess, Source: events.SourceTiny,
		DedupKey: "tiny.order:" + uuid.NewString(), Payload: payload,
		Metadata: map[string]string{"store_id": row.StoreID, "integration_id": row.ID},
	})
}

func (s *Service) ProcessTinyOrderWebhook(ctx context.Context, command TinyOrderWebhookCommand) error {
	validEvent := command.EventType == "atualizacao_pedido" ||
		command.EventType == "inclusao_pedido" || command.EventType == "nota_fiscal"
	validOwner := command.StoreID != "" && command.IntegrationID != "" && command.OrderID != ""
	if !validEvent || !validOwner || command.Kind != "order_observation" {
		return fmt.Errorf("invalid Tiny order webhook command: %w", asynq.SkipRetry)
	}
	row, err := s.tinyWebhookConsumerOwner(ctx, command.StoreID, command.IntegrationID)
	if err != nil || row == nil {
		return err
	}
	cartID, err := s.repo.FindCartByExternalOrderID(ctx, command.OrderID, command.StoreID)
	if httpx.IsNotFound(err) || errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx = logger.WithStore(ctx, command.StoreID, "")
	invoiceID := command.InvoiceID
	if invoiceID == "0" {
		invoiceID = ""
	}
	if command.EventType == "nota_fiscal" || invoiceID != "" {
		if _, err := s.ERP().SyncCartInvoiceByExternalOrder(ctx, command.StoreID, command.OrderID, invoiceID); err != nil {
			return err
		}
	}
	if command.EventType == "nota_fiscal" {
		return nil
	}
	// Delivery can resume days later, or out of order. Check the current ERP
	// state before a queued approval/cancellation can change a live purchase.
	provider, err := s.erpProviderFor(ctx, row)
	if err != nil {
		return err
	}
	situation, err := provider.GetOrderSituacao(ctx, command.OrderID)
	status, known := providers.ERPOrderStatusFromSituacao(situation)
	if errors.Is(err, providers.ErrOrderNotFound) {
		status, known, err = providers.ERPOrderStatusNaoEncontrado, true, nil
	}
	if err != nil {
		return fmt.Errorf("reading current Tiny webhook order status: %w", err)
	}
	if !known {
		return fmt.Errorf("Tiny webhook order has unknown situation %d", situation)
	}
	if status == providers.ERPOrderStatusAprovado {
		if err := s.ProcessTinyApproval(ctx, TinyApprovalCommand{
			StoreID: command.StoreID, IntegrationID: command.IntegrationID,
			OrderID: command.OrderID, Kind: "order_approved",
		}); err != nil {
			return err
		}
	}
	if err := s.ERP().ObserveOrderStatus(
		ctx, command.StoreID, command.OrderID, command.Number,
		status, erp.StatusSourceWebhook, command.Payload,
	); err != nil {
		return err
	}
	if command.EventType == "atualizacao_pedido" && status != providers.ERPOrderStatusNaoEncontrado {
		return s.reflectTinyOrderWebhook(ctx, cartID, command)
	}
	return nil
}

func (s *Service) reflectTinyOrderWebhook(ctx context.Context, cartID string, command TinyOrderWebhookCommand) error {
	ran, err := s.CoalescerReflexo().Fazer(command.StoreID+"|"+command.OrderID, func() error {
		// Financial discrepancies are persisted by the existing review workflow.
		// They must not prevent the independent item/status projection.
		if _, err := s.RecomporParcelasDoPedidoPago(ctx, cartID, command.StoreID); err != nil {
			logger.From(ctx, s.logger).Warn("Tiny order payment requires review",
				zap.String("cart_id", cartID), zap.Error(err))
		}
		_, err := s.SyncCartFromERPOrder(ctx, cartID, command.StoreID)
		return err
	})
	if err != nil {
		return err
	}
	if !ran {
		return events.NewDeferredError("Tiny order reflection already in progress")
	}
	return nil
}
