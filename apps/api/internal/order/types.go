package order

import (
	"time"

	validation "github.com/go-ozzo/ozzo-validation/v4"

	"livecart/apps/api/lib/query"
)

// Handler layer - Filters
type OrderFilters struct {
	Status        []string `query:"status"`        // active, checkout, completed, expired
	PaymentStatus []string `query:"paymentStatus"` // pending, paid, failed, refunded
	// EventID filtra por CAMPANHA. O campo se chamava LiveSessionID e o filtro
	// SQL sempre foi `c.event_id = ?` — nome que ativamente engana quem for
	// implementar: quem lesse "session" passaria um id de live_sessions e
	// receberia zero linhas, sem erro. RN-19.
	EventID  *string `query:"eventId"`
	DateFrom *string `query:"dateFrom"`
	DateTo   *string `query:"dateTo"`

	// Tri-state filter on whether the order has any shipment row.
	// nil = ignore; *true = only orders with at least one shipment;
	// *false = only orders without any shipment. Combined with ShipmentStatus
	// the latter wins (ShipmentStatus implies HasShipment=true).
	HasShipment *bool `query:"hasShipment"`

	// Filter orders whose latest shipment status is in this set. Empty/nil = ignore.
	// Values follow the normalized ShipmentStatus enum (in_transit, delivered, ...).
	ShipmentStatus []string `query:"shipmentStatus"`

	// Filter by the cart's ERP finalisation lifecycle. Direct-query escape
	// hatch — most callers should use NeedsAttention which folds this into
	// the unified "Precisam atenção" bucket.
	ERPFinalisation []string `query:"erpFinalisation"` // pending | done | failed

	// ProductID filtra pedidos que CONTÊM o produto (existe cart_item dele no
	// carrinho). Pedido do cliente 20/08/2026: "a partir de um produto, quais
	// pedidos estão com aquele produto" — a tela de produtos deep-linka para
	// /orders?product=<id>. Vale para lista E contadores de aba (mesmo builder).
	ProductID *string `query:"productId"`

	// Single triage flag that ORs every kind of "merchant has to fix this"
	// state into one filter — payment failed/refunded, shipment in error,
	// or ERP finalisation failed. Drives the unified "Precisam atenção"
	// tab so the merchant sees one bucket regardless of which subsystem
	// blew up. ERP-agnostic by design (works for Tiny, Bling, …).
	NeedsAttention *bool `query:"needsAttention"`
}

// Handler layer - Request/Response types
type UpdateOrderRequest struct {
	Status        *string `json:"status"`
	PaymentStatus *string `json:"paymentStatus"`
}

// Validate is the syntactic gate (ozzo). Both fields are optional; when present
// they must be one of the allowed enum values.
func (r UpdateOrderRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Status, validation.In("active", "checkout", "completed", "expired")),
		validation.Field(&r.PaymentStatus, validation.In("pending", "paid", "failed", "refunded")),
	)
}

// ToInput builds the usecase input from the validated request and the path/context
// arguments the handler supplies.
func (r UpdateOrderRequest) ToInput(id, storeID string) (UpdateOrderInput, error) {
	return UpdateOrderInput{
		ID:            id,
		StoreID:       storeID,
		Status:        r.Status,
		PaymentStatus: r.PaymentStatus,
	}, nil
}

// UpdateShippingAddressRequest is the admin's "edit address" payload. State
// is required; the 2-letter UF guard mirrors the public checkout flow.
type UpdateShippingAddressRequest struct {
	ZipCode      string `json:"zipCode"`
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement,omitempty"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
}

// Validate is the syntactic gate (ozzo). State is the 2-letter UF.
func (r UpdateShippingAddressRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.ZipCode, validation.Required),
		validation.Field(&r.Street, validation.Required),
		validation.Field(&r.Number, validation.Required),
		validation.Field(&r.Neighborhood, validation.Required),
		validation.Field(&r.City, validation.Required),
		validation.Field(&r.State, validation.Required, validation.Length(2, 2)),
	)
}

// ToInput builds the address map the usecase expects, carrying the path/context
// scoping args (order id + store id) alongside the address fields.
func (r UpdateShippingAddressRequest) ToInput(id, storeID string) (UpdateShippingAddressInput, error) {
	return UpdateShippingAddressInput{
		ID:      id,
		StoreID: storeID,
		Address: map[string]string{
			"zipCode":      r.ZipCode,
			"street":       r.Street,
			"number":       r.Number,
			"complement":   r.Complement,
			"neighborhood": r.Neighborhood,
			"city":         r.City,
			"state":        r.State,
		},
	}, nil
}

// RegenerateCheckoutResponse returns the data the admin needs to share with
// the buyer (the cart token + the new expiration). The frontend builds the
// public URL from the token because the base URL lives in the FE config.
type RegenerateCheckoutResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type OrderItemResponse struct {
	ID           string  `json:"id"`
	ProductID    string  `json:"productId"`
	ProductName  string  `json:"productName"`
	ProductImage *string `json:"productImage"`
	Keyword      string  `json:"keyword"`
	Size         *string `json:"size"`
	Quantity     int     `json:"quantity"`
	UnitPrice    int64   `json:"unitPrice"`
	TotalPrice   int64   `json:"totalPrice"`

	// WaitlistedQuantity é a parcela de quantity SEM estoque. `quantity` é o
	// total pedido, então o que a cliente pode pagar agora é
	// quantity - waitlistedQuantity — a mesma conta do checkout público. Sempre
	// 0 em pedido já pago (o snapshot registra o que foi vendido).
	WaitlistedQuantity int `json:"waitlistedQuantity"`

	// Shipping dimensions (joined from products). Zero when the product has no
	// dimensions filled in — admin UIs should treat them as "missing" not "0"
	// and block create-shipment until the merchant fills them in.
	WeightGrams   int    `json:"weightGrams"`
	HeightCm      int    `json:"heightCm"`
	WidthCm       int    `json:"widthCm"`
	LengthCm      int    `json:"lengthCm"`
	PackageFormat string `json:"packageFormat"`
}

