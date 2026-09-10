package erp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"livecart/apps/api/internal/integration/providers"
)

type tinyCheckoutInstallment struct {
	Value float64 `json:"valor"`
	Date  string  `json:"data"`
	Note  string  `json:"observacoes"`
}

type tinyCheckoutOrder struct {
	InvoiceID int64   `json:"idNotaFiscal"`
	Total     float64 `json:"valorTotalPedido"`
	Freight   float64 `json:"valorFrete"`
	Discount  float64 `json:"valorDesconto"`
	Customer  struct {
		Name     string `json:"nome"`
		Document string `json:"cpfCnpj"`
		Email    string `json:"email"`
		Phone    string `json:"telefone"`
		Mobile   string `json:"celular"`
	} `json:"cliente"`
	Address struct {
		Street       string `json:"endereco"`
		Number       string `json:"numero"`
		Complement   string `json:"complemento"`
		Neighborhood string `json:"bairro"`
		City         string `json:"municipio"`
		State        string `json:"uf"`
		Zip          string `json:"cep"`
	} `json:"enderecoEntrega"`
	Payment struct {
		Installments []tinyCheckoutInstallment `json:"parcelas"`
	} `json:"pagamento"`
}

func (t *Tiny) readCheckoutOrder(ctx context.Context, orderID string) (*tinyCheckoutOrder, error) {
	endpoint := fmt.Sprintf("%s/pedidos/%s", tinyAPIBaseURL, orderID)
	resp, body, err := t.DoRequestRetrying429(ctx, 2, http.MethodGet, endpoint, nil, t.authHeaders())
	if err != nil {
		return nil, fmt.Errorf("tiny: reading checkout: %w", err)
	}
	if !providers.IsSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("tiny: reading checkout returned status %d", resp.StatusCode)
	}
	var order tinyCheckoutOrder
	if err := json.Unmarshal(body, &order); err != nil {
		return nil, fmt.Errorf("tiny: decoding checkout: %w", err)
	}
	return &order, nil
}

// Tiny v3's update endpoint cannot apply customer/delivery/freight fields.
// Verify them instead of reporting a successful item-only payment. Divergence
// remains in the existing finalisation queue for explicit reconciliation.
func (t *Tiny) SyncOrderCheckout(ctx context.Context, orderID string, checkout providers.ERPOrderCheckout) error {
	order, err := t.readCheckoutOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if order.InvoiceID != 0 {
		return fmt.Errorf("tiny: checkout requer conciliação: pedido com nota fiscal")
	}
	fields := []string{}
	if checkout.Customer.Phone != "" && digitsOnlyTiny(order.Customer.Phone) != digitsOnlyTiny(checkout.Customer.Phone) && digitsOnlyTiny(order.Customer.Mobile) != digitsOnlyTiny(checkout.Customer.Phone) {
		fields = append(fields, "telefone do cliente")
	}
	if int64(math.Round(order.Freight*100)) != checkout.FreightCents {
		fields = append(fields, "frete")
	}
	if int64(math.Round(order.Discount*100)) != checkout.DiscountCents {
		fields = append(fields, "desconto")
	}
	if checkout.Customer.Name != "" && strings.TrimSpace(order.Customer.Name) != strings.TrimSpace(checkout.Customer.Name) {
		fields = append(fields, "nome do cliente")
	}
	if checkout.Customer.CpfCnpj != "" && digitsOnlyTiny(order.Customer.Document) != digitsOnlyTiny(checkout.Customer.CpfCnpj) {
		fields = append(fields, "documento do cliente")
	}
	if checkout.Customer.Email != "" && !strings.EqualFold(order.Customer.Email, checkout.Customer.Email) {
		fields = append(fields, "email do cliente")
	}
	if a := checkout.Address; a != nil {
		if order.Address.Street != a.Street || order.Address.Number != a.Number ||
			order.Address.Complement != a.Complement || order.Address.Neighborhood != a.Neighborhood ||
			order.Address.City != a.City || order.Address.State != a.State ||
			digitsOnlyTiny(order.Address.Zip) != digitsOnlyTiny(a.ZipCode) {
			fields = append(fields, "endereço de entrega")
		}
	}
	var paid int64
	for _, payment := range checkout.Payments {
		paid += payment.AmountCents
	}
	if len(checkout.Payments) == 0 || int64(math.Round(order.Total*100)) != paid {
		fields = append(fields, "total pago/desconto")
	}
	if len(fields) > 0 {
		return fmt.Errorf("tiny: checkout requer conciliação de %s; API v3 não permite atualizar esses campos no pedido existente", strings.Join(fields, ", "))
	}
	if err := t.SetOrderInstallments(ctx, orderID, checkout.Payments); err != nil {
		return err
	}
	verified, err := t.readCheckoutOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if verified.Total != order.Total || verified.Freight != order.Freight || verified.Discount != order.Discount ||
		verified.Customer != order.Customer || verified.Address != order.Address {
		return fmt.Errorf("tiny: dados comerciais alterados durante a confirmação; conciliação necessária")
	}
	if !tinyInstallmentsMatch(verified.Payment.Installments, checkout.Payments) {
		return fmt.Errorf("tiny: parcelas divergentes após gravação; confirmação pendente")
	}
	return nil
}

func (t *Tiny) GetOrderCommercialDiscount(ctx context.Context, orderID string) (int64, error) {
	order, err := t.readCheckoutOrder(ctx, orderID)
	if err != nil {
		return 0, err
	}
	return int64(math.Round(order.Discount * 100)), nil
}

func digitsOnlyTiny(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, value)
}

func (t *Tiny) OrderInstallmentsMatch(ctx context.Context, orderID string, desired []providers.ERPInstallment) (bool, error) {
	current, err := t.readCheckoutOrder(ctx, orderID)
	if err != nil {
		return false, err
	}
	// A concurrently issued invoice also makes this reflection read-only.
	return current.InvoiceID != 0 || tinyInstallmentsMatch(current.Payment.Installments, desired), nil
}

func tinyInstallmentsMatch(current []tinyCheckoutInstallment, desired []providers.ERPInstallment) bool {
	if len(current) != len(desired) {
		return false
	}
	used := make([]bool, len(current))
	for _, wanted := range desired {
		found := false
		for i, existing := range current {
			// The due date for an unpaid balance is generated relative to now.
			// Preserve the ERP's existing due date rather than moving it daily.
			dateMatches := strings.HasPrefix(wanted.Note, "A PAGAR -") ||
				strings.HasPrefix(existing.Date, wanted.DueDate.Format("2006-01-02"))
			if !used[i] && int64(math.Round(existing.Value*100)) == wanted.AmountCents &&
				existing.Note == wanted.Note && dateMatches {
				used[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
