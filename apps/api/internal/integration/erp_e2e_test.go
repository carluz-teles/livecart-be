//go:build e2e

package integration

// E2E do design C contra o Tiny REAL (conta de teste ADABYTE) usando o código
// de produção: provider Tiny de verdade + Service + Postgres local. O fluxo
// completo live→conversão→mutação→confirm→refund roda num produto real e o
// teste se auto-limpa (o refund devolve o saldo ao valor inicial).
//
// Rodar:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL='postgres://livecart:livecart@localhost:5432/livecart?sslmode=disable' \
//	TINY_E2E_TOKEN='<access token>' TINY_E2E_PRODUCT='357281337' \
//	go test -tags e2e -count=1 -v -run TestE2E ./apps/api/internal/integration/
//
// Sem as envs o teste dá Skip. Mantenha a bridge de webhooks apontada para o
// túnel → localhost:9099 para capturar os webhooks disparados pelo fluxo.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/integration/providers/erp"
)

// stubProductSyncer resolve o vínculo produto→ERP direto do banco de teste.
type stubProductSyncer struct{}

func (stubProductSyncer) HasProduct(ctx context.Context, storeID, externalID, externalSource string) (bool, error) {
	return true, nil
}

func (stubProductSyncer) GetProduct(ctx context.Context, storeID, productID string) (string, string, error) {
	var ext string
	err := testPool.QueryRow(ctx, `SELECT COALESCE(external_id,'') FROM products WHERE id = $1`, productID).Scan(&ext)
	return ext, "tiny", err
}

func (stubProductSyncer) FilterRegisteredExternalIDs(ctx context.Context, storeID, externalSource string, externalIDs []string) ([]string, error) {
	return nil, nil
}

func (stubProductSyncer) SyncProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct, skipStock, downgradeOnly bool) error {
	return nil
}

func (stubProductSyncer) ImportProduct(ctx context.Context, storeID, externalSource string, product providers.ERPProduct) (string, error) {
	return "", fmt.Errorf("not implemented in e2e stub")
}

func tinyGET(t *testing.T, token, path string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://api.tiny.com.br/public-api/v3"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	return body
}