type OrderResponse struct {
	ID string `json:"id"`
	// Per-store sequential order number, starts at 1000 in each store. UI shows
	// "#{shortId}" to merchants and customers — the UUID stays as the URL key.
	ShortID int `json:"shortId"`
	// EventID é o id da CAMPANHA. liveSessionId sai com o MESMO valor por
	// compatibilidade com o frontend atual — ele nunca carregou um id de
	// sessão, sempre foi event_id (RN-19). EventTitle idem: é o título do
	// evento, não da transmissão.
	EventID        string `json:"eventId"`
	EventTitle     string `json:"eventTitle"`
	LiveSessionID  string `json:"liveSessionId"` // Deprecated: use eventId.
	LiveTitle      string `json:"liveTitle"`     // Deprecated: use eventTitle.
	LivePlatform   string `json:"livePlatform"`
	CustomerHandle string `json:"customerHandle"`
	CustomerID     string `json:"customerId"`
	// Customer name/email captured at checkout. Empty until the buyer fills the
	// checkout form.
	CustomerName  string `json:"customerName"`
	CustomerEmail string `json:"customerEmail"`
	// Mirrors the live event's freeShipping flag, used by the list to render
	// a "frete grátis" indicator without loading the full event.
	FreeShipping  bool   `json:"freeShipping"`
	Status        string `json:"status"`
	PaymentStatus string `json:"paymentStatus"`
	// Latest shipment status (normalized enum). Empty string when the order has
	// no shipment yet.
	ShipmentStatus string `json:"shipmentStatus"`
	// True when the buyer picked a shipping service at checkout. Lets the
	// admin list distinguish "buyer never selected anything" from "selected,
	// but no shipment row created yet".
	HasShipping bool                `json:"hasShipping"`
	Items       []OrderItemResponse `json:"items"`
	// Lightweight preview (name/image/qty) so the list can render an avatar
	// stack without the full Items array. Populated only on list endpoints.
	ItemsPreview []OrderItemPreviewResponse `json:"itemsPreview"`
	TotalItems   int                        `json:"totalItems"`
	TotalAmount  int64                      `json:"totalAmount"`
	PaidAt       *time.Time                 `json:"paidAt"`
	CreatedAt    time.Time                  `json:"createdAt"`
	ExpiresAt    *time.Time                 `json:"expiresAt"`
	// True only for the buyer's earliest paid order in this store. Frontend
	// renders a "Primeira venda" badge from this flag.
	IsFirstPurchase bool `json:"isFirstPurchase"`
	// Lifecycle of the post-payment Tiny order creation: pending | done | failed.
	// Surfaced on the list so the admin can spot "Pedido pago, Tiny falhou" rows
	// without opening each one. Values mirror cart.erp_finalisation_status.
	ERPFinalisationStatus string `json:"erpFinalisationStatus"`
}

type OrderItemPreviewResponse struct {
	ProductName  string  `json:"productName"`
	ProductImage *string `json:"productImage"`
	Quantity     int     `json:"quantity"`
}

