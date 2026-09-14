package erp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"livecart/apps/api/internal/integration/providers"
)

type tinyCreateRejected struct {
	status int
	detail string
	cause  error
}

func (e *tinyCreateRejected) Unwrap() error { return e.cause }

func (e *tinyCreateRejected) Error() string {
	return fmt.Sprintf("tiny rejected checkout creation: status %d: %s", e.status, e.detail)
}

// TinyPaidInstallments reuses the gateway schedule used by Tiny order creation.
// The ledger's payment ID is retained in every installment, including rounding.
func TinyPaidInstallments(payment *providers.ERPOrderPayment) ([]providers.ERPInstallment, error) {
	if payment == nil || payment.Amount <= 0 || payment.PaidAt.IsZero() || payment.Installments > 100 {
		return nil, fmt.Errorf("invalid Tiny payment schedule")
	}
	var out []providers.ERPInstallment
	for _, p := range buildTinyParcelas(payment, nil, nil) {
		date, err := time.ParseInLocation("2006-01-02", p["data"].(string), tinyLocation)
		if err != nil {
			return nil, err
		}
		out = append(out, providers.ERPInstallment{AmountCents: int64(math.Round(p["valor"].(float64) * 100)), DueDate: date, Method: payment.Method, Note: p["observacoes"].(string)})
	}
	return out, nil
}

func (t *Tiny) checkoutPaymentPayload(ctx context.Context, payments []providers.ERPInstallment) (map[string]any, error) {
	if len(payments) == 0 {
		return nil, fmt.Errorf("tiny: checkout sem pagamentos")
	}
	refs := map[string]int64{}
	var parent int64
	mixed := false
	parcels := make([]map[string]any, 0, len(payments))
	for _, p := range payments {
		if p.AmountCents <= 0 || p.DueDate.IsZero() || p.Method == "" {
			return nil, fmt.Errorf("tiny: parcela sem valor, data ou forma de pagamento")
		}
		id, ok := refs[p.Method]
		if !ok {
			var err error
			id, err = t.lookupFormaRecebimentoID(ctx, p.Method)
			if err != nil {
				return nil, err
			}
			if id == 0 {
				return nil, fmt.Errorf("tiny: forma de recebimento %s não configurada", p.Method)
			}
			refs[p.Method] = id
		}
		if parent == 0 {
			parent = id
		} else if parent != id {
			mixed = true
		}
		parcels = append(parcels, map[string]any{"data": p.DueDate.Format("2006-01-02"), "valor": float64(p.AmountCents) / 100, "observacoes": p.Note, "formaRecebimento": map[string]any{"id": id}})
	}
	out := map[string]any{"parcelas": parcels}
	if !mixed {
		out["formaRecebimento"] = map[string]any{"id": parent}
	}
	return out, nil
}

// resolveShippingForm shares the carrier/aggregator mapping used at creation.
func (t *Tiny) resolveShippingForm(ctx context.Context, carrier string) (int64, string, error) {
	if isStorePickup(carrier) {
		return 0, carrier, nil
	}
	id, err := t.lookupFormaEnvioID(ctx, carrier)
	if err != nil || id > 0 || carrier == "SmartEnvios" {
		return id, carrier, err
	}
	id, err = t.lookupFormaEnvioID(ctx, "SmartEnvios")
	return id, "SmartEnvios", err
}

func tinyCheckoutShippingMatches(order *tinyCheckoutOrder, expected int64) bool {
	if order.Shipping.Form == nil {
		return expected == 0
	}
	return order.Shipping.Form.ID == expected
}

