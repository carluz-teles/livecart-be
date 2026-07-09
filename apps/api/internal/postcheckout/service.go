package postcheckout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/notification"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/email"
)

// Service orchestrates the post-checkout customer flow: token generation +
// transactional emails. Reads merchant-customizable templates from
// notification.Service when configured and falls back to hardcoded defaults
// when the merchant hasn't overridden them.
type Service struct {
	repo                *Repository
	email               *email.Client
	notificationService NotificationSettingsReader
	logger              *zap.Logger
	gmvReporter GMVReporter // optional — Stripe metered GMV fee (PRD 007)
}

// NotificationSettingsReader is the slice of notification.Service this
// package needs. Defined locally to avoid an import cycle and to keep the
// dependency surface minimal.
type NotificationSettingsReader interface {
	GetSettings(ctx context.Context, storeID string) (*notification.Settings, error)
}

func NewService(repo *Repository, emailClient *email.Client, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		email:  emailClient,
		logger: logger.Named("postcheckout"),
	}
}

// SetNotificationService wires the per-store template overrides. When unset,
// the service uses hardcoded defaults — handy in tests and for graceful
// rollout (BE deploys before FE editor exists).
// GMVReporter reports paid GMV for the metered subscription fee (PRD 007).
// Implemented by billing.Service; narrow interface keeps packages decoupled.
type GMVReporter interface {
	ReportPaidGMV(ctx context.Context, storeID, cartID string, amountCents int64)
}

// SetGMVReporter wires the metered-fee reporter (optional).
func (s *Service) SetGMVReporter(r GMVReporter) {
	s.gmvReporter = r
}

func (s *Service) SetNotificationService(reader NotificationSettingsReader) {
	s.notificationService = reader
}