// OrderDetailResponse includes everything the admin order-detail page needs:
// customer captured at checkout, delivery address, freight selection, the
// shipment created at the carrier (when any) with its tracking timeline, and
// store shipping defaults so the UI can pre-fill the create-shipment form.
// The `OrderResponse` embedding keeps list-page fields identical.
// OrderNotificationOutput é uma DM automática do pedido, direto do
// notification_logs: o QUE foi enviado (texto verbatim), quando, e o desfecho
// (sent/failed/skipped/cooldown + erro). É a metade "o que a cliente recebeu"
// da árvore de histórico — antes o lojista via o comentário e o pedido, mas
// nunca a mensagem que o LiveCart mandou (ou deixou de mandar).
type OrderNotificationOutput struct {
	Type      string     `json:"type"`
	Channel   string     `json:"channel"`
	Status    string     `json:"status"`
	Message   *string    `json:"message,omitempty"`
	Error     *string    `json:"error,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	SentAt    *time.Time `json:"sentAt,omitempty"`
}

// OrderWaitlistJourneyOutput é a jornada COMPLETA de uma entrada na fila —
// inclusive as encerradas (fulfilled/expired/cancelled), que a seção
// "Aguardando estoque" de propósito não mostra. O histórico precisa delas:
// "entrou na fila → liberou → o prazo da liberação venceu" é exatamente o tipo
// de desfecho que hoje some da tela.
type OrderWaitlistJourneyOutput struct {
	ProductName string     `json:"productName"`
	Quantity    int        `json:"quantity"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	NotifiedAt  *time.Time `json:"notifiedAt,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	FulfilledAt *time.Time `json:"fulfilledAt,omitempty"`
	CancelledAt *time.Time `json:"cancelledAt,omitempty"`
}

type OrderDetailResponse struct {
	OrderResponse
	// Cart token; the public buyer link is `${frontend_origin}/cart/${token}`.
	// Detail-only — the list endpoint does not expose this to keep the surface
	// area narrow.
	Token           string                        `json:"token"`
	Comments        []OrderCommentResponse        `json:"comments"`
	Customer        *OrderCustomerResponse        `json:"customer,omitempty"`
	ShippingAddress *OrderShippingAddressResponse `json:"shippingAddress,omitempty"`
	Shipping        *OrderShippingSelectionResp   `json:"shipping,omitempty"`
	Shipment        *OrderShipmentResponse        `json:"shipment,omitempty"`
	Store           *OrderStoreResponse           `json:"store,omitempty"`
	ERPFinalisation *ERPFinalisationResponse      `json:"erpFinalisation,omitempty"`
	ERPInvoice      *ERPInvoiceResponse           `json:"erpInvoice,omitempty"`
	// CustomerBlocked is true when the buyer's handle is currently blocked
	// for this store. The FE uses it to render a "Cliente bloqueado" badge.
	CustomerBlocked bool `json:"customerBlocked"`
	// CancellationRevertedAt: quando presente, este pedido foi cancelado pela
	// loja e mesmo assim foi pago — o cancelamento foi revertido e o pedido
	// seguiu o fluxo normal. O FE mostra isso no histórico do pedido.
	CancellationRevertedAt *time.Time `json:"cancellationRevertedAt,omitempty"`
	// Waitlist são os produtos que a cliente pediu, a loja não tinha e ela
	// entrou na fila. Não somam no total nem vão para a transportadora — são o
	// que o lojista precisa dizer que está esperando reposição.
	Waitlist []OrderWaitlistItemResponse `json:"waitlist"`
	// Árvore de histórico (20/08/2026): DMs enviadas e jornada completa da fila.
	Notifications   []OrderNotificationOutput    `json:"notifications"`
	WaitlistJourney []OrderWaitlistJourneyOutput `json:"waitlistJourney"`
	// PayableAmount: só as unidades COM estoque — é o que a cliente consegue
	// pagar agora e o valor que o orçamento impresso apresenta. Igual a
	// totalAmount quando não há nada em fila.
	PayableAmount int64 `json:"payableAmount"`
	// WaitlistedAmount: valor das unidades em fila, declarado no orçamento como
	// não incluído em vez de simplesmente omitido.
	WaitlistedAmount int64 `json:"waitlistedAmount"`

	// Pagamento: método (pix/credit_card/...), parcelas e os valores REAIS do
	// pedido. PaidTotalCents é EXATAMENTE o que foi cobrado (com desconto PIX);
	// DiscountCents é cupom + desconto PIX. O FE exibe "PIX" / "Cartão · 3x", o
	// desconto e o valor pago sem recalcular.
	PaymentMethod  string `json:"paymentMethod,omitempty"`
	Installments   int    `json:"installments,omitempty"`
	DiscountCents  int64  `json:"discountCents"`
	PaidTotalCents int64  `json:"paidTotalCents"`
}

// OrderWaitlistItemResponse é a projeção de uma entrada de fila de espera.
type OrderWaitlistItemResponse struct {
	ID           string    `json:"id"`
	ProductID    string    `json:"productId"`
	ProductName  string    `json:"productName"`
	ProductImage *string   `json:"productImage"`
	Keyword      string    `json:"keyword"`
	Quantity     int       `json:"quantity"`
	UnitPrice    int64     `json:"unitPrice"`
	Position     int       `json:"position"`
	Status       string    `json:"status"` // waiting | notified
	CreatedAt    time.Time `json:"createdAt"`
}

// ERPFinalisationResponse is the FE-visible projection of the cart's ERP
// finalisation lifecycle. Sent on every paid-cart detail; the FE shows the
// retry banner only when Status == "failed".
type ERPFinalisationResponse struct {
	Status        string     `json:"status"` // pending | done | failed
	LastError     string     `json:"lastError,omitempty"`
	LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty"`
	AttemptsCount int        `json:"attemptsCount"`
	CanRetry      bool       `json:"canRetry"`
}

// ERPInvoiceResponse is the FE-visible projection of carts.erp_invoice_*.
// Absent (nil pointer in the parent) means the merchant hasn't emitted the
// NFe yet — the FE renders "Aguardando NFe na Tiny" with a "Verificar NFe"
// button. Once present, status drives the next-step copy.
type ERPInvoiceResponse struct {
	InvoiceID  string     `json:"invoiceId,omitempty"`
	InvoiceKey string     `json:"invoiceKey,omitempty"`
	Status     string     `json:"status"` // pending | authorized | cancelled | rejected
	EmittedAt  *time.Time `json:"emittedAt,omitempty"`
}

// OrderCustomerResponse mirrors the customer_* columns on carts captured at checkout.
type OrderCustomerResponse struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Document string `json:"document"`
	Phone    string `json:"phone"`
}

// OrderShippingAddressResponse is the destination address captured at checkout.
// All fields are strings so the admin UI never has to coerce nulls.
type OrderShippingAddressResponse struct {
	ZipCode      string `json:"zipCode"`
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
}

// OrderShippingSelectionResp mirrors CartShippingSelection from the checkout
// DTOs, kept provider-agnostic (serviceId is opaque string + provider name).
type OrderShippingSelectionResp struct {
	Provider      string `json:"provider"`
	ServiceID     string `json:"serviceId"`
	ServiceName   string `json:"serviceName"`
	Carrier       string `json:"carrier"`
	CostCents     int64  `json:"costCents"`
	RealCostCents int64  `json:"realCostCents"`
	DeadlineDays  int    `json:"deadlineDays"`
	FreeShipping  bool   `json:"freeShipping"`
}

// OrderShipmentResponse is the freight order created at the carrier + its
// timeline of tracking events. Absent when no shipment has been created yet.
type OrderShipmentResponse struct {
	ID                  string                   `json:"id"`
	Provider            string                   `json:"provider"`
	ProviderOrderID     string                   `json:"providerOrderId"`
	ProviderOrderNumber string                   `json:"providerOrderNumber"`
	TrackingCode        string                   `json:"trackingCode"`
	PublicTrackingURL   string                   `json:"publicTrackingUrl"`
	InvoiceKey          string                   `json:"invoiceKey"`
	InvoiceKind         string                   `json:"invoiceKind"`
	LabelURL            string                   `json:"labelUrl"`
	Status              string                   `json:"status"`
	StatusRawCode       int                      `json:"statusRawCode"`
	StatusRawName       string                   `json:"statusRawName"`
	CreatedAt           time.Time                `json:"createdAt"`
	UpdatedAt           time.Time                `json:"updatedAt"`
	Events              []OrderShipmentEventResp `json:"events"`
}

