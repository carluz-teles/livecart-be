//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"livecart/apps/api/internal/events"
	"livecart/apps/api/internal/integration/providers"
)

func tinyOrderWebhookFixture(t *testing.T) (*Service, finFixture, string) {
	t.Helper()
	requireDB(t)
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(t.Context(), `UPDATE carts SET external_order_id='123456',erp_order_status='aberto' WHERE id=$1`, fx.cartID); err != nil {
		t.Fatal(err)
	}
	var integrationID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM integrations WHERE store_id=$1`, fx.storeID).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	provider := newScriptedERP()
	provider.situacoes["123456"] = providers.SituacaoEnviada
	return newFinalisationService(provider), fx, integrationID
}

func postTinyWebhook(t *testing.T, svc *Service, storeID, body string) int {
	t.Helper()
	h := &WebhookHandler{service: svc, logger: zap.NewNop()}
	app := fiber.New()
	app.Post("/tiny/:storeId", h.HandleTiny)
	req := httptest.NewRequest("POST", "/tiny/"+storeID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestTinyOrderWebhookPersistsDuringOAuthFailureAndProcessesAfterRecovery(t *testing.T) {
	svc, fx, integrationID := tinyOrderWebhookFixture(t)
	if err := testRepo.UpdateStatus(t.Context(), integrationID, "error"); err != nil {
		t.Fatal(err)
	}
	body := `{"tipo":"inclusao_pedido","dados":{"id":"123456","numero":27328,"codigoSituacao":"enviado"}}`
	if status := postTinyWebhook(t, svc, fx.storeID, body); status != 200 {
		t.Fatalf("HTTP %d", status)
	}
	var raw []byte
	if err := testPool.QueryRow(t.Context(), `SELECT payload FROM event_outbox WHERE name=$1 AND payload->>'store_id'=$2`, events.ERPWebhookProcess, fx.storeID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var command TinyOrderWebhookCommand
	if err := json.Unmarshal(raw, &command); err != nil {
		t.Fatal(err)
	}
	if command.Number != "27328" || command.IntegrationID != integrationID || command.OrderID != "123456" {
		t.Fatalf("lost payload: %+v", command)
	}
	if err := svc.ProcessTinyOrderWebhook(t.Context(), command); !errors.Is(err, errTinyWebhookConnectionPending) {
		t.Fatalf("notification lost while disconnected: %v", err)
	}
	if err := testRepo.UpdateStatus(t.Context(), integrationID, "active"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := svc.ProcessTinyOrderWebhook(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	if err := testPool.QueryRow(t.Context(), `SELECT erp_order_status FROM carts WHERE id=$1`, fx.cartID).Scan(&status); err != nil || status != "enviado" {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestTinyWebhookDoesNotAcknowledgeFailedPersistence(t *testing.T) {
	svc, fx, _ := tinyOrderWebhookFixture(t)
	constraint := "reject_tiny_outbox_" + strings.ReplaceAll(fx.storeID, "-", "")
	ddl := fmt.Sprintf(`ALTER TABLE event_outbox ADD CONSTRAINT %s CHECK(payload->>'store_id' IS DISTINCT FROM '%s')`, constraint, fx.storeID)
	if _, err := testPool.Exec(t.Context(), ddl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := testPool.Exec(ctx, "ALTER TABLE event_outbox DROP CONSTRAINT "+constraint); err != nil {
			t.Error(err)
		}
	})
	for _, body := range []string{
		`{"tipo":"atualizacao_pedido","dados":{"id":"123456","codigoSituacao":"enviado"}}`,
		`{"tipo":"nota_fiscal","dados":{"idPedido":"123456","idNotaFiscal":123}}`,
	} {
		if status := postTinyWebhook(t, svc, fx.storeID, body); status != 503 {
			t.Fatalf("persistence failure returned HTTP %d", status)
		}
	}
}

func TestTinyOrderWebhookIgnoresOtherChannelsAndDisconnectedStores(t *testing.T) {
	for _, state := range []string{"disconnected", "pending_auth", "other_channel", "other_provider"} {
		t.Run(state, func(t *testing.T) {
			svc, fx, integrationID := tinyOrderWebhookFixture(t)
			orderID := "123456"
			switch state {
			case "other_channel":
				orderID = "foreign-order"
			case "other_provider":
				if _, err := testPool.Exec(t.Context(), `UPDATE integrations SET provider='bling' WHERE id=$1`, integrationID); err != nil {
					t.Fatal(err)
				}
			default:
				if err := testRepo.UpdateStatus(t.Context(), integrationID, state); err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf(`{"tipo":"atualizacao_pedido","dados":{"id":%q,"codigoSituacao":"aprovado"}}`, orderID)
			if status := postTinyWebhook(t, svc, fx.storeID, body); status != 200 {
				t.Fatalf("HTTP %d", status)
			}
			var count int
			if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox WHERE name=$1 AND payload->>'store_id'=$2`, events.ERPWebhookProcess, fx.storeID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("foreign/disabled event enqueued: %d %v", count, err)
			}
		})
	}
}

