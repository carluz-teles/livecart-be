package erp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"livecart/apps/api/internal/integration/providers"
)

func checkoutOrderFixture(t *testing.T) map[string]any {
	t.Helper()
	var order map[string]any
	if err := json.Unmarshal([]byte(`{
		"id":42,"numeroLoja":"lc-cart-test","data":"2026-09-01","dataSaida":"2026-09-01","dataPrevista":"2026-09-08",
		"contato":{"id":1,"nome":"Buyer"},"totalProdutos":120,"total":125,
		"itens":[{"produto":{"id":1},"quantidade":1,"valor":100,"descricaoDetalhada":"[LiveCart]"},{"produto":{"id":2},"quantidade":1,"valor":20,"descricaoDetalhada":"manual"}],
		"parcelas":[{"valor":125,"dataVencimento":"2026-09-01","formaPagamento":{"id":17},"observacoes":""}],
		"transporte":{"frete":0,"pesoBruto":3.5},"desconto":{"valor":0,"unidade":"REAL"},
		"outrasDespesas":5,"comissoes":[{"vendedor":{"id":99},"aliquota":7}],"observacoesInternas":"Merchant instruction"
	}`), &order); err != nil {
		t.Fatal(err)
	}
	return order
}

func TestBlingCheckoutPreservesMerchantFieldsAndActualPayments(t *testing.T) {
	order := checkoutOrderFixture(t)
	writes := 0
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes++
			if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
				t.Error(err)
			}
			transport := order["transporte"].(map[string]any)
			discount := order["desconto"].(map[string]any)
			order["totalProdutos"] = float64(120)
			order["total"] = 125 + transport["frete"].(float64) - discount["valor"].(float64)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": order}); err != nil {
			t.Error(err)
		}
	})
	b.guardarFormasDePagamento(1, map[int]int64{17: 17, 3: 3})
	paidAt := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	checkout := providers.ERPOrderCheckout{
		FreightCents: 4000, DiscountCents: 1000,
		Shipping: &providers.ERPOrderShipping{Carrier: "Correios", Service: "PAC", CostCents: 7500, DeadlineDays: 5},
		Address:  &providers.ERPShippingAddress{RecipientName: "Buyer", Street: "Rua X", Number: "12", City: "São Paulo", State: "SP", ZipCode: "01001000"},
		Payments: []providers.ERPInstallment{
			{AmountCents: 9000, DueDate: paidAt, Method: "pix", Note: "PAGO PIX checkout-1"},
			{AmountCents: 6000, DueDate: paidAt, Method: "credit_card", Note: "PAGO CARD checkout-2"},
		},
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := b.SyncOrderCheckout(context.Background(), "42", checkout); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 || order["total"] != float64(155) {
		t.Fatalf("checkout did not converge: writes=%d total=%v", writes, order["total"])
	}
	if len(order["itens"].([]any)) != 2 || order["outrasDespesas"] != float64(5) || len(order["comissoes"].([]any)) != 1 {
		t.Fatal("merchant items, commission or expenses were lost")
	}
	parcels := order["parcelas"].([]any)
	if len(parcels) != 3 || parcels[0].(map[string]any)["valor"] != float64(90) || parcels[1].(map[string]any)["valor"] != float64(60) || parcels[2].(map[string]any)["valor"] != float64(5) {
		t.Fatalf("actual paid amounts and merchant-item balance changed: %v", parcels)
	}
	if parcels[0].(map[string]any)["formaPagamento"].(map[string]any)["id"] != float64(17) || parcels[1].(map[string]any)["formaPagamento"].(map[string]any)["id"] != float64(3) {
		t.Fatal("payment methods were not preserved per payment")
	}
	transport := order["transporte"].(map[string]any)
	if transport["frete"] != float64(40) || transport["pesoBruto"] != 3.5 {
		t.Fatalf("freight charged or merchant weight changed: %v", transport)
	}
	if _, exists := transport["volumes"]; exists {
		t.Fatal("display service name cannot be invented as a Bling logistics alias")
	}
	note := order["observacoesInternas"].(string)
	if !strings.Contains(note, "Merchant instruction") || !strings.Contains(note, "75.00") || strings.Count(note, "LiveCart entrega:") != 1 {
		t.Fatalf("merchant notes/actual freight expense missing or duplicated: %s", note)
	}
}

