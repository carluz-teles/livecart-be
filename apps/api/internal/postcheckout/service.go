package postcheckout

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"strings"

	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/email"
)

// Service orchestrates the post-checkout customer flow: token generation +
// transactional emails. Today only "Pagamento confirmado" is wired; later
// phases will add shipped/delivered.
type Service struct {
	repo   *Repository
	email  *email.Client
	logger *zap.Logger
}

func NewService(repo *Repository, emailClient *email.Client, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		email:  emailClient,
		logger: logger.Named("postcheckout"),
	}
}

// OnCartPaid is the hook called when a cart's payment_status flips to "paid".
// It is best-effort: every error is logged but never returned, because the
// caller is a payment webhook handler that must ACK regardless.
//
// Idempotency: the cart's tracking_token is the marker. If it is already set
// we assume the email was sent and skip — this is robust to webhook retries
// without needing a separate notification log.
func (s *Service) OnCartPaid(ctx context.Context, cartID string) {
	snapshot, err := s.repo.LoadCart(ctx, cartID)
	if err != nil {
		s.logger.Warn("post-checkout flow could not load cart",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}

	// Already processed — webhook retry or merchant manual mark-as-paid.
	if snapshot.Cart.TrackingToken.Valid && snapshot.Cart.TrackingToken.String != "" {
		s.logger.Debug("post-checkout already ran for cart",
			zap.String("cart_id", cartID),
		)
		return
	}

	token, err := generateTrackingToken()
	if err != nil {
		s.logger.Error("failed to generate tracking token",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}
	if err := s.repo.SetTrackingToken(ctx, cartID, token); err != nil {
		s.logger.Error("failed to persist tracking token",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}

	// No customer email captured (rare — checkout currently requires it, but
	// this guards data drift). Token is still saved so the customer can be
	// linked manually if they ask.
	customerEmail := snapshot.Cart.CustomerEmail.String
	if customerEmail == "" {
		s.logger.Info("cart has no customer email, skipping receipt",
			zap.String("cart_id", cartID),
		)
		return
	}

	input := buildEmailInput(snapshot, token)
	if err := s.email.SendOrderPaid(ctx, input); err != nil {
		s.logger.Warn("failed to send order paid email",
			zap.String("cart_id", cartID),
			zap.String("to", customerEmail),
			zap.Error(err),
		)
		return
	}

	s.logger.Info("post-checkout flow completed",
		zap.String("cart_id", cartID),
		zap.String("to", customerEmail),
	)
}

func buildEmailInput(s *CartSnapshot, trackingToken string) email.OrderPaidEmailInput {
	customerName := s.Cart.CustomerName.String
	if customerName == "" {
		customerName = s.Cart.PlatformHandle
	}
	if customerName == "" {
		customerName = "cliente"
	}

	addr := ParseShippingAddress(s.Cart.ShippingAddress)

	carrierLine := ""
	if s.Cart.ShippingServiceName.Valid && s.Cart.ShippingServiceName.String != "" {
		carrierLine = s.Cart.ShippingServiceName.String
		if s.Cart.ShippingCarrier.Valid && s.Cart.ShippingCarrier.String != "" {
			carrierLine = fmt.Sprintf("%s · %s", s.Cart.ShippingServiceName.String, s.Cart.ShippingCarrier.String)
		}
	}

	logoURL := ""
	if s.Store.LogoUrl.Valid {
		logoURL = s.Store.LogoUrl.String
	}

	frontendURL := strings.TrimRight(config.FrontendURL.StringOr("http://localhost:3000"), "/")
	trackingURL := fmt.Sprintf("%s/order/%d?key=%s",
		frontendURL,
		s.Cart.ShortID,
		trackingToken,
	)

	return email.OrderPaidEmailInput{
		StoreName:      s.Store.Name,
		StoreLogoURL:   logoURL,
		ToEmail:        s.Cart.CustomerEmail.String,
		ToName:         customerName,
		OrderShortID:   fmt.Sprintf("%d", s.Cart.ShortID),
		TotalFormatted: formatTotal(s),
		ItemsHTML:      renderItemsHTML(s.Items),
		ShippingLine:   formatShippingLine(addr),
		CarrierLine:    carrierLine,
		TrackingURL:    trackingURL,
	}
}

// renderItemsHTML emits the <tr>...</tr> rows for the items table inside the
// email shell. Inline styles are kept verbose-but-clear for email clients
// that strip <style> blocks (Gmail, Outlook).
func renderItemsHTML(items []sqlc.ListCartItemsRow) string {
	var b bytes.Buffer
	for _, it := range items {
		if !it.Quantity.Valid || !it.UnitPrice.Valid {
			continue
		}
		qty := int64(it.Quantity.Int32)
		unit := it.UnitPrice.Int64
		line := qty * unit
		b.WriteString(`<tr><td style="padding: 8px 0; vertical-align: top;">`)
		b.WriteString(`<span style="color:#111827;font-size:14px;font-weight:500;">`)
		b.WriteString(html.EscapeString(it.ProductName))
		b.WriteString(`</span>`)
		b.WriteString(`<br><span style="color:#9ca3af;font-size:12px;">`)
		b.WriteString(fmt.Sprintf("%d × R$ %d,%02d", qty, unit/100, unit%100))
		b.WriteString(`</span></td>`)
		b.WriteString(`<td style="padding: 8px 0; text-align: right; vertical-align: top; color:#111827;font-size:14px;font-weight:500;">`)
		b.WriteString(fmt.Sprintf("R$ %d,%02d", line/100, line%100))
		b.WriteString(`</td></tr>`)
	}
	return b.String()
}

// formatTotal computes the cart total from item lines + shipping − coupon.
// We don't trust an aggregate column here because the Cart row doesn't carry
// a precomputed total; the canonical source is items × unit_price + shipping.
func formatTotal(s *CartSnapshot) string {
	var sumCents int64
	for _, it := range s.Items {
		if !it.Quantity.Valid || !it.UnitPrice.Valid {
			continue
		}
		sumCents += int64(it.Quantity.Int32) * it.UnitPrice.Int64
	}
	if s.Cart.ShippingCostCents.Valid {
		sumCents += s.Cart.ShippingCostCents.Int64
	}
	sumCents -= s.Cart.CouponDiscountCents
	if sumCents < 0 {
		sumCents = 0
	}
	return fmt.Sprintf("R$ %d,%02d", sumCents/100, sumCents%100)
}

func formatShippingLine(a ShippingAddressJSON) string {
	if a.Street == "" && a.City == "" {
		return ""
	}
	var b bytes.Buffer
	if a.Street != "" {
		b.WriteString(a.Street)
	}
	if a.Number != "" {
		b.WriteString(", ")
		b.WriteString(a.Number)
	}
	if a.City != "" {
		if b.Len() > 0 {
			b.WriteString(" — ")
		}
		b.WriteString(a.City)
		if a.State != "" {
			b.WriteString("/")
			b.WriteString(a.State)
		}
	}
	return b.String()
}