func TestTinyStockAndApprovalRemainDurableWhileIntegrationHasError(t *testing.T) {
	svc, fx, integrationID := tinyOrderWebhookFixture(t)
	if err := testRepo.UpdateStatus(t.Context(), integrationID, "error"); err != nil {
		t.Fatal(err)
	}
	if err := svc.enqueueTinyProductWebhook(t.Context(), fx.storeID, "produto", "known-product"); err != nil {
		t.Fatal(err)
	}
	if err := svc.enqueueTinyApproval(t.Context(), fx.storeID, "123456"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM event_outbox WHERE name=$1 AND payload->>'store_id'=$2`, events.ERPWebhookProcess, fx.storeID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("durable commands=%d err=%v", count, err)
	}
	if err := svc.ProcessTinyProductWebhook(t.Context(), TinyProductWebhookCommand{StoreID: fx.storeID, IntegrationID: integrationID, ProductID: "known-product", Kind: "produto"}); !errors.Is(err, errTinyWebhookConnectionPending) {
		t.Fatalf("stock event lost: %v", err)
	}
	if err := svc.ProcessTinyApproval(t.Context(), TinyApprovalCommand{StoreID: fx.storeID, IntegrationID: integrationID, OrderID: "123456", Kind: "order_approved"}); !errors.Is(err, errTinyWebhookConnectionPending) {
		t.Fatalf("approval lost: %v", err)
	}
	if err := testRepo.UpdateStatus(t.Context(), integrationID, "disconnected"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessTinyApproval(t.Context(), TinyApprovalCommand{StoreID: fx.storeID, IntegrationID: integrationID, OrderID: "123456", Kind: "order_approved"}); err != nil {
		t.Fatalf("intentional disconnection kept retrying: %v", err)
	}
}

type tinyWebhookInvoiceProvider struct {
	providers.ERPProvider
	failure    error
	orderReads int
}

func (p *tinyWebhookInvoiceProvider) GetInvoiceByID(context.Context, string) (*providers.ERPInvoice, error) {
	if p.failure != nil {
		return nil, p.failure
	}
	return &providers.ERPInvoice{InvoiceID: "987", Status: providers.ERPInvoiceStatusAuthorized}, nil
}

func (p *tinyWebhookInvoiceProvider) GetInvoiceByOrder(ctx context.Context, id string) (*providers.ERPInvoice, error) {
	p.orderReads++
	return p.GetInvoiceByID(ctx, id)
}

func (p *tinyWebhookInvoiceProvider) GetInvoiceXML(context.Context, string) ([]byte, error) {
	return nil, errors.New("unexpected XML fetch")
}

func TestTinyInvoiceWebhookSurvivesRestartAndProviderFailure(t *testing.T) {
	svc, fx, integrationID := tinyOrderWebhookFixture(t)
	before := paymentLedgerSnapshot(t, fx.cartID)
	if status := postTinyWebhook(t, svc, fx.storeID,
		`{"tipo":"nota_fiscal","dados":{"idPedido":123456,"idNotaFiscal":987}}`); status != 200 {
		t.Fatalf("HTTP %d", status)
	}
	var raw []byte
	if err := testPool.QueryRow(t.Context(), `SELECT payload FROM event_outbox WHERE name=$1 AND payload->>'store_id'=$2`,
		events.ERPWebhookProcess, fx.storeID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var command TinyOrderWebhookCommand
	if err := json.Unmarshal(raw, &command); err != nil {
		t.Fatal(err)
	}
	provider := &tinyWebhookInvoiceProvider{failure: errors.New("temporary invoice outage")}
	restarted := newFinalisationService(provider)
	if err := restarted.ProcessTinyOrderWebhook(t.Context(), command); err == nil {
		t.Fatal("provider failure was acknowledged as success")
	}
	provider.failure = nil
	for range 2 {
		if err := restarted.ProcessTinyOrderWebhook(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	if invoiceStatus(t, fx.cartID) != "authorized" {
		t.Fatal("invoice was not recovered")
	}
	if after := paymentLedgerSnapshot(t, fx.cartID); after != before {
		t.Fatal("invoice recovery altered payments or emitted a new sale")
	}
	// A queued event cannot cross stores, or revive a disconnected integration.
	other := seedPaidCart(t, 1, 0)
	command.StoreID = other.storeID
	if err := restarted.ProcessTinyOrderWebhook(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	command.StoreID = fx.storeID
	if err := testRepo.UpdateStatus(t.Context(), integrationID, "disconnected"); err != nil {
		t.Fatal(err)
	}
	provider.failure = errors.New("must not call disconnected provider")
	if err := restarted.ProcessTinyOrderWebhook(t.Context(), command); err != nil {
		t.Fatal(err)
	}
}

func TestTinyInvoiceWebhookWithoutInvoiceIDResolvesByOrder(t *testing.T) {
	for _, invoiceID := range []string{"", "0"} {
		t.Run("invoice="+invoiceID, func(t *testing.T) {
			svc, fx, _ := tinyOrderWebhookFixture(t)
			body := fmt.Sprintf(`{"tipo":"nota_fiscal","dados":{"idPedido":123456,"idNotaFiscal":%q}}`, invoiceID)
			if invoiceID == "" {
				body = `{"tipo":"nota_fiscal","dados":{"idPedido":123456}}`
			}
			if status := postTinyWebhook(t, svc, fx.storeID, body); status != 200 {
				t.Fatalf("HTTP %d", status)
			}
			var raw []byte
			if err := testPool.QueryRow(t.Context(), `SELECT payload FROM event_outbox WHERE name=$1 AND payload->>'store_id'=$2`,
				events.ERPWebhookProcess, fx.storeID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var command TinyOrderWebhookCommand
			if err := json.Unmarshal(raw, &command); err != nil {
				t.Fatal(err)
			}
			provider := &tinyWebhookInvoiceProvider{}
			if err := newFinalisationService(provider).ProcessTinyOrderWebhook(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			if provider.orderReads != 1 || invoiceStatus(t, fx.cartID) != "authorized" {
				t.Fatalf("invoice not resolved by order: calls=%d", provider.orderReads)
			}
		})
	}
}

func TestTinyQueuedOrderDoesNotApplyObsoleteApprovalOrCancellation(t *testing.T) {
	for _, oldStatus := range []string{"aprovado", "cancelado"} {
		t.Run(oldStatus, func(t *testing.T) {
			svc, fx, integrationID := tinyOrderWebhookFixture(t)
			if _, err := testPool.Exec(t.Context(), `UPDATE carts SET payment_status='pending',status='checkout' WHERE id=$1`, fx.cartID); err != nil {
				t.Fatal(err)
			}
			paymentSnapshot := func() string {
				var snapshot string
				if err := testPool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'payments',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM cart_payments p WHERE p.cart_id=$1),
 'events',(SELECT count(*) FROM event_outbox WHERE payload->>'cart_id'=$1::text))::text`, fx.cartID).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			before := paymentSnapshot()
			command := TinyOrderWebhookCommand{
				StoreID: fx.storeID, IntegrationID: integrationID, OrderID: "123456",
				Kind: "order_observation", EventType: "inclusao_pedido", Status: oldStatus,
			}
			if err := svc.ProcessTinyOrderWebhook(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			var status, payment, remote string
			if err := testPool.QueryRow(t.Context(), `SELECT status,payment_status,erp_order_status FROM carts WHERE id=$1`, fx.cartID).Scan(&status, &payment, &remote); err != nil {
				t.Fatal(err)
			}
			if status != "checkout" || payment != "pending" || remote != "enviado" {
				t.Fatalf("obsolete webhook changed purchase: %s/%s/%s", status, payment, remote)
			}
			if after := paymentSnapshot(); after != before {
				t.Fatal("obsolete webhook changed payment ledger")
			}
		})
	}
}