func TestBlingCheckoutBlocksIncorrectReadbackAndUnsafeDiscount(t *testing.T) {
	for _, tc := range []struct {
		name           string
		change         func(map[string]any)
		beforeDiscount bool
	}{
		{name: "same total but payment split changed", change: func(order map[string]any) {
			p := order["parcelas"].([]any)
			p[0].(map[string]any)["valor"] = float64(90)
			p[1].(map[string]any)["valor"] = float64(35)
		}},
		{name: "freight silently ignored", change: func(order map[string]any) { order["transporte"].(map[string]any)["frete"] = float64(20) }},
		{name: "manual discount", beforeDiscount: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := checkoutOrderFixture(t)
			if tc.beforeDiscount {
				order["desconto"].(map[string]any)["valor"] = float64(5)
			}
			writes := 0
			b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					writes++
					if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
						t.Error(err)
					}
					order["total"] = float64(125)
					tc.change(order)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": order}); err != nil {
					t.Error(err)
				}
			})
			b.guardarFormasDePagamento(1, map[int]int64{17: 17})
			err := b.SyncOrderCheckout(context.Background(), "42", providers.ERPOrderCheckout{Payments: []providers.ERPInstallment{{AmountCents: 10000, Method: "pix", DueDate: time.Now(), Note: "PAGO"}}})
			if err == nil {
				t.Fatal("unsafe commercial state was accepted")
			}
			if tc.beforeDiscount && writes != 0 {
				t.Fatal("merchant discount must be rejected before writing")
			}
		})
	}
}

func TestBlingCheckoutFreeShippingAndPickup(t *testing.T) {
	for _, carrier := range []string{"Correios", providers.StorePickupCarrier} {
		t.Run(carrier, func(t *testing.T) {
			order := checkoutOrderFixture(t)
			order["transporte"].(map[string]any)["contato"] = map[string]any{"id": 2, "nome": "Old"}
			b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
						t.Error(err)
					}
					order["total"] = float64(125)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": order}); err != nil {
					t.Error(err)
				}
			})
			b.guardarFormasDePagamento(1, map[int]int64{})
			err := b.SyncOrderCheckout(context.Background(), "42", providers.ERPOrderCheckout{
				Shipping: &providers.ERPOrderShipping{Carrier: carrier, Service: "PAC", CostCents: 7500},
				Payments: []providers.ERPInstallment{{AmountCents: 12500, DueDate: time.Now(), Note: "PAGO"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			transport := order["transporte"].(map[string]any)
			if transport["frete"] != float64(0) {
				t.Fatal("merchant shipping expense was charged to customer")
			}
			if carrier == providers.StorePickupCarrier && (transport["fretePorConta"] != float64(9) || transport["contato"] != nil) {
				t.Fatal("pickup was treated as shipment")
			}
		})
	}
}