// OrderShipmentEventResp is a single row from shipment_tracking_events.
type OrderShipmentEventResp struct {
	Status      string    `json:"status"`
	RawCode     int       `json:"rawCode"`
	RawName     string    `json:"rawName"`
	Observation string    `json:"observation"`
	EventAt     time.Time `json:"eventAt"`
	Source      string    `json:"source"`
}

// OrderStoreResponse exposes the origin data needed to pre-fill create-shipment.
type OrderStoreResponse struct {
	ID               string                     `json:"id"`
	Name             string                     `json:"name"`
	LogoURL          *string                    `json:"logoUrl"`
	Document         string                     `json:"document"` // CNPJ
	Email            string                     `json:"email"`
	Phone            string                     `json:"phone"`
	Address          OrderStoreAddressResponse  `json:"address"`
	ShippingDefaults OrderStoreShippingDefaults `json:"shippingDefaults"`
}

type OrderStoreAddressResponse struct {
	ZipCode      string `json:"zipCode"`
	Street       string `json:"street"`
	Number       string `json:"number"`
	Complement   string `json:"complement"`
	Neighborhood string `json:"neighborhood"`
	City         string `json:"city"`
	State        string `json:"state"`
}

type OrderStoreShippingDefaults struct {
	PackageWeightGrams int    `json:"packageWeightGrams"`
	PackageFormat      string `json:"packageFormat"`
}

type OrderCommentResponse struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

type ListOrdersResponse struct {
	Data       []OrderResponse          `json:"data"`
	Pagination query.PaginationResponse `json:"pagination"`
}

type OrderStatsResponse struct {
	TotalOrders   int   `json:"totalOrders"`
	PendingOrders int   `json:"pendingOrders"`
	TotalRevenue  int64 `json:"totalRevenue"`
	AvgTicket     int64 `json:"avgTicket"`
}

// ============================================
// Outbound mappers (Output -> Response). These are the controller's outbound
// mappers, co-located with the Response structs: presentation knows the service
// output; the service never knows the Response.
// ============================================

// NewOrderResponse maps a single order output to its API response.
func NewOrderResponse(o OrderOutput) OrderResponse {
	items := make([]OrderItemResponse, len(o.Items))
	for i, item := range o.Items {
		items[i] = OrderItemResponse{
			ID:                 item.ID,
			ProductID:          item.ProductID,
			ProductName:        item.ProductName,
			ProductImage:       item.ProductImage,
			Keyword:            item.Keyword,
			Size:               item.Size,
			Quantity:           item.Quantity,
			UnitPrice:          item.UnitPrice,
			TotalPrice:         item.TotalPrice,
			WaitlistedQuantity: item.WaitlistedQuantity,
			WeightGrams:        item.WeightGrams,
			HeightCm:           item.HeightCm,
			WidthCm:            item.WidthCm,
			LengthCm:           item.LengthCm,
			PackageFormat:      item.PackageFormat,
		}
	}

	previews := make([]OrderItemPreviewResponse, len(o.ItemsPreview))
	for i, p := range o.ItemsPreview {
		previews[i] = OrderItemPreviewResponse{
			ProductName:  p.ProductName,
			ProductImage: p.ProductImage,
			Quantity:     p.Quantity,
		}
	}

	return OrderResponse{
		ID:                    o.ID,
		ShortID:               o.ShortID,
		EventID:               o.EventID,
		EventTitle:            o.EventTitle,
		LiveSessionID:         o.EventID,
		LiveTitle:             o.EventTitle,
		LivePlatform:          o.LivePlatform,
		CustomerHandle:        o.CustomerHandle,
		CustomerID:            o.CustomerID,
		CustomerName:          o.CustomerName,
		CustomerEmail:         o.CustomerEmail,
		FreeShipping:          o.FreeShipping,
		Status:                o.Status,
		PaymentStatus:         o.PaymentStatus,
		ShipmentStatus:        o.ShipmentStatus,
		HasShipping:           o.HasShipping,
		Items:                 items,
		ItemsPreview:          previews,
		TotalItems:            o.TotalItems,
		TotalAmount:           o.TotalAmount,
		PaidAt:                o.PaidAt,
		CreatedAt:             o.CreatedAt,
		ExpiresAt:             o.ExpiresAt,
		IsFirstPurchase:       o.IsFirstPurchase,
		ERPFinalisationStatus: o.ERPFinalisationStatus,
	}
}

// NewListOrdersResponse maps a page of order outputs + pagination to the list response.
func NewListOrdersResponse(out ListOrdersOutput) ListOrdersResponse {
	responses := make([]OrderResponse, len(out.Orders))
	for i, o := range out.Orders {
		responses[i] = NewOrderResponse(o)
	}
	return ListOrdersResponse{
		Data:       responses,
		Pagination: query.NewPaginationResponse(out.Pagination, out.Total),
	}
}

// NewOrderStatsResponse maps the aggregated stats output to its response.
func NewOrderStatsResponse(o OrderStatsOutput) OrderStatsResponse {
	return OrderStatsResponse{
		TotalOrders:   o.TotalOrders,
		PendingOrders: o.PendingOrders,
		TotalRevenue:  o.TotalRevenue,
		AvgTicket:     o.AvgTicket,
	}
}

