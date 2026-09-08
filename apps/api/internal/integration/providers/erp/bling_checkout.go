package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

// blingInitialCheckout adds delivery information without confusing the real
// carrier expense with freight charged to the customer. The paid checkout
// supplies the latter through SyncOrderCheckout.
func blingInitialCheckout(p blingPedido, order providers.ERPOrder) (map[string]any, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("bling: encoding initial order: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("bling: preparing initial order: %w", err)
	}
	blingApplyDelivery(raw, order.Shipping, order.ShippingAddress, nil)
	return raw, nil
}

func blingApplyDelivery(raw map[string]any, shipping *providers.ERPOrderShipping, address *providers.ERPShippingAddress, freight *int64) {
	if shipping == nil && address == nil && freight == nil {
		return
	}
	transport, _ := raw["transporte"].(map[string]any)
	if transport == nil {
		transport = map[string]any{}
	}
	if freight != nil {
		transport["frete"] = float64(*freight) / 100
	}
	if shipping != nil {
		if shipping.Carrier == providers.StorePickupCarrier {
			transport["fretePorConta"] = 9
			transport["frete"] = float64(0)
			delete(transport, "contato")
			delete(transport, "etiqueta")
			delete(transport, "volumes")
			address = nil
		} else {
			// The merchant contracts the carrier, even if the customer paid a
			// shipping fee at checkout. Charging freight does not imply FOB.
			transport["fretePorConta"] = 0
			if shipping.Carrier != "" {
				contact, _ := transport["contato"].(map[string]any)
				if name, _ := contact["nome"].(string); !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(shipping.Carrier)) {
					contact = map[string]any{"nome": shipping.Carrier}
				}
				transport["contato"] = contact
			}
			if shipping.DeadlineDays > 0 {
				transport["prazoEntrega"] = shipping.DeadlineDays
			}
		}
		// volumes[].servico expects a Bling logistics alias, not a quote's
		// display label ("PAC", "SEDEX"). Keep that label and the real cost
		// informative without fabricating an alias or another receivable.
		const prefix = "LiveCart entrega: "
		note := fmt.Sprintf("%s%s / %s; custo transportadora R$ %.2f", prefix,
			shipping.Carrier, shipping.Service, float64(shipping.CostCents)/100)
		previous, _ := raw["observacoesInternas"].(string)
		lines := strings.Split(previous, "\n")
		kept := make([]string, 0, len(lines)+1)
		for _, line := range lines {
			if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, prefix) {
				kept = append(kept, line)
			}
		}
		raw["observacoesInternas"] = strings.Join(append(kept, note), "\n")
	}
	if address != nil {
		transport["etiqueta"] = map[string]any{
			"nome": address.RecipientName, "endereco": address.Street,
			"numero": address.Number, "complemento": address.Complement,
			"bairro": address.Neighborhood, "municipio": address.City,
			"uf": address.State, "cep": address.ZipCode,
		}
	}
	raw["transporte"] = transport
}

func blingMoney(value any) int64 {
	number, _ := value.(float64)
	return int64(math.Round(number * 100))
}

func blingCommercialDiscount(raw map[string]any) int64 {
	discount, _ := raw["desconto"].(map[string]any)
	if discount["unidade"] == "PERCENTUAL" {
		percent, _ := discount["valor"].(float64)
		return int64(math.Round(float64(blingMoney(raw["totalProdutos"])) * percent / 100))
	}
	return blingMoney(discount["valor"])
}

// GetOrderCommercialDiscount lets the shared ledger distinguish an actual
// order discount from Tiny's legacy informational discount installment.
func (b *Bling) GetOrderCommercialDiscount(ctx context.Context, orderID string) (int64, error) {
	_, raw, err := b.pedido(ctx, orderID)
	if err != nil {
		return 0, err
	}
	return blingCommercialDiscount(raw), nil
}

