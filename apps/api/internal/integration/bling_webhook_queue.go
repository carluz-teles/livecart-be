package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/hibiken/asynq"
	"go.uber.org/zap"

	"livecart/apps/api/internal/erp"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// blingWebhookSecret selects the same application credentials as the provider
// factory. A different private client ID cannot silently inherit the shared
// application's secret when its own secret is missing.
func (s *Service) blingWebhookSecret(integration *IntegrationRow) (string, error) {
	credentials, err := s.decryptCredentials(integration.Credentials)
	if err != nil {
		return "", fmt.Errorf("reading bling webhook application credentials: %w", err)
	}
	privateClientID, _ := credentials.Extra["client_id"].(string)
	privateSecret, _ := credentials.Extra["client_secret"].(string)
	if privateSecret != "" {
		return privateSecret, nil
	}
	if privateClientID != "" && privateClientID != config.BlingClientID.String() {
		return "", fmt.Errorf("private bling application webhook secret is not configured")
	}
	return config.BlingClientSecret.String(), nil
}

// BlingWebhookCommand persists the authenticated envelope and its resolved owner.
// The consumer checks ownership again in case the integration was disconnected
// or replaced while the command waited in the queue.
type BlingWebhookCommand struct {
	StoreID       string        `json:"store_id"`
	IntegrationID string        `json:"integration_id"`
	Envelope      BlingEnvelope `json:"envelope"`
}

func (s *Service) enqueueBlingWebhook(ctx context.Context, integration *IntegrationRow, env BlingEnvelope) error {
	payload, err := json.Marshal(BlingWebhookCommand{integration.StoreID, integration.ID, env})
	if err != nil {
		return fmt.Errorf("encoding bling webhook command: %w", err)
	}
	return s.EmitEvent(ctx, events.Envelope{
		Name: events.ERPWebhookProcess, Source: events.SourceBling,
		DedupKey: "bling.webhook:" + integration.ID + ":" + env.EventID,
		Payload:  payload,
		Metadata: map[string]string{"store_id": integration.StoreID, "integration_id": integration.ID, "provider_event_id": env.EventID},
	})
}

// ProcessBlingWebhook runs synchronously under the queue's timeout. No detached
// goroutine may acknowledge work before its database/API side effects finish.
func (s *Service) ProcessBlingWebhook(ctx context.Context, command BlingWebhookCommand) error {
	env := command.Envelope
	if command.StoreID == "" || command.IntegrationID == "" || env.CompanyID == "" || env.EventID == "" || env.Event == "" {
		return fmt.Errorf("invalid bling webhook command: %w", asynq.SkipRetry)
	}
	ctx = logger.WithStore(ctx, command.StoreID, "")
	log := logger.From(ctx, s.logger).With(zap.String("bling_event", env.Event), zap.String("bling_event_id", env.EventID), zap.String("integration_id", command.IntegrationID))
	integration, err := s.repo.GetActiveERPByAccount(ctx, "bling", env.CompanyID)
	if httpx.IsNotFound(err) || (err == nil && (integration == nil || integration.ID != command.IntegrationID || integration.StoreID != command.StoreID)) {
		log.Warn("bling webhook: integração deixou de pertencer à conta; ignorado")
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolving bling webhook owner: %w", err)
	}
	recurso, action, _ := cortarRecurso(env.Event)
	switch recurso {
	case "stock", "virtual_stock":
		var data BlingStockData
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Produto.ID <= 0 {
			return fmt.Errorf("invalid bling stock payload: %w", asynq.SkipRetry)
		}
		if err := s.observarCapacidadeDeReserva(ctx, integration, int(data.SaldoFisicoTotal), int(data.SaldoVirtualTotal)); err != nil {
			return err
		}
		id := strconv.FormatInt(data.Produto.ID, 10)
		// Read the current available balance: Bling delivery may be out of order.
		applied, err := s.ProcessProductWebhook(ctx, integration.StoreID, "bling", id)
		if err != nil {
			return fmt.Errorf("processing bling stock webhook: %w", err)
		}
		if applied {
			if err := s.ProcessWaitlistAfterStockWebhook(ctx, integration.StoreID, "bling", id); err != nil {
				return fmt.Errorf("processing bling stock waitlist: %w", err)
			}
		}
	case "product":
		id, err := blingResourceID(env.Data)
		if err != nil {
			return fmt.Errorf("invalid bling product payload (%v): %w", err, asynq.SkipRetry)
		}
		if action == "deleted" {
			// Keep historical cart references; deleted ERP products stop being sold.
			if err := s.repo.deactivateDeletedBlingProduct(ctx, integration.StoreID, id); err != nil {
				return err
			}
			log.Info("bling webhook: produto excluído desativado localmente", zap.String("external_product_id", id))
		} else if _, err := s.ProcessProductWebhook(ctx, integration.StoreID, "bling", id); err != nil {
			return fmt.Errorf("processing bling product webhook: %w", err)
		}
	case "order":
		if _, err := blingResourceID(env.Data); err != nil {
			return fmt.Errorf("invalid bling order payload (%v): %w", err, asynq.SkipRetry)
		}
		if err := s.despacharPedidoBling(ctx, integration, env); err != nil {
			return fmt.Errorf("processing bling order webhook: %w", err)
		}
	case "invoice", "consumer_invoice":
		// data.id is an invoice ID, never a sales-order ID. Without an explicit
		// mapping, querying /pedidos/vendas/{invoiceID} can affect another order.
		log.Warn("bling webhook: nota fiscal sem mapeamento de pedido; acompanhamento permanece no evento de pedido", zap.String("outcome", "unsupported_invoice_mapping"))
		return nil
	default:
		log.Debug("bling webhook: recurso sem tratamento")
	}
	log.Info("bling webhook: processamento concluído")
	return nil
}

// Narrow JSONB merge preserves unrelated settings updated concurrently.
func (r *Repository) recordBlingReservationCapability(ctx context.Context, integrationID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE integrations
		SET metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object($2::text, true)
		WHERE id = $1::uuid AND provider = 'bling'`, integrationID, erp.ChaveCapacidadeDeReserva)
	if err != nil {
		return fmt.Errorf("recording bling reservation capability: %w", err)
	}
	return nil
}

func (r *Repository) deactivateDeletedBlingProduct(ctx context.Context, storeID, externalID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE products SET active = false, stock = 0,
		erp_seq = erp_seq + 1, updated_at = now()
		WHERE store_id = $1::uuid AND external_source = 'bling' AND external_id = $2
		AND (active OR stock <> 0)`, storeID, externalID)
	if err != nil {
		return fmt.Errorf("deactivating deleted bling product: %w", err)
	}
	return nil
}