// OnCartPaid is the hook called when a cart's payment_status flips to "paid".
// It is best-effort: every error is logged but never returned, because the
// caller is a payment webhook handler that must ACK regardless.
//
// Idempotency lives in two places: the tracking_token (set once, never
// rotated) and the order_events unique index. Either is enough to short
// circuit duplicate runs.
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

	// Metered GMV fee (PRD 007): report the paid total to Stripe. Runs after
	// the tracking-token guard above, so webhook retries never double-report.
	if s.gmvReporter != nil {
		total := int64(0)
		for _, item := range snapshot.Items {
			total += itemLineTotalCents(item)
		}
		s.gmvReporter.ReportPaidGMV(ctx, uuidStr(snapshot.Store.ID), cartID, total)
	}

	// Append `payment_confirmed` to the customer-facing timeline. Insert is
	// idempotent at the DB level (unique cart_id+event_type) — duplicate
	// hooks are a no-op.
	if _, err := s.repo.InsertOrderEvent(ctx, cartID, "payment_confirmed", "system", nil); err != nil {
		s.logger.Warn("failed to record payment_confirmed event",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}

	// Marca como 'fulfilled' qualquer waitlist em status='notified' deste
	// cart — o cliente pagou dentro da janela. Itens em 'waiting' (que ele
	// optou por deixar na fila) permanecem.
	if err := s.repo.MarkWaitlistFulfilledByCart(ctx, cartID); err != nil {
		s.logger.Warn("failed to mark notified waitlist as fulfilled",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
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
	s.applyPaidOverride(ctx, snapshot, &input)
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

// OnDelivered records the final state in the timeline. source distinguishes
// who confirmed the delivery: the merchant clicking in the dashboard, the
// customer clicking on the public page, or the auto-poller (system) seeing
// the carrier flip the SmartEnvios status to "entregue". Same idempotency
// guarantees as OnShipmentPosted: one delivered event per cart.
func (s *Service) OnDelivered(ctx context.Context, cartID, source string) {
	if source == "" {
		source = "system"
	}

	snapshot, err := s.repo.LoadCart(ctx, cartID)
	if err != nil {
		s.logger.Warn("post-checkout delivered hook could not load cart",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}

	metadata := json.RawMessage(fmt.Sprintf(`{"source":%q}`, source))
	inserted, err := s.repo.InsertOrderEvent(ctx, cartID, "delivered", source, metadata)
	if err != nil {
		s.logger.Warn("failed to record delivered event",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}
	if !inserted {
		return
	}

	customerEmail := snapshot.Cart.CustomerEmail.String
	if customerEmail == "" {
		return
	}

	// Skip the email when the customer themselves confirmed — they don't need
	// to be notified about something they just clicked. Other paths (merchant,
	// system poller) still send.
	if source == "customer" {
		s.logger.Info("delivery confirmed by customer, skipping email",
			zap.String("cart_id", cartID),
		)
		return
	}

	customerName := snapshot.Cart.CustomerName.String
	if customerName == "" {
		customerName = snapshot.Cart.PlatformHandle
	}
	if customerName == "" {
		customerName = "cliente"
	}
	logoURL := ""
	if snapshot.Store.LogoUrl.Valid {
		logoURL = snapshot.Store.LogoUrl.String
	}

	deliveredInput := email.OrderDeliveredEmailInput{
		StoreName:    snapshot.Store.Name,
		StoreLogoURL: logoURL,
		ToEmail:      customerEmail,
		ToName:       customerName,
		OrderShortID: fmt.Sprintf("%d", snapshot.Cart.ShortID),
		ReplyTo:      storeReplyTo(snapshot.Store),
	}
	s.applyDeliveredOverride(ctx, snapshot, &deliveredInput)
	if err := s.email.SendOrderDelivered(ctx, deliveredInput); err != nil {
		s.logger.Warn("failed to send order delivered email",
			zap.String("cart_id", cartID),
			zap.String("to", customerEmail),
			zap.Error(err),
		)
		return
	}

	s.logger.Info("delivered flow completed",
		zap.String("cart_id", cartID),
		zap.String("source", source),
	)
}

// OnShipmentPosted fires after the merchant successfully creates a shipment
// at a carrier (Melhor Envio / SmartEnvios) or attaches a tracking_code via
// label generation. Records `shipped` in the timeline and emails the customer
// with the code. Best-effort.
//
// trackingCode is required — if empty (e.g. provider didn't return one yet),
// the call is a no-op.
func (s *Service) OnShipmentPosted(ctx context.Context, cartID, trackingCode string) {
	if trackingCode == "" {
		return
	}

	snapshot, err := s.repo.LoadCart(ctx, cartID)
	if err != nil {
		s.logger.Warn("post-checkout shipped hook could not load cart",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}

	// Idempotency: one shipped event per cart. Subsequent calls (label re-
	// generation, webhook retry) become no-ops without ever touching email.
	metadata := json.RawMessage(fmt.Sprintf(`{"tracking_code":%q}`, trackingCode))
	inserted, err := s.repo.InsertOrderEvent(ctx, cartID, "shipped", "merchant", metadata)
	if err != nil {
		s.logger.Warn("failed to record shipped event",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}
	if !inserted {
		// Already shipped — don't re-email.
		return
	}

	customerEmail := snapshot.Cart.CustomerEmail.String
	if customerEmail == "" {
		return
	}
	if !snapshot.Cart.TrackingToken.Valid || snapshot.Cart.TrackingToken.String == "" {
		// Defensive: shipped before paid would mean tracking_token wasn't
		// set. Skip the email; the order_event row remains as audit.
		return
	}

	input := buildShippedEmailInput(snapshot, trackingCode)
	s.applyShippedOverride(ctx, snapshot, &input, trackingCode)
	if err := s.email.SendOrderShipped(ctx, input); err != nil {
		s.logger.Warn("failed to send order shipped email",
			zap.String("cart_id", cartID),
			zap.String("to", customerEmail),
			zap.Error(err),
		)
		return
	}

	s.logger.Info("shipped flow completed",
		zap.String("cart_id", cartID),
		zap.String("to", customerEmail),
		zap.String("tracking_code", trackingCode),
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
		ReplyTo:        storeReplyTo(s.Store),
	}
}

// loadEmailSettings returns the merchant's notification.Settings, or nil when
// the feature isn't wired (e.g. tests). Errors are swallowed because every
// call site is in a best-effort flow.
func (s *Service) loadEmailSettings(ctx context.Context, storeID string) *notification.Settings {
	if s.notificationService == nil {
		return nil
	}
	settings, err := s.notificationService.GetSettings(ctx, storeID)
	if err != nil {
		s.logger.Debug("notification settings load failed",
			zap.String("store_id", storeID),
			zap.Error(err),
		)
		return nil
	}
	return settings
}

// vars assembles the substitution map that {variable} placeholders in
// merchant-customized templates expand against. Same set the IG DM cart
// templates use, plus the post-payment-only ones.
func vars(s *CartSnapshot, trackingCode, trackingURL string) notification.TemplateVariables {
	carrier := ""
	if s.Cart.ShippingServiceName.Valid && s.Cart.ShippingServiceName.String != "" {
		carrier = s.Cart.ShippingServiceName.String
		if s.Cart.ShippingCarrier.Valid && s.Cart.ShippingCarrier.String != "" {
			carrier = fmt.Sprintf("%s · %s", s.Cart.ShippingServiceName.String, s.Cart.ShippingCarrier.String)
		}
	}

	var totalCents int64
	for _, it := range s.Items {
		if !it.Quantity.Valid || !it.UnitPrice.Valid {
			continue
		}
		totalCents += int64(it.Quantity.Int32) * it.UnitPrice.Int64
	}
	if s.Cart.ShippingCostCents.Valid {
		totalCents += s.Cart.ShippingCostCents.Int64
	}
	totalCents -= s.Cart.CouponDiscountCents
	if totalCents < 0 {
		totalCents = 0
	}

	return notification.TemplateVariables{
		Handle:         s.Cart.PlatformHandle,
		Loja:           s.Store.Name,
		Total:          fmt.Sprintf("R$ %d,%02d", totalCents/100, totalCents%100),
		TotalCents:     totalCents,
		NumeroPedido:   fmt.Sprintf("%d", s.Cart.ShortID),
		TrackingCode:   trackingCode,
		Transportadora: carrier,
		LinkPedido:     trackingURL,
	}
}

func (s *Service) applyPaidOverride(ctx context.Context, snap *CartSnapshot, input *email.OrderPaidEmailInput) {
	settings := s.loadEmailSettings(ctx, snap.Store.ID.String())
	if settings == nil || settings.PaymentConfirmed == nil || !settings.PaymentConfirmed.Enabled {
		return
	}
	v := vars(snap, "", input.TrackingURL)
	if settings.PaymentConfirmed.Subject != "" {
		input.OverrideSubject = notification.RenderTemplate(settings.PaymentConfirmed.Subject, v)
	}
	if settings.PaymentConfirmed.BodyHTML != "" {
		input.OverrideBodyHTML = notification.RenderTemplate(settings.PaymentConfirmed.BodyHTML, v)
	}
}

func (s *Service) applyShippedOverride(ctx context.Context, snap *CartSnapshot, input *email.OrderShippedEmailInput, trackingCode string) {
	settings := s.loadEmailSettings(ctx, snap.Store.ID.String())
	if settings == nil || settings.Shipped == nil || !settings.Shipped.Enabled {
		return
	}
	v := vars(snap, trackingCode, input.TrackingURL)
	if settings.Shipped.Subject != "" {
		input.OverrideSubject = notification.RenderTemplate(settings.Shipped.Subject, v)
	}
	if settings.Shipped.BodyHTML != "" {
		input.OverrideBodyHTML = notification.RenderTemplate(settings.Shipped.BodyHTML, v)
	}
}

func (s *Service) applyDeliveredOverride(ctx context.Context, snap *CartSnapshot, input *email.OrderDeliveredEmailInput) {
	settings := s.loadEmailSettings(ctx, snap.Store.ID.String())
	if settings == nil || settings.Delivered == nil || !settings.Delivered.Enabled {
		return
	}
	v := vars(snap, "", "")
	if settings.Delivered.Subject != "" {
		input.OverrideSubject = notification.RenderTemplate(settings.Delivered.Subject, v)
	}
	if settings.Delivered.BodyHTML != "" {
		input.OverrideBodyHTML = notification.RenderTemplate(settings.Delivered.BodyHTML, v)
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

func buildShippedEmailInput(s *CartSnapshot, trackingCode string) email.OrderShippedEmailInput {
	customerName := s.Cart.CustomerName.String
	if customerName == "" {
		customerName = s.Cart.PlatformHandle
	}
	if customerName == "" {
		customerName = "cliente"
	}

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
		s.Cart.TrackingToken.String,
	)

	return email.OrderShippedEmailInput{
		StoreName:    s.Store.Name,
		StoreLogoURL: logoURL,
		ToEmail:      s.Cart.CustomerEmail.String,
		ToName:       customerName,
		OrderShortID: fmt.Sprintf("%d", s.Cart.ShortID),
		TrackingCode: trackingCode,
		CarrierLine:  carrierLine,
		TrackingURL:  trackingURL,
		ReplyTo:      storeReplyTo(s.Store),
	}
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


// itemLineTotalCents mirrors the GetCartTotals expression
// (SUM(quantity * unit_price)) used across notifications and dashboards, so
// the metered GMV matches the totals the merchant sees elsewhere.
func itemLineTotalCents(item sqlc.ListCartItemsRow) int64 {
	qty := int64(item.Quantity.Int32)
	if !item.Quantity.Valid {
		qty = 0
	}
	price := item.UnitPrice.Int64
	if !item.UnitPrice.Valid {
		price = 0
	}
	return qty * price
}

// storeReplyTo devolve o e-mail de contato da loja para o reply-to dos
// e-mails transacionais — respostas do cliente caem no lojista, não na
// caixa da plataforma.
func storeReplyTo(store sqlc.Store) string {
	if store.EmailAddress.Valid {
		return store.EmailAddress.String
	}
	return ""
}

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