// SyncOrderCheckout sends one full-document PUT with freight, discount,
// delivery address and the real ledger. It preserves items, commission,
// expenses and other merchant fields from a fresh GET, then verifies the
// financial result before the caller is allowed to approve the order.
func (b *Bling) SyncOrderCheckout(ctx context.Context, orderID string, checkout providers.ERPOrderCheckout) error {
	if checkout.FreightCents < 0 || checkout.DiscountCents < 0 || len(checkout.Payments) == 0 {
		return fmt.Errorf("bling: checkout requires nonnegative freight/discount and a payment ledger")
	}
	if checkout.Shipping != nil && checkout.Shipping.Carrier == providers.StorePickupCarrier && checkout.FreightCents != 0 {
		return fmt.Errorf("bling: pickup checkout cannot carry a freight charge")
	}
	before, raw, err := b.pedido(ctx, orderID)
	if err != nil {
		return err
	}
	if before.NotaFiscal != nil && before.NotaFiscal.ID != 0 {
		return fmt.Errorf("bling: invoiced order requires manual commercial reconciliation")
	}
	oldDiscount := blingCommercialDiscount(raw)
	if oldDiscount != 0 && oldDiscount != checkout.DiscountCents && !blingOwnsCommercialDiscount(raw, oldDiscount) {
		return fmt.Errorf("bling: existing commercial discount differs from checkout; refusing to overwrite merchant discount")
	}
	transport, _ := raw["transporte"].(map[string]any)
	expected := int64(math.Round(before.Total*100)) - blingMoney(transport["frete"]) + checkout.FreightCents + oldDiscount - checkout.DiscountCents
	var paid int64
	for _, payment := range checkout.Payments {
		if payment.AmountCents <= 0 {
			return fmt.Errorf("bling: checkout ledger contains a nonpositive payment")
		}
		paid += payment.AmountCents
	}
	if expected < paid {
		return fmt.Errorf("bling: checkout total %d is below ledger payments %d; reconciliation required", expected, paid)
	}
	payments := append([]providers.ERPInstallment(nil), checkout.Payments...)
	if expected > paid {
		payments = append(payments, providers.ERPInstallment{
			AmountCents: expected - paid, DueDate: time.Now().AddDate(0, 0, 7), Note: "A PAGAR — saldo não coberto pelos pagamentos LiveCart",
		})
	}
	installments := make([]any, 0, len(payments))
	for _, payment := range payments {
		form, err := b.formaPagamentoPara(ctx, payment.Method)
		if err != nil {
			return err
		}
		installments = append(installments, map[string]any{
			"valor":          float64(payment.AmountCents) / 100,
			"dataVencimento": payment.DueDate.In(blingLocation).Format("2006-01-02"),
			"observacoes":    payment.Note, "formaPagamento": map[string]any{"id": form},
		})
	}
	originalJSON, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("bling: preserving checkout snapshot: %w", err)
	}
	var original map[string]any
	if err := json.Unmarshal(originalJSON, &original); err != nil {
		return fmt.Errorf("bling: copying checkout snapshot: %w", err)
	}
	blingApplyDelivery(raw, checkout.Shipping, checkout.Address, &checkout.FreightCents)
	if checkout.DiscountCents != 0 || blingOwnsCommercialDiscount(original, oldDiscount) {
		blingRecordCommercialDiscount(raw, checkout.DiscountCents)
	}
	raw["desconto"] = map[string]any{"valor": float64(checkout.DiscountCents) / 100, "unidade": "REAL"}
	raw["parcelas"] = installments
	wantTransport := raw["transporte"]
	if verifyBlingCheckout(before, original, checkout, payments, installments, wantTransport, expected) == nil && original["observacoesInternas"] == raw["observacoesInternas"] {
		logger.From(ctx, b.Logger).Info("bling checkout already synchronized", zap.String("external_order_id", orderID))
		return nil
	}
	if err := blingCheckoutCanReplaceInstallments(original); err != nil {
		return err
	}
	limparReadOnly(raw)
	if err := b.escrever(ctx, http.MethodPut, "/pedidos/vendas/"+url.PathEscape(orderID), raw, nil); err != nil {
		return fmt.Errorf("bling: updating checkout: %w", err)
	}
	after, verified, err := b.pedido(ctx, orderID)
	if err != nil {
		return fmt.Errorf("bling: checkout sent but readback failed: %w", err)
	}
	if err := verifyBlingCheckout(after, verified, checkout, payments, installments, wantTransport, expected); err != nil {
		return err
	}
	logger.From(ctx, b.Logger).Info("bling checkout synchronized and verified",
		zap.String("external_order_id", orderID), zap.Int64("freight_cents", checkout.FreightCents),
		zap.Int64("discount_cents", checkout.DiscountCents), zap.Int64("total_cents", expected),
		zap.Int64("paid_cents", paid), zap.Int("installments", len(payments)))
	return nil
}

const blingDiscountNotePrefix = "LiveCart desconto aplicado: "

func blingOwnsCommercialDiscount(raw map[string]any, amount int64) bool {
	notes, _ := raw["observacoesInternas"].(string)
	found := false
	for _, line := range strings.Split(notes, "\n") {
		value, ok := strings.CutPrefix(line, blingDiscountNotePrefix)
		if !ok {
			continue
		}
		if found {
			return false
		}
		cents, err := strconv.ParseInt(strings.TrimSuffix(value, " centavos"), 10, 64)
		if err != nil || cents != amount {
			return false
		}
		found = true
	}
	return found
}

