package postcheckout

import (
	"crypto/subtle"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// Local alias so the handler doesn't have to drag the full sqlc package
// reference through every helper signature.
type sqlcOrderEvent = sqlc.OrderEvent

// Handler exposes the public order-tracking endpoint. The URL surfaced to the
// customer is `/order/{shortId}?key={token}` — the FE forwards both into this
// handler. ShortId is decorative (human-friendly), the security boundary is
// the token compared in constant time against the persisted value.
type Handler struct {
	repo    *Repository
	service *Service
	pool    *pgxpool.Pool
	logger  *zap.Logger
}

func NewHandler(repo *Repository, service *Service, pool *pgxpool.Pool, logger *zap.Logger) *Handler {
	return &Handler{repo: repo, service: service, pool: pool, logger: logger.Named("postcheckout-handler")}
}

// RegisterPublicRoutes mounts the unauthenticated routes under /api/public/.
func (h *Handler) RegisterPublicRoutes(app *fiber.App) {
	public := app.Group("/api/public/orders")
	public.Get("/:shortId", h.GetOrder)
	public.Post("/:shortId/confirm-delivery", h.ConfirmDelivery)
}

// RegisterMerchantRoutes mounts the auth-protected, store-scoped routes.
// Caller passes the storeScoped group so this handler inherits both
// AuthMiddleware and the :storeId resolution.
func (h *Handler) RegisterMerchantRoutes(g fiber.Router) {
	g.Post("/orders/:id/mark-delivered", h.MarkDelivered)
}

// PublicOrderResponse is the slim, customer-safe view of a paid order.
// We deliberately exclude PII like CPF and full address that the customer
// already typed; we echo back only what's helpful on the tracking page.
type PublicOrderResponse struct {
	ShortID         string                 `json:"short_id"`
	StoreName       string                 `json:"store_name"`
	StoreLogoURL    string                 `json:"store_logo_url,omitempty"`
	Status          string                 `json:"status"` // paid | shipped | delivered
	PaidAt          *string                `json:"paid_at,omitempty"`
	CustomerName    string                 `json:"customer_name"`
	CustomerEmail   string                 `json:"customer_email"`
	Items           []PublicOrderItem      `json:"items"`
	Subtotal        int64                  `json:"subtotal_cents"`
	ShippingCents   int64                  `json:"shipping_cents"`
	DiscountCents   int64                  `json:"discount_cents"`
	TotalCents      int64                  `json:"total_cents"`
	ShippingAddress *PublicShippingAddress `json:"shipping_address,omitempty"`
	Shipping        *PublicShippingSummary `json:"shipping,omitempty"`
	Events          []PublicOrderEvent     `json:"events"`
}

// PublicOrderEvent is one timeline entry. The customer sees these mapped onto
// the visual status bar; type drives the icon + label, occurred_at drives
// "há X minutos".
type PublicOrderEvent struct {
	Type       string `json:"type"` // payment_confirmed | shipped | delivered
	OccurredAt string `json:"occurred_at"`
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle,omitempty"`
	Source     string `json:"source"` // system | merchant | customer
}

type PublicOrderItem struct {
	ProductName     string `json:"product_name"`
	ProductImageURL string `json:"product_image_url,omitempty"`
	Quantity        int32  `json:"quantity"`
	UnitPriceCents  int64  `json:"unit_price_cents"`
	LineTotalCents  int64  `json:"line_total_cents"`
}

type PublicShippingAddress struct {
	City  string `json:"city"`
	State string `json:"state"`
}

type PublicShippingSummary struct {
	ServiceName  string `json:"service_name,omitempty"`
	Carrier      string `json:"carrier,omitempty"`
	TrackingCode string `json:"tracking_code,omitempty"`
	DeadlineDays int32  `json:"deadline_days,omitempty"`
}

// GetOrder handles GET /api/public/orders/:shortId?key={token}.
// Looks up the cart by token (constant-time compared) and returns the public
// view if the URL-bound shortId matches.
func (h *Handler) GetOrder(c *fiber.Ctx) error {
	shortIDParam := c.Params("shortId")
	token := c.Query("key")

	if shortIDParam == "" || token == "" {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	shortID, err := strconv.Atoi(shortIDParam)
	if err != nil {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	cart, resolvedToken, err := h.repo.GetCartByTrackingToken(c.Context(), token)
	if err != nil || cart == nil {
		// 404 instead of 401: don't leak that the token format is correct.
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	// Constant-time token comparison defends against timing oracles. Belt and
	// suspenders — Postgres lookup already gates on equality against
	// order_logistics.tracking_token (the source of truth).
	if subtle.ConstantTimeCompare([]byte(resolvedToken), []byte(token)) != 1 {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	if int(cart.ShortID) != shortID {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	snapshot, err := h.repo.LoadCart(c.Context(), cart.ID.String())
	if err != nil {
		logger.From(c.Context(), h.logger).Error("failed to load order snapshot",
			zap.String("cart_id", cart.ID.String()),
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao carregar pedido")
	}
	// Rota pública por tracking token: o store só é conhecido após o snapshot.
	ctx := logger.WithStore(c.Context(), uuidStr(snapshot.Store.ID), snapshot.Store.Slug)

	events, err := h.repo.ListOrderEvents(c.Context(), cart.ID.String())
	if err != nil {
		// Don't fail the whole request — timeline is auxiliary, the order
		// summary itself is the primary value.
		logger.From(ctx, h.logger).Warn("failed to load order events",
			zap.String("cart_id", cart.ID.String()),
			zap.Error(err),
		)
	}

	var shipment *ShipmentSummary
	if h.pool != nil {
		shipment, err = h.repo.GetShipmentSummary(c.Context(), h.pool, cart.ID.String())
		if err != nil {
			logger.From(ctx, h.logger).Warn("failed to load shipment summary",
				zap.String("cart_id", cart.ID.String()),
				zap.Error(err),
			)
		}
	}

	resp := toPublicResponse(snapshot)
	resp.Events = make([]PublicOrderEvent, 0, len(events))
	for _, ev := range events {
		// `receipt_sent` is an internal exactly-once marker for the receipt email
		// (SendPaidReceipt), not a customer-facing milestone — hide it.
		if ev.EventType == "receipt_sent" {
			continue
		}
		resp.Events = append(resp.Events, eventToResponse(ev.EventType, ev.OccurredAt.Time, ev.Source))
	}
	resp.Status = deriveStatusFromEvents(events, snapshot)
	if shipment != nil && shipment.TrackingCode != "" {
		if resp.Shipping == nil {
			resp.Shipping = &PublicShippingSummary{}
		}
		resp.Shipping.TrackingCode = shipment.TrackingCode
	}

	return httpx.OK(c, resp)
}

// ConfirmDelivery handles POST /api/public/orders/:shortId/confirm-delivery
// when the customer clicks "Recebi!" on the public page. Token-gated, same
// auth model as GetOrder.
func (h *Handler) ConfirmDelivery(c *fiber.Ctx) error {
	shortIDParam := c.Params("shortId")
	token := c.Query("key")
	if shortIDParam == "" || token == "" {
		return httpx.ErrNotFound("Pedido não encontrado")
	}
	shortID, err := strconv.Atoi(shortIDParam)
	if err != nil {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	cart, resolvedToken, err := h.repo.GetCartByTrackingToken(c.Context(), token)
	if err != nil || cart == nil {
		return httpx.ErrNotFound("Pedido não encontrado")
	}
	if subtle.ConstantTimeCompare([]byte(resolvedToken), []byte(token)) != 1 {
		return httpx.ErrNotFound("Pedido não encontrado")
	}
	if int(cart.ShortID) != shortID {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	h.service.OnDelivered(c.Context(), cart.ID.String(), "customer")
	return httpx.OK(c, fiber.Map{"confirmed": true})
}

// MarkDelivered handles POST /api/v1/stores/:storeId/orders/:id/mark-delivered
// when the merchant clicks the button in the dashboard. Verifies the cart
// belongs to the authenticated store before firing the hook.
func (h *Handler) MarkDelivered(c *fiber.Ctx) error {
	storeID, ok := c.Locals("store_id").(string)
	if !ok || storeID == "" {
		return httpx.ErrForbidden("Loja não identificada")
	}
	cartID := c.Params("id")
	if cartID == "" {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	snapshot, err := h.repo.LoadCart(c.Context(), cartID)
	if err != nil {
		return httpx.ErrNotFound("Pedido não encontrado")
	}
	if snapshot.Event.StoreID.String() != storeID {
		// Don't leak existence — same response code as not found.
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	h.service.OnDelivered(c.Context(), cartID, "merchant")
	return httpx.OK(c, fiber.Map{"confirmed": true})
}

func eventToResponse(eventType string, occurred time.Time, source string) PublicOrderEvent {
	title, subtitle := eventLabel(eventType)
	return PublicOrderEvent{
		Type:       eventType,
		OccurredAt: occurred.Format(time.RFC3339),
		Title:      title,
		Subtitle:   subtitle,
		Source:     source,
	}
}

func eventLabel(eventType string) (string, string) {
	switch eventType {
	case "payment_confirmed":
		return "Pagamento confirmado", "Recebemos seu pagamento"
	case "shipped":
		return "Pedido enviado", "Sua encomenda foi despachada"
	case "delivered":
		return "Pedido entregue", "Esperamos que tenha gostado"
	default:
		return eventType, ""
	}
}

// deriveStatusFromEvents picks the latest non-refund event as the canonical
// status. Falls back to payment_status when no events exist (defensive — the
// migration backfilled paid carts, but this guards data drift).
func deriveStatusFromEvents(events []sqlcOrderEvent, snapshot *CartSnapshot) string {
	if snapshot.Cart.PaymentStatus.String == "refunded" {
		return "refunded"
	}
	// Walk in reverse — events come back chronologically, latest one wins.
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].EventType {
		case "delivered":
			return "delivered"
		case "shipped":
			return "shipped"
		case "payment_confirmed":
			return "paid"
		}
	}
	if snapshot.Cart.PaymentStatus.String == "paid" {
		return "paid"
	}
	return "paid"
}

func toPublicResponse(s *CartSnapshot) PublicOrderResponse {
	resp := PublicOrderResponse{
		ShortID:       strconv.Itoa(int(s.Cart.ShortID)),
		StoreName:     s.Store.Name,
		Status:        "paid", // overwritten by deriveStatusFromEvents in caller
		CustomerName:  s.Cart.CustomerName.String,
		CustomerEmail: s.Cart.CustomerEmail.String,
	}
	if s.Store.LogoUrl.Valid {
		resp.StoreLogoURL = s.Store.LogoUrl.String
	}
	if s.Cart.PaidAt.Valid {
		ts := s.Cart.PaidAt.Time.Format("2006-01-02T15:04:05Z07:00")
		resp.PaidAt = &ts
	}

	addr := ParseShippingAddress(s.Cart.ShippingAddress)
	if addr.City != "" || addr.State != "" {
		resp.ShippingAddress = &PublicShippingAddress{City: addr.City, State: addr.State}
	}

	var subtotal int64
	for _, it := range s.Items {
		if !it.Quantity.Valid || !it.UnitPrice.Valid {
			continue
		}
		qty := it.Quantity.Int32
		unit := it.UnitPrice.Int64
		line := int64(qty) * unit
		subtotal += line

		img := ""
		if it.ProductImageUrl.Valid {
			img = it.ProductImageUrl.String
		}
		resp.Items = append(resp.Items, PublicOrderItem{
			ProductName:     it.ProductName,
			ProductImageURL: img,
			Quantity:        qty,
			UnitPriceCents:  unit,
			LineTotalCents:  line,
		})
	}
	resp.Subtotal = subtotal

	if s.Cart.ShippingCostCents.Valid {
		resp.ShippingCents = s.Cart.ShippingCostCents.Int64
	}
	resp.DiscountCents = s.Cart.CouponDiscountCents
	resp.TotalCents = subtotal + resp.ShippingCents - resp.DiscountCents
	if resp.TotalCents < 0 {
		resp.TotalCents = 0
	}

	// Shipping summary is only meaningful once the merchant has selected a
	// service. Tracking_code is currently null — Phase 2/3 will populate it
	// from the existing shipments table.
	if s.Cart.ShippingProvider.Valid && s.Cart.ShippingProvider.String != "" {
		shipSummary := &PublicShippingSummary{}
		if s.Cart.ShippingServiceName.Valid {
			shipSummary.ServiceName = s.Cart.ShippingServiceName.String
		}
		if s.Cart.ShippingCarrier.Valid {
			shipSummary.Carrier = s.Cart.ShippingCarrier.String
		}
		if s.Cart.ShippingDeadlineDays.Valid {
			shipSummary.DeadlineDays = s.Cart.ShippingDeadlineDays.Int32
		}
		resp.Shipping = shipSummary
	}

	return resp
}
