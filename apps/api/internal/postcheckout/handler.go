package postcheckout

import (
	"crypto/subtle"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"livecart/apps/api/lib/httpx"
)

// Handler exposes the public order-tracking endpoint. The URL surfaced to the
// customer is `/order/{shortId}?key={token}` — the FE forwards both into this
// handler. ShortId is decorative (human-friendly), the security boundary is
// the token compared in constant time against the persisted value.
type Handler struct {
	repo   *Repository
	logger *zap.Logger
}

func NewHandler(repo *Repository, logger *zap.Logger) *Handler {
	return &Handler{repo: repo, logger: logger.Named("postcheckout-handler")}
}

func (h *Handler) RegisterRoutes(app *fiber.App) {
	// Public — no auth, token in query string is the secret.
	public := app.Group("/api/public/orders")
	public.Get("/:shortId", h.GetOrder)
}

// PublicOrderResponse is the slim, customer-safe view of a paid order.
// We deliberately exclude PII like CPF and full address that the customer
// already typed; we echo back only what's helpful on the tracking page.
type PublicOrderResponse struct {
	ShortID         string                  `json:"short_id"`
	StoreName       string                  `json:"store_name"`
	StoreLogoURL    string                  `json:"store_logo_url,omitempty"`
	Status          string                  `json:"status"` // paid | shipped | delivered
	PaidAt          *string                 `json:"paid_at,omitempty"`
	CustomerName    string                  `json:"customer_name"`
	CustomerEmail   string                  `json:"customer_email"`
	Items           []PublicOrderItem       `json:"items"`
	Subtotal        int64                   `json:"subtotal_cents"`
	ShippingCents   int64                   `json:"shipping_cents"`
	DiscountCents   int64                   `json:"discount_cents"`
	TotalCents      int64                   `json:"total_cents"`
	ShippingAddress *PublicShippingAddress  `json:"shipping_address,omitempty"`
	Shipping        *PublicShippingSummary  `json:"shipping,omitempty"`
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

	cart, err := h.repo.GetCartByTrackingToken(c.Context(), token)
	if err != nil || cart == nil {
		// 404 instead of 401: don't leak that the token format is correct.
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	// Constant-time token comparison defends against timing oracles. Belt and
	// suspenders — Postgres lookup already gates on equality.
	if subtle.ConstantTimeCompare([]byte(cart.TrackingToken.String), []byte(token)) != 1 {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	if int(cart.ShortID) != shortID {
		return httpx.ErrNotFound("Pedido não encontrado")
	}

	snapshot, err := h.repo.LoadCart(c.Context(), cart.ID.String())
	if err != nil {
		h.logger.Error("failed to load order snapshot",
			zap.String("cart_id", cart.ID.String()),
			zap.Error(err),
		)
		return httpx.ErrInternal("Erro ao carregar pedido")
	}

	return httpx.OK(c, toPublicResponse(snapshot))
}

func toPublicResponse(s *CartSnapshot) PublicOrderResponse {
	resp := PublicOrderResponse{
		ShortID:       strconv.Itoa(int(s.Cart.ShortID)),
		StoreName:     s.Store.Name,
		Status:        derivePublicStatus(s),
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

// derivePublicStatus maps the cart's payment_status into the customer-facing
// state machine surfaced on the public page. Today we only have "paid" — the
// "shipped" and "delivered" transitions land in Phases 2 and 3.
func derivePublicStatus(s *CartSnapshot) string {
	switch s.Cart.PaymentStatus.String {
	case "paid":
		return "paid"
	case "refunded":
		return "refunded"
	default:
		return "paid"
	}
}
