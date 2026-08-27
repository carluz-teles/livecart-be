// Package erp owns the ERP-integration domain (Tiny order-as-reservation, stock
// launch, finalisation state machine). Bloco B2 strangles this logic out of
// internal/integration slice by slice; B2a lays the foundation (package + ports
// + pure domain types) additively — no behaviour is moved yet.
//
// Import rule (sacred): erp MUST NOT import internal/integration (the package),
// otherwise integration→erp becomes a cycle. erp may import integration/providers
// (pure types). integration imports erp and adapts its concrete types into the
// neutral shapes declared here — the same "wired at boot to break the import
// cycle" approach already used by ERPOrderMirror.
package erp

import (
	"context"
	"errors"
	"time"
)

// ErrCartNotConverted signals to the caller (paid-webhook / finalisation) that
// the cart has no ERP order-as-reservation — the flow must fall back to the
// legacy finalisation path. Canonical home is this package; internal/integration
// keeps a backward-compatible alias while its logic is strangled out (Bloco B2).
var ErrCartNotConverted = errors.New("cart não convertido em pedido ERP")

// ErrPedidoFaturado é a recusa em somar item a um pedido que já virou nota.
//
// Tipada porque tem consequência de negócio a montante: quem quiser juntar
// carrinhos precisa saber que ESTE não junta mais, e abrir um pedido novo em vez
// de perder a venda.
var ErrPedidoFaturado = errors.New("pedido já faturado: não recebe mais item")

// ErrCartBusy diz que há uma escrita em voo naquele pedido e a operação pedida
// precisa esperar. É transitório por natureza — a mutação devolve o carrinho em
// menos de um segundo —, e existe para o chamador saber que RETENTAR resolve, em
// vez de tratar como falha e desistir.
var ErrCartBusy = errors.New("cart com operação ERP em voo")

// ERP order state machine (carts.erp_order_state column):
//
//	none → converting → open ⇄ mutating → confirmed
//	                      └──────────────→ cancelled
//
// Sacred rule: 'converting' NEVER goes back to 'none' — the in-flight call may
// have succeeded server-side, so resetting would reopen the legacy path and
// double the stock write. Canonical home is this package; integration aliases
// these to its existing unexported names to avoid churning ~30 call sites.
const (
	OrderStateNone       = "none"
	OrderStateConverting = "converting"
	OrderStateOpen       = "open"
	OrderStateMutating   = "mutating"
	OrderStateConfirmed  = "confirmed"
	OrderStateCancelled  = "cancelled"
)

// ERPOrderMirror projects ERP state changes into the Order aggregate.
// Implemented by order/listeners.Listener; wired at boot to break the import cycle.
type ERPOrderMirror interface {
	MirrorCartERPToOrder(ctx context.Context, cartID string)
}

// StockReservationRow mirrors a stock_reservations row the ERP stock flow reads
// and writes. Canonical home is this package (Bloco B2b); internal/integration
// aliases it so the Repository (which owns the SQL) keeps compiling unchanged.
type StockReservationRow struct {
	ID                string
	EventID           string
	CartID            string
	ProductID         string
	ExternalProductID string
	Quantity          int
	ERPMovementID     string
	Status            string
	CreatedAt         time.Time
}

// UpsertReservationParams soma unidades à reserva ativa do (carrinho, produto,
// evento), criando a linha quando ela ainda não existe.
type UpsertReservationParams struct {
	EventID           string
	CartID            string
	ProductID         string
	ExternalProductID string
	IncQty            int
	ERPMovementID     string
}

// ReservationDecrement é o que o banco decidiu ao baixar unidades de uma
// reserva. O chamador só age depois de ler isto — nunca a partir de uma leitura
// própria anterior, que envelhece durante a chamada ao ERP.
type ReservationDecrement struct {
	// Applied é falso quando a reserva tinha menos unidades do que se pediu para
	// baixar: leitura obsoleta, ou outra requisição do mesmo comprador chegou
	// primeiro. Nesse caso NADA foi alterado.
	Applied bool
	// Remaining é o que continua reservado depois da baixa. Zero significa que a
	// reserva foi consumida por inteiro e já saiu de 'active' — com a quantidade
	// intacta, porque o CHECK (quantity > 0) proíbe zerar em vigor.
	Remaining int
	// ReservationIDs são as linhas efetivamente mexidas, para a compensação
	// acertar exatamente elas se o ERP recusar.
	ReservationIDs []string
}