func TestE2EOrderAsReservationAgainstRealTiny(t *testing.T) {
	requireDB(t)
	token := os.Getenv("TINY_E2E_TOKEN")
	if token == "" {
		t.Skip("TINY_E2E_TOKEN não setado")
	}
	extProduct := os.Getenv("TINY_E2E_PRODUCT")
	if extProduct == "" {
		extProduct = "357281337" // PS5 da conta ADABYTE
	}
	ctx := context.Background()

	// Provider Tiny REAL com o token da conta de teste, injetado pelo seam.
	logger, _ := zap.NewDevelopment()
	realTiny, err := erp.NewTiny(erp.TinyConfig{
		IntegrationID: "e2e",
		StoreID:       "e2e",
		Credentials:   &providers.Credentials{AccessToken: token},
		Logger:        logger,
	})
	if err != nil {
		t.Fatalf("NewTiny: %v", err)
	}
	svc := &Service{
		repo:          testRepo,
		logger:        logger,
		productSyncer: stubProductSyncer{},
		erpProviderFactory: func(ctx context.Context, integration *IntegrationRow) (providers.ERPProvider, error) {
			return realTiny, nil
		},
	}

	// Fixture local apontando para o produto REAL do Tiny.
	fx := seedPaidCart(t, 1, 0)
	if _, err := testPool.Exec(ctx,
		`UPDATE products SET external_id = $2 WHERE id = $1`, fx.productID, extProduct); err != nil {
		t.Fatalf("vinculando produto real: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE carts SET payment_status = 'pending', paid_at = NULL,
		        customer_name = 'Cliente E2E LiveCart', customer_email = 'e2e@livecart.com.br'
		 WHERE id = $1`, fx.cartID); err != nil {
		t.Fatalf("preparando cart: %v", err)
	}

	saldo := func(label string) int {
		p, err := realTiny.GetProduct(ctx, extProduct)
		if err != nil {
			t.Fatalf("GetProduct (%s): %v", label, err)
		}
		t.Logf("SALDO %-28s = %d", label, p.Stock)
		return p.Stock
	}
	saldo0 := saldo("inicial")

	// ── FASE LIVE: reserva por saída manual (modelo legado, pré-conversão) ──
	if err := svc.ReserveStockInERP(ctx, fx.storeID, fx.cartID, fx.eventID, fx.productID, 1, 1000, "@e2e"); err != nil {
		t.Fatalf("reserva da live: %v", err)
	}
	if got := saldo("após reserva manual (live)"); got != saldo0-1 {
		t.Fatalf("saída manual não baixou o saldo (esperado %d)", saldo0-1)
	}
	if n := activeReservationCount(t, fx.cartID); n != 1 {
		t.Fatalf("reserva local ausente (n=%d)", n)
	}

	// ── INICIAÇÃO: conversão em pedido-como-reserva ──
	if err := svc.EnsureERPOrderForCart(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("conversão: %v", err)
	}
	state, launched, orderID := cartOrderState(t, fx.cartID)
	if state != "open" || !launched || orderID == "" {
		t.Fatalf("pós-conversão: state=%q launched=%v orderID=%q", state, launched, orderID)
	}
	t.Logf("PEDIDO criado no Tiny: id=%s", orderID)
	// Líquido zero: launch (-1) + estorno da reserva manual (+1). O saldo fez
	// um MERGULHO transitório, nunca um pico.
	if got := saldo("após conversão (launch+estorno)"); got != saldo0-1 {
		t.Fatalf("conversão desequilibrou o saldo (esperado %d)", saldo0-1)
	}
	if n := activeReservationCount(t, fx.cartID); n != 0 {
		t.Fatalf("reserva manual não foi estornada (n=%d)", n)
	}
	var pedido struct {
		Situacao int `json:"situacao"`
		Itens    []struct {
			Quantidade float64 `json:"quantidade"`
		} `json:"itens"`
	}
	_ = json.Unmarshal(tinyGET(t, token, "/pedidos/"+orderID), &pedido)
	if pedido.Situacao != 0 {
		t.Fatalf("pedido deveria estar Aberta (situacao=%d)", pedido.Situacao)
	}
	if marcadores := string(tinyGET(t, token, "/pedidos/"+orderID+"/marcadores")); !strings.Contains(marcadores, "lc-cart-"+fx.cartID) {
		t.Fatalf("marcador lc-cart ausente: %s", marcadores)
	}
	t.Logf("PEDIDO %s: situação Aberta ✓, marcador lc-cart ✓", orderID)

	// ── MUTAÇÃO NO CHECKOUT: qty 1→2 via ciclo estornar→PUT→lançar ──
	if _, err := testPool.Exec(ctx,
		`UPDATE cart_items SET quantity = 2 WHERE cart_id = $1`, fx.cartID); err != nil {
		t.Fatalf("update qty: %v", err)
	}
	if _, err := svc.AdjustStockReservationDelta(ctx, fx.storeID, fx.cartID, fx.eventID, fx.productID, 1, 1000, "@e2e"); err != nil {
		t.Fatalf("mutação: %v", err)
	}
	if got := saldo("após mutação qty 1→2"); got != saldo0-2 {
		t.Fatalf("ciclo de mutação não refletiu a grade nova (esperado %d)", saldo0-2)
	}
	_ = json.Unmarshal(tinyGET(t, token, "/pedidos/"+orderID), &pedido)
	if len(pedido.Itens) != 1 || pedido.Itens[0].Quantidade != 2 {
		t.Fatalf("grade do pedido não atualizou: %+v", pedido.Itens)
	}
	t.Logf("PEDIDO %s: grade atualizada para qty=2 ✓", orderID)

	// ── PAGO: confirm = 2 PUTs, zero movimentação ──
	paidAt := time.Now()
	if err := svc.ConfirmERPOrderPayment(ctx, fx.cartID, fx.storeID, &providers.PaymentStatus{
		PaymentID:     "E2E-PAY-1",
		PaymentMethod: "pix",
		Amount:        2000,
		PaidAt:        &paidAt,
	}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got := saldo("após confirm (deve ser IGUAL)"); got != saldo0-2 {
		t.Fatalf("confirm moveu estoque! (esperado %d)", saldo0-2)
	}
	raw := tinyGET(t, token, "/pedidos/"+orderID)
	_ = json.Unmarshal(raw, &pedido)
	if pedido.Situacao != 3 {
		t.Fatalf("pedido deveria estar Aprovada (situacao=%d)", pedido.Situacao)
	}
	if !strings.Contains(string(raw), "parcelas") {
		t.Fatalf("parcelas ausentes no pedido confirmado")
	}
	state, _, _ = cartOrderState(t, fx.cartID)
	if state != "confirmed" {
		t.Fatalf("state = %q", state)
	}
	t.Logf("PEDIDO %s: Aprovada ✓, parcelas gravadas ✓, saldo intocado ✓", orderID)

	// ── REFUND (e cleanup): situação 2 → estorno → saldo volta ao inicial ──
	if err := svc.RefundConvertedCartOrder(ctx, fx.cartID, fx.storeID); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if got := saldo("após refund (volta ao inicial)"); got != saldo0 {
		t.Fatalf("refund não devolveu o saldo (esperado %d)", saldo0)
	}
	_ = json.Unmarshal(tinyGET(t, token, "/pedidos/"+orderID), &pedido)
	if pedido.Situacao != 2 {
		t.Fatalf("pedido deveria estar Cancelada (situacao=%d)", pedido.Situacao)
	}
	state, _, _ = cartOrderState(t, fx.cartID)
	if state != "cancelled" {
		t.Fatalf("state final = %q", state)
	}
	t.Logf("E2E COMPLETO: live→conversão→mutação→confirm→refund contra o Tiny real, saldo restaurado a %d", saldo0)
}