// NewRegenerateCheckoutResponse builds the admin-facing regenerate payload.
func NewRegenerateCheckoutResponse(token string, expiresAt time.Time) RegenerateCheckoutResponse {
	return RegenerateCheckoutResponse{Token: token, ExpiresAt: expiresAt}
}

// NewOrderDetailResponse maps the rich order-detail aggregate output to its
// response, including customer, address, shipping, shipment, store and ERP
// projections.
func NewOrderDetailResponse(o OrderDetailOutput) OrderDetailResponse {
	comments := make([]OrderCommentResponse, len(o.Comments))
	for i, c := range o.Comments {
		comments[i] = OrderCommentResponse{
			ID:        c.ID,
			Text:      c.Text,
			CreatedAt: c.CreatedAt,
		}
	}

	waitlist := make([]OrderWaitlistItemResponse, len(o.Waitlist))
	for i, w := range o.Waitlist {
		waitlist[i] = OrderWaitlistItemResponse{
			ID:           w.ID,
			ProductID:    w.ProductID,
			ProductName:  w.ProductName,
			ProductImage: w.ProductImage,
			Keyword:      w.Keyword,
			Quantity:     w.Quantity,
			UnitPrice:    w.UnitPrice,
			Position:     w.Position,
			Status:       w.Status,
			CreatedAt:    w.CreatedAt,
		}
	}

	resp := OrderDetailResponse{
		OrderResponse:          NewOrderResponse(o.OrderOutput),
		Token:                  o.Token,
		Comments:               comments,
		Waitlist:               waitlist,
		Notifications:          append([]OrderNotificationOutput{}, o.Notifications...),
		WaitlistJourney:        append([]OrderWaitlistJourneyOutput{}, o.WaitlistJourney...),
		PayableAmount:          o.PayableAmount,
		WaitlistedAmount:       o.WaitlistedAmount,
		PaymentMethod:          o.PaymentMethod,
		Installments:           o.Installments,
		DiscountCents:          o.DiscountCents,
		PaidTotalCents:         o.PaidTotalCents,
		CustomerBlocked:        o.CustomerBlocked,
		CancellationRevertedAt: o.CancellationRevertedAt,
	}

	if o.Customer != nil {
		resp.Customer = &OrderCustomerResponse{
			Name:     o.Customer.Name,
			Email:    o.Customer.Email,
			Document: o.Customer.Document,
			Phone:    o.Customer.Phone,
		}
	}
	if o.ShippingAddress != nil {
		resp.ShippingAddress = &OrderShippingAddressResponse{
			ZipCode:      o.ShippingAddress.ZipCode,
			Street:       o.ShippingAddress.Street,
			Number:       o.ShippingAddress.Number,
			Complement:   o.ShippingAddress.Complement,
			Neighborhood: o.ShippingAddress.Neighborhood,
			City:         o.ShippingAddress.City,
			State:        o.ShippingAddress.State,
		}
	}
	if o.Shipping != nil {
		resp.Shipping = &OrderShippingSelectionResp{
			Provider:      o.Shipping.Provider,
			ServiceID:     o.Shipping.ServiceID,
			ServiceName:   o.Shipping.ServiceName,
			Carrier:       o.Shipping.Carrier,
			CostCents:     o.Shipping.CostCents,
			RealCostCents: o.Shipping.RealCostCents,
			DeadlineDays:  o.Shipping.DeadlineDays,
			FreeShipping:  o.Shipping.FreeShipping,
		}
	}
	if o.Shipment != nil {
		events := make([]OrderShipmentEventResp, len(o.Shipment.Events))
		for i, e := range o.Shipment.Events {
			events[i] = OrderShipmentEventResp{
				Status:      e.Status,
				RawCode:     e.RawCode,
				RawName:     e.RawName,
				Observation: e.Observation,
				EventAt:     e.EventAt,
				Source:      e.Source,
			}
		}
		resp.Shipment = &OrderShipmentResponse{
			ID:                  o.Shipment.ID,
			Provider:            o.Shipment.Provider,
			ProviderOrderID:     o.Shipment.ProviderOrderID,
			ProviderOrderNumber: o.Shipment.ProviderOrderNumber,
			TrackingCode:        o.Shipment.TrackingCode,
			PublicTrackingURL:   o.Shipment.PublicTrackingURL,
			InvoiceKey:          o.Shipment.InvoiceKey,
			InvoiceKind:         o.Shipment.InvoiceKind,
			LabelURL:            o.Shipment.LabelURL,
			Status:              o.Shipment.Status,
			StatusRawCode:       o.Shipment.StatusRawCode,
			StatusRawName:       o.Shipment.StatusRawName,
			CreatedAt:           o.Shipment.CreatedAt,
			UpdatedAt:           o.Shipment.UpdatedAt,
			Events:              events,
		}
	}
	if o.ERPFinalisation != nil {
		resp.ERPFinalisation = &ERPFinalisationResponse{
			Status:        o.ERPFinalisation.Status,
			LastError:     o.ERPFinalisation.LastError,
			LastAttemptAt: o.ERPFinalisation.LastAttemptAt,
			AttemptsCount: o.ERPFinalisation.AttemptsCount,
			CanRetry:      o.ERPFinalisation.Status == "failed",
		}
	}
	if o.ERPInvoice != nil {
		resp.ERPInvoice = &ERPInvoiceResponse{
			InvoiceID:  o.ERPInvoice.InvoiceID,
			InvoiceKey: o.ERPInvoice.InvoiceKey,
			Status:     o.ERPInvoice.Status,
			EmittedAt:  o.ERPInvoice.EmittedAt,
		}
	}
	if o.Store != nil {
		resp.Store = &OrderStoreResponse{
			ID:       o.Store.ID,
			Name:     o.Store.Name,
			LogoURL:  o.Store.LogoURL,
			Document: o.Store.Document,
			Email:    o.Store.Email,
			Phone:    o.Store.Phone,
			Address: OrderStoreAddressResponse{
				ZipCode:      o.Store.Address.ZipCode,
				Street:       o.Store.Address.Street,
				Number:       o.Store.Address.Number,
				Complement:   o.Store.Address.Complement,
				Neighborhood: o.Store.Address.Neighborhood,
				City:         o.Store.Address.City,
				State:        o.Store.Address.State,
			},
			ShippingDefaults: OrderStoreShippingDefaults{
				PackageWeightGrams: o.Store.PackageWeightGrams,
				PackageFormat:      o.Store.PackageFormat,
			},
		}
	}
	return resp
}