// CreateStockReservationParams holds params for creating a stock reservation.
type CreateStockReservationParams struct {
	EventID           string
	CartID            string
	ProductID         string
	ExternalProductID string
	Quantity          int
	ERPMovementID     string
}

// CartERPOrderState is the order-as-reservation lifecycle snapshot (design C).
type CartERPOrderState struct {
	State           string
	StockLaunched   bool
	ExternalOrderID string
	// Situação do pedido no ERP (slug do `codigoSituacao`), vazia enquanto
	// nenhum webhook chegou. Vazio significa "ainda não sei", e quem decide se
	// o pedido recebe item trata isso como "não faturado" — ver pedidoJaFaturado.
	OrderStatus string
	// Quanto o carrinho já recebeu, somando TODOS os pagamentos. É o número que
	// vai para a parcela "PAGO" no ERP. Vem do carrinho e não do retrato do
	// gateway porque o retrato conhece um pagamento só, e um carrinho que juntou
	// compras pode ter dois.
	PaidAmountCents int64
	// Quando o último pagamento entrou — a data que a parcela "PAGO" carrega.
	PaidAt time.Time
}

// NonWaitlistedCartItem is a non-waitlisted cart item carrying its ERP external
// id — the grid input for order mutation (design C) and the base of the payment
// total. Canonical home is this package (Bloco B2c); internal/integration aliases
// it so the Repository (which owns the SQL) keeps compiling unchanged.
type NonWaitlistedCartItem struct {
	ID                string
	CartID            string
	ProductID         string
	Quantity          int
	UnitPrice         int64
	ProductName       string
	ProductExternalID string
	ProductKeyword    string
}

// CartFinalisationStatus is the slim view of the Order payment row's ERP
// finalisation lifecycle (status pending|done|failed) plus the cart's reserve
// external_order_id, joined in for the resume-vs-legacy decision. Canonical home
// is this package (Bloco B2c-2, when the legacy finalisation moved in);
// internal/integration aliases it so the Repository (which owns the SQL) keeps
// compiling unchanged.
type CartFinalisationStatus struct {
	CartID          string
	Status          string
	LastError       string
	LastAttemptAt   *time.Time
	AttemptsCount   int
	ExternalOrderID string
	PaymentSnapshot []byte
}

// StuckERPOrderOp is a conversion/mutation stuck in flight (the process died
// mid-op) — the input to the reconciliation sweep. Canonical home is this
// package; internal/integration aliases it.
type StuckERPOrderOp struct {
	CartID          string
	State           string
	ExternalOrderID string
	StoreID         string
}

// StaleStockWebhookIntegration is an active Tiny integration that stopped
// receiving stock webhooks — the Tiny side silently removes the URL after
// consecutive delivery failures (field lesson, 11/07/2026). Canonical home is
// this package; internal/integration aliases it.
type StaleStockWebhookIntegration struct {
	IntegrationID    string
	StoreID          string
	LastStockEventAt *time.Time
}

// Integration is the ERP-domain view of an active integration row. It is the
// return type of ERPRepository.GetActiveByProvider so this package never has to
// import internal/integration (which owns the concrete IntegrationRow). The
// integration package supplies a thin adapter (Bloco B2b) that copies the row
// into this neutral shape. Mirror of integration.IntegrationRow — deliberate
// port DTO, not duplicated business logic.
type Integration struct {
	ID             string
	StoreID        string
	Type           string
	Provider       string
	Status         string
	Credentials    []byte // encrypted
	TokenExpiresAt *time.Time
	Metadata       map[string]any
	LastSyncedAt   *time.Time
	CreatedAt      time.Time
	Priority       int
}