func tinyCheckoutMethodsMatch(order *tinyCheckoutOrder, desired []providers.ERPInstallment) bool {
	if len(order.Payment.Installments) != len(desired) {
		return false
	}
	used := make([]bool, len(order.Payment.Installments))
	for _, p := range desired {
		found := false
		for i, actual := range order.Payment.Installments {
			if used[i] || int64(math.Round(actual.Value*100)) != p.AmountCents {
				continue
			}
			if matchesFormaRecebimento(p.Method, actual.Method.Name) {
				used[i], found = true, true
			}
			if found {
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func tinyCheckoutGrid(order *tinyCheckoutOrder) []providers.ERPOrderItem {
	items := make([]providers.ERPOrderItem, 0, len(order.Items))
	for _, i := range order.Items {
		items = append(items, providers.ERPOrderItem{ProductID: strconv.FormatInt(i.Product.ID, 10), Quantity: i.Quantity, UnitPrice: int64(math.Round(i.UnitPrice * 100)), Note: i.Note})
	}
	return items
}

func tinyCheckoutGridMatches(order *tinyCheckoutOrder, desired []providers.ERPOrderItem) bool {
	key := func(id string, cents int64) string { return fmt.Sprintf("%s:%d", id, cents) }
	quantities := map[string]int{}
	for _, i := range tinyCheckoutGrid(order) {
		quantities[key(i.ProductID, i.UnitPrice)] += i.Quantity
	}
	for _, i := range desired {
		quantities[key(i.ProductID, i.UnitPrice)] -= i.Quantity
	}
	for _, quantity := range quantities {
		if quantity != 0 {
			return false
		}
	}
	return true
}

func (t *Tiny) sourceForCheckout(ctx context.Context, op *providers.TinyCheckoutOperation) (*tinyCheckoutOrder, error) {
	source, err := t.readCheckoutOrder(ctx, op.SourceID)
	if err != nil {
		return nil, err
	}
	anchor := tinyCartMarker(op.CartID)
	if op.SourceAnchor != "" {
		anchor = op.SourceAnchor
	}
	if source.Anchor != anchor {
		return nil, fmt.Errorf("tiny: pedido de origem sem vínculo verificável com o carrinho")
	}
	if source.InvoiceID != 0 || (source.Status != 0 && source.Status != 3 && !(op.CreateStarted && source.Status == 2)) {
		return nil, fmt.Errorf("tiny: pedido de origem faturado ou encerrado; conciliação manual necessária")
	}
	return source, nil
}

// FinalizePaidCheckout holds the old reservation until a verified replacement
// exists. The caller owns the cart's distributed finalisation lock and CAS.
// Invoiced/received orders are never cancelled or financially reversed.
func (t *Tiny) FinalizePaidCheckout(ctx context.Context, op *providers.TinyCheckoutOperation, journal providers.TinyCheckoutJournal) (*OrderResult, error) {
	if op.Order.Checkout == nil || op.CartID == "" || op.ID == "" {
		return nil, fmt.Errorf("tiny: missing checkout operation")
	}
	save := func() error { return journal.Save(ctx, op) }
	checkout := *op.Order.Checkout
	if op.Completed {
		if op.TargetID == "" {
			return nil, fmt.Errorf("tiny: finalização concluída sem pedido")
		}
		return &OrderResult{OrderID: op.TargetID, OrderNumber: op.TargetNumber}, nil
	}
	t.Logger.Info("tiny paid checkout reconciliation started", zap.String("cart_id", op.CartID),
		zap.String("operation_id", op.ID), zap.String("source_order_id", op.SourceID),
		zap.String("target_order_id", op.TargetID), zap.Bool("resuming", op.Prepared))
	if !op.Prepared {
		source, err := t.sourceForCheckout(ctx, op)
		if err != nil {
			return nil, err
		}
		accounts, err := t.checkoutReceivables(ctx, op.SourceID)
		if err != nil {
			return nil, err
		}
		if op.Order.ContactID == "" {
			if source.Customer.ID <= 0 {
				return nil, fmt.Errorf("tiny: pedido sem contato verificável")
			}
			op.Order.ContactID = strconv.FormatInt(source.Customer.ID, 10)
		}
		if ship := checkout.Shipping; ship != nil {
			id, _, err := t.resolveShippingForm(ctx, ship.Carrier)
			if err != nil {
				return nil, err
			}
			if id == 0 && !isStorePickup(ship.Carrier) {
				return nil, fmt.Errorf("tiny: forma de envio não configurada para o checkout")
			}
			op.ExpectedShippingID = id
		}
		op.Replace = len(tinyCheckoutDifferences(source, checkout)) > 0 || !tinyCheckoutMethodsMatch(source, checkout.Payments) || !tinyCheckoutGridMatches(source, op.Order.Items) || (checkout.Shipping != nil && !tinyCheckoutShippingMatches(source, op.ExpectedShippingID))
		if op.Replace {
			if source.OtherExpenses != 0 || !tinyCheckoutPreservesMerchantItems(source, op.Order.Items) {
				return nil, fmt.Errorf("tiny: ajustes do lojista fora do checkout; conciliação manual necessária")
			}
			headers := map[string]any{}
			for key, ref := range map[string]*tinyCheckoutReference{"deposito": source.Deposit, "naturezaOperacao": source.Nature, "listaPreco": source.PriceList, "vendedor": source.Seller} {
				if ref != nil && ref.ID > 0 {
					headers[key] = map[string]any{"id": ref.ID}
				}
			}
			if op.Order.Metadata == nil {
				op.Order.Metadata = map[string]any{}
			}
			op.Order.Metadata["tiny_order_headers"] = headers
		}
		op.AccountsRequired = len(accounts) > 0 && (op.Replace || !tinyReceivablesMatch(accounts, checkout.Payments))
		if op.AccountsRequired {
			if err := t.requireUnreceivedAccounts(ctx, accounts); err != nil {
				return nil, err
			}
		}
		if op.Replace {
			if err := t.UpdateOrderItems(ctx, op.SourceID, tinyCheckoutGrid(source)); err != nil && !(op.AccountsRequired && errors.Is(err, providers.ErrOrderAccountsLaunched)) {
				return nil, err
			}
		}
		if !op.Replace {
			op.TargetID, op.TargetNumber = op.SourceID, source.Number.String()
		}
		op.Prepared = true
		if err := save(); err != nil {
			return nil, err
		}
	}
	if op.AccountsRequired && !op.AccountsCleared {
		source, err := t.sourceForCheckout(ctx, op)
		if err != nil {
			return nil, err
		}
		accounts, err := t.checkoutReceivables(ctx, op.SourceID)
		if err != nil {
			return nil, err
		}
		if len(accounts) > 0 {
			if err := t.requireUnreceivedAccounts(ctx, accounts); err != nil {
				return nil, err
			}
			if err := t.UpdateOrderItems(ctx, op.SourceID, tinyCheckoutGrid(source)); err != nil && !errors.Is(err, providers.ErrOrderAccountsLaunched) {
				return nil, err
			}
			if err := t.checkoutRequest(ctx, http.MethodPost, "/pedidos/"+op.SourceID+"/estornar-contas", nil, nil); err != nil {
				return nil, err
			}
			accounts, err = t.checkoutReceivables(ctx, op.SourceID)
			if err != nil {
				return nil, err
			}
			if len(accounts) != 0 {
				return nil, fmt.Errorf("tiny: estorno das contas não confirmado")
			}
		}
		op.AccountsCleared = true
		if err := save(); err != nil {
			return nil, err
		}
	}
	if op.Replace && op.TargetID == "" {
		if op.CreateStarted {
			// A lost response is ambiguous. Never repeat a POST merely because a
			// short recent-orders search returned empty.
			id, err := t.findCheckoutReplacement(ctx, tinyCartMarker(op.Order.ExternalID), op.StartedAt)
			if err != nil {
				return nil, err
			}
			if id == "" {
				return nil, fmt.Errorf("tiny: criação com resultado incerto; aguardando localizar o pedido %s, sem repetir POST", op.ID)
			}
			op.TargetID = id
		} else {
			source, err := t.sourceForCheckout(ctx, op)
			if err != nil {
				return nil, err
			}
			// Same-grid PUT is the supported lock check. Unlike the legacy grid
			// flow, this path never reverses stock to bypass a merchant launch.
			if err := t.UpdateOrderItems(ctx, op.SourceID, tinyCheckoutGrid(source)); err != nil {
				return nil, err
			}
			if err := t.UpdateContact(ctx, op.Order.ContactID, checkout.Customer); err != nil {
				return nil, fmt.Errorf("updating Tiny checkout customer: %w", err)
			}
			op.CreateStarted = true
			if err := save(); err != nil {
				return nil, err
			}
			created, err := t.CreateOrder(ctx, op.Order)
			if err != nil {
				var rejection *tinyCreateRejected
				if errors.As(err, &rejection) {
					op.CreateStarted = false
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer cancel()
					return nil, errors.Join(err, journal.Save(cleanup, op))
				}
				return nil, err
			}
			op.TargetID, op.TargetNumber = created.OrderID, created.OrderNumber
		}
		if err := save(); err != nil {
			return nil, err
		}
	}
	if op.Replace && !op.SourceCancelled {
		target, err := t.readCheckoutOrder(ctx, op.TargetID)
		if err != nil {
			return nil, err
		}
		if checkout.Shipping != nil && !tinyCheckoutShippingMatches(target, op.ExpectedShippingID) {
			return nil, fmt.Errorf("tiny: forma de envio divergente no pedido substituto; reserva original preservada")
		}
		if target.InvoiceID != 0 || target.Status != 0 || target.Anchor != tinyCartMarker(op.Order.ExternalID) || len(tinyCheckoutDifferences(target, checkout)) != 0 || !tinyCheckoutMethodsMatch(target, checkout.Payments) || !tinyCheckoutGridMatches(target, op.Order.Items) {
			return nil, fmt.Errorf("tiny: pedido substituto divergente; campos=%v status=%d nota=%t vínculo=%t formas=%t itens=%t; reserva original preservada", tinyCheckoutDifferences(target, checkout), target.Status, target.InvoiceID != 0, target.Anchor == tinyCartMarker(op.Order.ExternalID), tinyCheckoutMethodsMatch(target, checkout.Payments), tinyCheckoutGridMatches(target, op.Order.Items))
		}
		op.TargetNumber = target.Number.String()
		source, err := t.sourceForCheckout(ctx, op)
		if err != nil {
			return nil, err
		}
		if source.Status != 2 {
			accounts, err := t.checkoutReceivables(ctx, op.SourceID)
			if err != nil {
				return nil, err
			}
			if len(accounts) != 0 {
				return nil, fmt.Errorf("tiny: contas lançadas durante a finalização; reserva original preservada")
			}
			if err := t.UpdateOrderItems(ctx, op.SourceID, tinyCheckoutGrid(source)); err != nil {
				return nil, err
			}
			if err := t.SetOrderSituacao(ctx, op.SourceID, providers.SituacaoCancelada); err != nil {
				return nil, err
			}
			source, err = t.readCheckoutOrder(ctx, op.SourceID)
			if err != nil {
				return nil, err
			}
			if source.Status != 2 {
				return nil, fmt.Errorf("tiny: cancelamento da reserva não confirmado")
			}
		}
		op.SourceCancelled = true
		if err := save(); err != nil {
			return nil, err
		}
	}
	if err := t.SyncOrderCheckout(ctx, op.TargetID, checkout); err != nil {
		return nil, err
	}
	if op.AccountsRequired {
		accounts, err := t.checkoutReceivables(ctx, op.TargetID)
		if err != nil {
			return nil, err
		}
		if len(accounts) == 0 {
			if err := t.checkoutRequest(ctx, http.MethodPost, "/pedidos/"+op.TargetID+"/lancar-contas", nil, nil); err != nil {
				return nil, err
			}
			accounts, err = t.checkoutReceivables(ctx, op.TargetID)
			if err != nil {
				return nil, err
			}
		}
		if !tinyReceivablesMatch(accounts, checkout.Payments) {
			return nil, fmt.Errorf("tiny: contas a receber divergentes após finalização")
		}
	}
	status, err := t.GetOrderSituacao(ctx, op.TargetID)
	if err != nil {
		return nil, err
	}
	if status == 0 {
		if err := t.SetOrderSituacao(ctx, op.TargetID, providers.SituacaoAprovada); err != nil {
			return nil, err
		}
		status, err = t.GetOrderSituacao(ctx, op.TargetID)
		if err != nil {
			return nil, err
		}
	}
	if status != providers.SituacaoAprovada {
		return nil, fmt.Errorf("tiny: aprovação do pedido final não confirmada")
	}
	if err := journal.Bind(ctx, op); err != nil {
		return nil, err
	}
	t.Logger.Info("tiny paid checkout reconciled", zap.String("cart_id", op.CartID), zap.String("operation_id", op.ID), zap.String("source_order_id", op.SourceID), zap.String("target_order_id", op.TargetID), zap.Bool("replacement", op.Replace), zap.Bool("receivables_rebuilt", op.AccountsRequired))
	return &OrderResult{OrderID: op.TargetID, OrderNumber: op.TargetNumber}, nil
}

func tinyCheckoutPreservesMerchantItems(source *tinyCheckoutOrder, desired []providers.ERPOrderItem) bool {
	remaining := map[string]int{}
	key := func(id string, price int64) string { return fmt.Sprintf("%s:%d", id, price) }
	for _, i := range desired {
		remaining[key(i.ProductID, i.UnitPrice)] += i.Quantity
	}
	for _, i := range tinyCheckoutGrid(source) {
		if strings.HasPrefix(strings.TrimSpace(i.Note), providers.LiveCartItemMarker) {
			continue
		}
		k := key(i.ProductID, i.UnitPrice)
		remaining[k] -= i.Quantity
		if remaining[k] < 0 {
			return false
		}
	}
	return true
}

// Full, bounded-by-creation-date lookup. A read failure is not absence.
func (t *Tiny) findCheckoutReplacement(ctx context.Context, marker string, since time.Time) (string, error) {
	for offset := 0; ; offset += 100 {
		q := url.Values{"dataInicial": {since.In(tinyLocation).Format("2006-01-02")}, "limit": {"100"}, "offset": {strconv.Itoa(offset)}, "orderBy": {"desc"}}
		var page struct {
			Items []struct {
				ID     int64  `json:"id"`
				Anchor string `json:"numeroOrdemCompra"`
			} `json:"itens"`
		}
		if err := t.checkoutRequest(ctx, http.MethodGet, "/pedidos?"+q.Encode(), nil, &page); err != nil {
			return "", err
		}
		for _, item := range page.Items {
			id := strconv.FormatInt(item.ID, 10)
			anchor := item.Anchor
			if anchor == "" {
				var err error
				anchor, err = t.orderAnchor(ctx, id)
				if err != nil {
					return "", err
				}
			}
			if anchor == marker {
				return id, nil
			}
		}
		if len(page.Items) < 100 {
			return "", nil
		}
	}
}