// =============================================================================
// UPSELL / DOWNSELL TYPES
// =============================================================================

// OrderUpsellOutput is the per-order summary used by the dashboard upsell
// card. DeltaCents > 0 means the buyer added value at checkout (upsell);
// DeltaCents < 0 means they removed value (downsell). HasSnapshot is false
// for legacy orders that predate the feature — frontend renders a neutral
// empty state in that case.
type OrderUpsellOutput struct {
	HasSnapshot          bool                  `json:"hasSnapshot"`
	SnapshotTakenAt      *time.Time            `json:"snapshotTakenAt,omitempty"`
	InitialSubtotalCents int64                 `json:"initialSubtotalCents"`
	FinalSubtotalCents   int64                 `json:"finalSubtotalCents"`
	DeltaCents           int64                 `json:"deltaCents"`
	MutationCount        int                   `json:"mutationCount"`
	InitialItems         []OrderUpsellItem     `json:"initialItems"`
	Mutations            []OrderUpsellMutation `json:"mutations"`
}

type OrderUpsellItem struct {
	ProductID string  `json:"productId"`
	Name      string  `json:"name"`
	Keyword   string  `json:"keyword,omitempty"`
	ImageURL  *string `json:"imageUrl,omitempty"`
	Quantity  int     `json:"quantity"`
	UnitPrice int64   `json:"unitPrice"`
}