func TestBlingInitialCheckoutDoesNotUseRealExpenseAsFreight(t *testing.T) {
	payload, err := blingInitialCheckout(blingPedido{}, providers.ERPOrder{
		Shipping:        &providers.ERPOrderShipping{Carrier: "Correios", Service: "SEDEX", CostCents: 6500},
		ShippingAddress: &providers.ERPShippingAddress{Street: "Rua X"},
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := payload["transporte"].(map[string]any)
	if _, exists := transport["frete"]; exists {
		t.Fatal("initial order invented customer freight from merchant expense")
	}
	if transport["etiqueta"].(map[string]any)["endereco"] != "Rua X" {
		t.Fatal("delivery address discarded")
	}
}

func TestBlingCheckoutDoesNotReplaceMerchantPaymentSchedules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "merchant payment note", change: func(order map[string]any) {
			order["parcelas"].([]any)[0].(map[string]any)["observacoes"] = "Entrada em dinheiro registrada na loja"
		}},
		{name: "financial authorization", change: func(order map[string]any) { order["parcelas"].([]any)[0].(map[string]any)["caut"] = "AUTH123" }},
		{name: "multiple unnamed installments", change: func(order map[string]any) {
			order["parcelas"] = append(order["parcelas"].([]any), map[string]any{"valor": 25, "formaPagamento": map[string]any{"id": 1}})
		}},
		{name: "invoiced order", change: func(order map[string]any) { order["notaFiscal"] = map[string]any{"id": 22} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := checkoutOrderFixture(t)
			tc.change(order)
			writes := 0
			b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": order}); err != nil {
					t.Error(err)
				}
			})
			b.guardarFormasDePagamento(1, map[int]int64{})
			err := b.SyncOrderCheckout(context.Background(), "42", providers.ERPOrderCheckout{Payments: []providers.ERPInstallment{{AmountCents: 12500, DueDate: time.Now(), Note: "PAGO LiveCart — manual checkout-1"}}})
			if err == nil || writes != 0 {
				t.Fatalf("merchant financial records were not protected: error=%v writes=%d", err, writes)
			}
		})
	}
}

func TestBlingCheckoutSupplementalShippingAndDiscountConverge(t *testing.T) {
	order := checkoutOrderFixture(t)
	baseTotal := float64(125)
	writes := 0
	b, _ := bancadaBling(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes++
			if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
				t.Error(err)
			}
			order["totalProdutos"] = baseTotal - 5
			order["total"] = baseTotal + order["transporte"].(map[string]any)["frete"].(float64) - order["desconto"].(map[string]any)["valor"].(float64)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": order}); err != nil {
			t.Error(err)
		}
	})
	b.guardarFormasDePagamento(17, map[int]int64{17: 17})
	checkout := providers.ERPOrderCheckout{Payments: []providers.ERPInstallment{{AmountCents: 12500, DueDate: time.Now(), Method: "pix", Note: "PAGO LiveCart — pix initial"}}}
	if err := b.SyncOrderCheckout(context.Background(), "42", checkout); err != nil {
		t.Fatal(err)
	}
	checkout.FreightCents = 900
	checkout.Payments = append(checkout.Payments, providers.ERPInstallment{AmountCents: 900, DueDate: time.Now(), Method: "pix", Note: "PAGO LiveCart — pix shipping"})
	if err := b.SyncOrderCheckout(context.Background(), "42", checkout); err != nil {
		t.Fatal(err)
	}
	if order["total"] != float64(134) {
		t.Fatalf("supplemental freight missing: %v", order["total"])
	}
	for _, suffix := range []string{"upsell-1", "upsell-2"} {
		baseTotal += 20
		order["totalProdutos"] = baseTotal - 5
		order["total"] = order["total"].(float64) + 20
		checkout.DiscountCents += 200
		checkout.Payments = append(checkout.Payments, providers.ERPInstallment{AmountCents: 1800, DueDate: time.Now(), Method: "pix", Note: "PAGO LiveCart — pix " + suffix})
		if err := b.SyncOrderCheckout(context.Background(), "42", checkout); err != nil {
			t.Fatal(err)
		}
	}
	if order["total"] != float64(170) || len(order["parcelas"].([]any)) != 4 {
		t.Fatalf("supplemental discounts/payments do not close: total=%v installments=%v", order["total"], order["parcelas"])
	}
	if err := b.SyncOrderCheckout(context.Background(), "42", checkout); err != nil {
		t.Fatal(err)
	}
	if writes != 4 {
		t.Fatalf("redelivery repeated checkout write: %d", writes)
	}
}