func blingRecordCommercialDiscount(raw map[string]any, amount int64) {
	notes, _ := raw["observacoesInternas"].(string)
	lines := make([]string, 0)
	for _, line := range strings.Split(notes, "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, blingDiscountNotePrefix) {
			lines = append(lines, line)
		}
	}
	lines = append(lines, fmt.Sprintf("%s%d centavos", blingDiscountNotePrefix, amount))
	raw["observacoesInternas"] = strings.Join(lines, "\n")
}

func blingCheckoutCanReplaceInstallments(raw map[string]any) error {
	parcels, _ := raw["parcelas"].([]any)
	for _, parcel := range parcels {
		row, _ := parcel.(map[string]any)
		if code, _ := row["caut"].(string); strings.TrimSpace(code) != "" {
			return fmt.Errorf("bling: installment has a financial authorization code; automatic replacement requires reconciliation")
		}
		note, _ := row["observacoes"].(string)
		note = strings.TrimSpace(note)
		// A newly created order has one unnamed commitment. Existing rows
		// bearing unknown notes or multiple unnamed installments may belong
		// to the merchant; do not silently replace their financial schedule.
		known := strings.HasPrefix(note, "PAGO LiveCart — ") || strings.HasPrefix(note, "PAGO — ") ||
			strings.HasPrefix(note, "PAGO PIX em ") || strings.HasPrefix(note, "PAGO CREDIT_CARD em ") ||
			strings.HasPrefix(note, "PAGO BOLETO em ") || strings.HasPrefix(note, "PAGO DEBIT_CARD em ") ||
			strings.HasPrefix(note, "A PAGAR") || note == "DESCONTO concedido (cupom/PIX) - nao cobrar"
		if !known && !(note == "" && len(parcels) == 1) {
			return fmt.Errorf("bling: existing installments are not identified as LiveCart; preserving merchant payment schedule")
		}
	}
	return nil
}

func verifyBlingCheckout(after *blingPedido, verified map[string]any, checkout providers.ERPOrderCheckout, payments []providers.ERPInstallment, installments []any, wantTransport any, expected int64) error {
	actualTransport, _ := verified["transporte"].(map[string]any)
	if int64(math.Round(after.Total*100)) != expected || blingMoney(actualTransport["frete"]) != checkout.FreightCents || blingCommercialDiscount(verified) != checkout.DiscountCents {
		return fmt.Errorf("bling: checkout readback differs in total, freight or discount; approval blocked")
	}
	if len(after.Parcelas) != len(payments) {
		return fmt.Errorf("bling: checkout installment count changed; approval blocked")
	}
	for i, got := range after.Parcelas {
		want := installments[i].(map[string]any)
		wantForm := want["formaPagamento"].(map[string]any)["id"].(int64)
		if int64(math.Round(got.Valor*100)) != payments[i].AmountCents || got.FormaPagamento.ID != wantForm || got.Observacoes != payments[i].Note || got.DataVencimento != want["dataVencimento"] {
			return fmt.Errorf("bling: checkout installment %d changed; approval blocked", i+1)
		}
	}
	if checkout.Address != nil && (checkout.Shipping == nil || checkout.Shipping.Carrier != providers.StorePickupCarrier) {
		want := wantTransport.(map[string]any)["etiqueta"].(map[string]any)
		got, _ := actualTransport["etiqueta"].(map[string]any)
		for _, field := range []string{"nome", "endereco", "numero", "complemento", "bairro", "municipio", "uf"} {
			wantString, _ := want[field].(string)
			gotString, _ := got[field].(string)
			if !strings.EqualFold(strings.TrimSpace(wantString), strings.TrimSpace(gotString)) {
				return fmt.Errorf("bling: checkout delivery address changed; approval blocked")
			}
		}
		wantZIP, _ := want["cep"].(string)
		gotZIP, _ := got["cep"].(string)
		if somenteDigitos(wantZIP) != somenteDigitos(gotZIP) {
			return fmt.Errorf("bling: checkout delivery ZIP changed; approval blocked")
		}
	}
	if shipping := checkout.Shipping; shipping != nil && shipping.Carrier != "" {
		if shipping.Carrier == providers.StorePickupCarrier {
			if actualTransport["fretePorConta"] != float64(9) {
				return fmt.Errorf("bling: checkout pickup mode changed; approval blocked")
			}
		} else {
			contact, _ := actualTransport["contato"].(map[string]any)
			name, _ := contact["nome"].(string)
			if !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(shipping.Carrier)) {
				return fmt.Errorf("bling: checkout carrier changed; approval blocked")
			}
		}
	}
	return nil
}