type OrderUpsellMutation struct {
	ID             string    `json:"id"`
	ProductID      string    `json:"productId"`
	ProductName    string    `json:"productName"`
	Keyword        string    `json:"keyword,omitempty"`
	ImageURL       *string   `json:"imageUrl,omitempty"`
	MutationType   string    `json:"mutationType"`
	QuantityBefore int       `json:"quantityBefore"`
	QuantityAfter  int       `json:"quantityAfter"`
	UnitPrice      int64     `json:"unitPrice"`
	Source         string    `json:"source"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Service layer
type ListOrdersInput struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    OrderFilters
}

type ListOrdersOutput struct {
	Orders     []OrderOutput
	Total      int
	Pagination query.Pagination
}

type OrderOutput struct {
	ID      string
	ShortID int
	// EventID/EventTitle são da CAMPANHA. O campo interno se chamava
	// LiveSessionID e sempre carregou row.EventID — o comentário "keeping
	// response field name for backwards compatibility" no service justificava o
	// nome do JSON, mas o campo Go também tinha herdado a mentira (RN-19).
	EventID               string
	EventTitle            string
	LivePlatform          string
	CustomerHandle        string
	CustomerID            string
	CustomerName          string
	CustomerEmail         string
	FreeShipping          bool
	Status                string
	PaymentStatus         string
	ShipmentStatus        string
	HasShipping           bool
	Items                 []OrderItemOutput
	ItemsPreview          []OrderItemPreviewOutput
	TotalItems            int
	TotalAmount           int64
	PaidAt                *time.Time
	CreatedAt             time.Time
	ExpiresAt             *time.Time
	IsFirstPurchase       bool
	ERPFinalisationStatus string // pending | done | failed
}

type OrderItemPreviewOutput struct {
	ProductName  string
	ProductImage *string
	Quantity     int
}

type OrderItemOutput struct {
	ID           string
	ProductID    string
	ProductName  string
	ProductImage *string
	Keyword      string
	Size         *string
	Quantity     int
	UnitPrice    int64
	TotalPrice   int64

	// WaitlistedQuantity é a parcela de Quantity que está em fila de espera.
	// Quantity - WaitlistedQuantity é o que a cliente pode pagar agora — a
	// mesma conta que o checkout público faz para montar o subtotal. Sempre 0
	// depois da venda materializada.
	WaitlistedQuantity int

	WeightGrams   int
	HeightCm      int
	WidthCm       int
	LengthCm      int
	PackageFormat string
}

type UpdateOrderInput struct {
	ID            string
	StoreID       string
	Status        *string
	PaymentStatus *string
}

// UpdateShippingAddressInput carries the scoping ids plus the parsed address map
// the repository persists into the cart's shipping_address JSONB.
type UpdateShippingAddressInput struct {
	ID      string
	StoreID string
	Address map[string]string
}

// ProductOrderBreakdownRow é um balde do "pedidos com este produto": quantos
// pedidos e quantas unidades do produto em cada par (status do carrinho,
// status de pagamento). O FE dobra os pares em rótulos amigáveis.
type ProductOrderBreakdownRow struct {
	Status        string `json:"status"`
	PaymentStatus string `json:"paymentStatus"`
	Orders        int    `json:"orders"`
	Units         int    `json:"units"`
}

type OrderStatsOutput struct {
	TotalOrders   int
	PendingOrders int
	TotalRevenue  int64
	AvgTicket     int64
}

// Repository layer
type ListOrdersParams struct {
	StoreID    string
	Search     string
	Pagination query.Pagination
	Sorting    query.Sorting
	Filters    OrderFilters
}

type ListOrdersResult struct {
	Orders []OrderRow
	Total  int
}

type OrderRow struct {
	ID              string
	ShortID         int
	EventID         string
	PlatformUserID  string
	PlatformHandle  string
	Token           string
	Status          string
	PaymentStatus   string
	PaidAt          *time.Time
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	CustomerName    string
	CustomerEmail   string
	LiveTitle       string
	FreeShipping    bool
	LivePlatform    string
	TotalAmount     int64
	TotalItems      int
	IsFirstPurchase bool
	// Latest shipment status for the cart, "" when no shipment exists yet.
	ShipmentStatus string
	// True when the buyer picked a shipping service at checkout and the cart
	// is still in a state where a shipment can be issued (i.e. payment is not
	// 'cancelled'/'refunded'). Lets the list show "Aguardando emissão" instead
	// of "Sem envio" between checkout and shipment creation, while keeping
	// terminal/non-shippable orders out of that label.
	HasShipping           bool
	ERPFinalisationStatus string
}

// OrderItemPreviewRow is the projection used by the list page to render an
// avatar stack of products on each row without loading every cart_items column.
type OrderItemPreviewRow struct {
	ProductName  string
	ProductImage *string
	Quantity     int
}

type OrderItemRow struct {
	ID             string
	CartID         string
	ProductID      string
	Size           *string
	Quantity       int
	UnitPrice      int64
	ProductName    string
	ProductImage   *string
	ProductKeyword string

	// WaitlistedQuantity é a parcela de Quantity SEM estoque — o que a cliente
	// pediu e a loja ainda não tem. Só existe enquanto o pedido é carrinho:
	// `order_items` não tem a coluna porque o snapshot registra o que foi
	// vendido, e vem 0 para pedido já materializado.
	WaitlistedQuantity int

	// Joined from products for the shipping flow. Zero when the product has
	// no dimensions filled in — service-layer sets them unchanged (0) so the
	// UI knows they are missing.
	WeightGrams   int
	HeightCm      int
	WidthCm       int
	LengthCm      int
	PackageFormat string
}

type OrderDetailRow struct {
	ID              string
	ShortID         int
	EventID         string
	PlatformUserID  string
	PlatformHandle  string
	Token           string
	Status          string
	PaymentStatus   string
	PaidAt          *time.Time
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	LiveTitle       string
	LivePlatform    string
	StoreID         string
	IsFirstPurchase bool

	// Customer captured at checkout (all optional — nil-safe reads).
	CustomerEmail    string
	CustomerName     string
	CustomerDocument string
	CustomerPhone    string

	// shipping_address JSONB — decoded into the fields below by the repo.
	ShippingAddressZip          string
	ShippingAddressStreet       string
	ShippingAddressNumber       string
	ShippingAddressComplement   string
	ShippingAddressNeighborhood string
	ShippingAddressCity         string
	ShippingAddressState        string

	// Cart freight selection (CartShippingSelection projection).
	ShippingProvider      string
	ShippingServiceID     string
	ShippingServiceName   string
	ShippingCarrier       string
	ShippingCostCents     int64
	ShippingCostRealCents int64
	ShippingDeadlineDays  int
	EventFreeShipping     bool

	// Store info for create-shipment pre-fill.
	StoreName                  string
	StoreLogoURL               *string
	StoreCNPJ                  string
	StoreEmail                 string
	StorePhone                 string
	StoreAddressZip            string
	StoreAddressStreet         string
	StoreAddressNumber         string
	StoreAddressComplement     string
	StoreAddressDistrict       string
	StoreAddressCity           string
	StoreAddressState          string
	StoreDefaultPkgWeightGrams int
	StoreDefaultPkgFormat      string

	// ERP finalisation lifecycle on the underlying cart. Surfaced on the
	// admin order detail so the page can show a "tentar novamente" banner
	// when the post-payment Tiny order creation failed mid-flow.
	ERPFinalisationStatus string
	ERPLastError          string
	ERPLastAttemptAt      *time.Time
	ERPAttemptsCount      int

	// ERP invoice (NFe) state captured by the Tiny webhook or the manual
	// "Verificar NFe" button. Empty status means no NFe has been linked yet
	// — that's the "Aguardando NFe" branch on the FE.
	ERPInvoiceID        string
	ERPInvoiceKey       string
	ERPInvoiceStatus    string
	ERPInvoiceEmittedAt *time.Time

	// Preenchido quando o lojista cancelou este pedido e o pagamento entrou
	// assim mesmo, revertendo o cancelamento. NULL na esmagadora maioria.
	CancellationRevertedAt *time.Time

	// Pagamento: método (pix/credit_card/...), parcelas (do gateway_snapshot) e
	// os valores REAIS do pedido — desconto (cupom + PIX) e valor efetivamente
	// pago. Zero quando ainda não há pedido/pagamento materializado.
	PaymentMethod  string
	Installments   int
	DiscountCents  int64
	PaidTotalCents int64
}

// OrderShipmentRecord is the projection of `shipments` used by order service.
// Kept local to avoid importing the integration package here (just a SQL shape).
type OrderShipmentRecord struct {
	ID                  string
	Provider            string
	ProviderOrderID     string
	ProviderOrderNumber string
	TrackingCode        string
	PublicTrackingURL   string
	InvoiceKey          string
	InvoiceKind         string
	LabelURL            string
	Status              string
	StatusRawCode       int
	StatusRawName       string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// OrderShipmentEventRecord is the projection of `shipment_tracking_events`.
type OrderShipmentEventRecord struct {
	Status      string
	RawCode     int
	RawName     string
	Observation string
	EventAt     time.Time
	Source      string
}

type CommentRow struct {
	ID        string
	Text      string
	CreatedAt time.Time
}

type CommentOutput struct {
	ID        string
	Text      string
	CreatedAt time.Time
}

// OrderShippingAddressOutput is the parsed shipping_address JSONB projection.
type OrderShippingAddressOutput struct {
	ZipCode      string
	Street       string
	Number       string
	Complement   string
	Neighborhood string
	City         string
	State        string
}

// OrderCustomerOutput mirrors customer_* columns on carts.
type OrderCustomerOutput struct {
	Name     string
	Email    string
	Document string
	Phone    string
}

// OrderShippingOutput is the cart's chosen freight option.
type OrderShippingOutput struct {
	Provider      string
	ServiceID     string
	ServiceName   string
	Carrier       string
	CostCents     int64
	RealCostCents int64
	DeadlineDays  int
	FreeShipping  bool
}

// OrderStoreOutput is the store origin info.
type OrderStoreOutput struct {
	ID                 string
	Name               string
	LogoURL            *string
	Document           string
	Email              string
	Phone              string
	Address            OrderShippingAddressOutput // reused shape
	PackageWeightGrams int
	PackageFormat      string
}

// OrderShipmentOutput bundles the shipment record + its tracking events.
type OrderShipmentOutput struct {
	OrderShipmentRecord
	Events []OrderShipmentEventRecord
}

type OrderDetailOutput struct {
	OrderOutput
	// Cart token used to build the public checkout link (/cart/{token}). Only
	// surfaced on the detail endpoint because the admin actions menu builds the
	// shareable URL from it.
	Token           string
	Comments        []CommentOutput
	Customer        *OrderCustomerOutput
	ShippingAddress *OrderShippingAddressOutput
	Shipping        *OrderShippingOutput
	Shipment        *OrderShipmentOutput
	Store           *OrderStoreOutput
	ERPFinalisation *ERPFinalisationOutput
	// ERPInvoice is populated once the merchant emits the NFe in the ERP.
	// Drives the "Aguardando NFe" / "Criar envio" gate on the order detail
	// page — pointer so the FE can distinguish "no NFe yet" (nil) from
	// "rejected/cancelled" (status is set, key may be empty).
	ERPInvoice *ERPInvoiceOutput
	// CustomerBlocked is true when the buyer's Instagram handle is currently
	// blocked for this store. Drives the "Cliente bloqueado" badge on the
	// order detail page. Doesn't affect past orders — informational only.
	CustomerBlocked bool
	// CancellationRevertedAt marca o caso em que a loja cancelou este pedido e
	// o pagamento entrou assim mesmo: o cancelamento foi revertido e o pedido
	// seguiu o fluxo normal. Vira uma entrada no histórico do pedido para o
	// lojista entender por que um pedido "cancelado" está pago.
	CancellationRevertedAt *time.Time
	// Waitlist são os produtos que a cliente pediu, a loja não tinha e ela
	// entrou na fila. Não são itens pagáveis: não entram no total e não vão
	// para a transportadora. Aparecem no detalhe porque é a única forma do
	// lojista saber o que prometeu — e porque o orçamento impresso precisa
	// dizer à cliente o que está reservado e o que está esperando reposição.
	//
	// Mesma fonte do checkout público (waitlist_items com status ativo), para
	// que o que o lojista lê em voz alta seja exatamente o que a cliente vê.
	Waitlist []OrderWaitlistItemOutput
	// Notifications e WaitlistJourney alimentam a árvore de histórico — ver
	// os tipos correspondentes acima.
	Notifications   []OrderNotificationOutput
	WaitlistJourney []OrderWaitlistJourneyOutput

	// PayableAmount é o que a cliente consegue pagar AGORA: só as unidades com
	// estoque. TotalAmount soma a quantidade CHEIA (é a fonte da receita do
	// evento e do valor que a lista mostra) e por isso inclui o que está em
	// fila. Quando não há fila os dois são idênticos.
	//
	// O orçamento impresso usa este número: cobrar pela unidade que a loja não
	// tem para entregar é a única forma de o documento sair errado.
	PayableAmount int64
	// WaitlistedAmount é o valor das unidades em fila — o que o orçamento
	// declara como NÃO incluído no total, em vez de apenas omitir.
	WaitlistedAmount int64

	// Pagamento (método/parcelas) e os valores reais do pedido: desconto (cupom +
	// PIX) e valor efetivamente pago. Ver OrderDetailRow.
	PaymentMethod  string
	Installments   int
	DiscountCents  int64
	PaidTotalCents int64
}

// OrderWaitlistItemOutput é uma entrada de fila de espera do carrinho.
type OrderWaitlistItemOutput struct {
	ID           string
	ProductID    string
	ProductName  string
	ProductImage *string
	Keyword      string
	Quantity     int
	UnitPrice    int64
	// Position é o lugar na fila daquele produto no evento — 1 é o próximo a
	// ser atendido quando o estoque voltar.
	Position int
	// Status é waiting (esperando estoque) ou notified (estoque voltou e o
	// prazo extra dela está correndo). Encerradas não chegam aqui.
	Status    string
	CreatedAt time.Time
}

// ERPInvoiceOutput surfaces the persisted erp_invoice_* state on the cart.
// The FE reads InvoiceKey to pre-fill "Criar envio" and Status to decide
// which copy to show.
type ERPInvoiceOutput struct {
	InvoiceID  string
	InvoiceKey string
	Status     string // pending | authorized | cancelled | rejected
	EmittedAt  *time.Time
}

// ERPFinalisationOutput exposes the cart's ERP finalisation lifecycle to the
// admin order detail page. Drives the "tentar novamente" banner: when status
// is "failed" the FE shows the error and a retry button; otherwise the
// section stays hidden.
type ERPFinalisationOutput struct {
	Status        string     // pending | done | failed
	LastError     string     // populated when Status == "failed"
	LastAttemptAt *time.Time // most recent attempt (success or failure)
	AttemptsCount int        // includes the initial finalisation
}
