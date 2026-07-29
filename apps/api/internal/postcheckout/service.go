package postcheckout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	"livecart/apps/api/db/sqlc"
	"livecart/apps/api/internal/notification"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/email"
	"livecart/apps/api/lib/logger"
)

// ErrReceiptNotReady signals that the buyer receipt cannot be sent yet because
// the Order has not been materialised or its tracking token is not set. It is
// returned by SendPaidReceipt so the caller (the Notification reactor under
// asynq at-least-once) retries until the Order + token are ready — never
// sending a receipt whose tracking link would 404 on the public page.
var ErrReceiptNotReady = errors.New("post-checkout receipt not ready: order/token pending")

// EmailSender is the slice of lib/email.Client this package needs. Declared as
// an interface so the transactional-email dependency is injectable (a fake in
// tests, the real Resend client in production); *email.Client satisfies it.
type EmailSender interface {
	SendOrderPaid(ctx context.Context, input email.OrderPaidEmailInput) error
	SendOrderCancelled(ctx context.Context, input email.OrderCancelledEmailInput) error
	SendOrderRefunded(ctx context.Context, input email.OrderRefundedEmailInput) error
	SendOrderDelivered(ctx context.Context, input email.OrderDeliveredEmailInput) error
	SendOrderShipped(ctx context.Context, input email.OrderShippedEmailInput) error
}

// Service orchestrates the post-checkout customer flow: token generation +
// transactional emails. Reads merchant-customizable templates from
// notification.Service when configured and falls back to hardcoded defaults
// when the merchant hasn't overridden them.
type Service struct {
	repo                *Repository
	email               EmailSender
	notificationService NotificationSettingsReader
	logger              *zap.Logger
}

// NotificationSettingsReader is the slice of notification.Service this
// package needs. Defined locally to avoid an import cycle and to keep the
// dependency surface minimal.
type NotificationSettingsReader interface {
	GetSettings(ctx context.Context, storeID string) (*notification.Settings, error)
}

func NewService(repo *Repository, emailClient EmailSender, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		email:  emailClient,
		logger: logger.Named("postcheckout"),
	}
}

func (s *Service) SetNotificationService(reader NotificationSettingsReader) {
	s.notificationService = reader
}

// insertOrderEvent resolves the Order id for the cart (reusando
// GetOrderIDByCartID via repo.ResolveOrderID) e anexa um evento à timeline,
// keyed por (order_id, event_type). A resolução é best-effort: se falhar ou a
// Order ainda não existir (path síncrono do cartão), grava com order_id nulo —
// cart_id continua como âncora — em vez de perder o evento. Centraliza a
// resolução para os 5 call sites do post-checkout (paid/cancelled/refunded/
// shipped/delivered) num único ponto.
func (s *Service) insertOrderEvent(ctx context.Context, cartID, eventType, source string, metadata json.RawMessage) (bool, error) {
	orderID, err := s.repo.ResolveOrderID(ctx, cartID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("could not resolve order id for timeline event",
			zap.String("cart_id", cartID),
			zap.String("event_type", eventType),
			zap.Error(err),
		)
		orderID = pgtype.UUID{}
	}
	return s.repo.InsertOrderEvent(ctx, orderID, cartID, eventType, source, metadata)
}

// OnCartPaid is the hook called when a cart's payment_status flips to "paid".
// It is best-effort: every error is logged but never returned, because the
// caller is a payment webhook handler that must ACK regardless.
//
// Idempotency lives in two places: order_logistics.tracking_token (set once,
// never rotated — the source of truth since Fatia 10-a) and the order_events
// unique index. Either is enough to short circuit duplicate runs. Quando a Order
// ainda não foi materializada (path síncrono do cartão), o fluxo é adiado para o
// reactor async, que materializa a Order antes de re-rodar este hook.
func (s *Service) OnCartPaid(ctx context.Context, cartID string) {
	snapshot, err := s.repo.LoadCart(ctx, cartID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("post-checkout flow could not load cart",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}
	ctx = logger.WithStore(ctx, uuidStr(snapshot.Store.ID), snapshot.Store.Slug)

	// O token de rastreio vive só em order_logistics (fonte da verdade — Fatia
	// 10-a). Resolve o estado atual da Order deste cart para decidir o fluxo.
	existing, materialised, err := s.repo.GetOrderLogisticsTrackingToken(ctx, cartID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("post-checkout could not resolve order tracking token",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}

	// Path síncrono do cartão: a Order só é materializada pelo fato cart.paid
	// (emitido pelo webhook). Sem order_logistics não há onde persistir o token
	// de forma rastreável, então adiamos o fluxo inteiro para o reactor async —
	// que materializa a Order e DEPOIS roda este hook, na mesma task. Evita
	// mandar e-mail com um token que não resolveria na página pública.
	if !materialised {
		logger.From(ctx, s.logger).Debug("post-checkout deferred: order not materialised yet",
			zap.String("cart_id", cartID),
		)
		return
	}

	// Idempotência (replay de webhook / mark-as-paid manual): token já gravado na
	// Order → já processamos este cart.
	if existing != "" {
		logger.From(ctx, s.logger).Debug("post-checkout already ran for cart",
			zap.String("cart_id", cartID),
		)
		return
	}

	token, err := generateTrackingToken()
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to generate tracking token",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}

	// Grava o token na Order (order_logistics) — set-once no banco (WHERE
	// tracking_token IS NULL), então redeliveries concorrentes não o rotacionam.
	if err := s.repo.SetOrderLogisticsTrackingToken(ctx, cartID, token); err != nil {
		logger.From(ctx, s.logger).Error("failed to persist tracking token on order_logistics",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}

	// Append `payment_confirmed` to the customer-facing timeline. Insert is
	// idempotent at the DB level (unique order_id+event_type) — duplicate
	// hooks are a no-op.
	if _, err := s.insertOrderEvent(ctx, cartID, "payment_confirmed", "system", nil); err != nil {
		logger.From(ctx, s.logger).Warn("failed to record payment_confirmed event",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}

	// Marca como 'fulfilled' qualquer waitlist em status='notified' deste
	// cart — o cliente pagou dentro da janela. Itens em 'waiting' (que ele
	// optou por deixar na fila) permanecem.
	if err := s.repo.MarkWaitlistFulfilledByCart(ctx, cartID); err != nil {
		logger.From(ctx, s.logger).Warn("failed to mark notified waitlist as fulfilled",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}

	// O recibo do comprador NÃO é mais enviado aqui: virou um reactor do domínio
	// Notification (notification/listeners.OnCartPaid → SendPaidReceipt), que
	// reage ao fato cart.paid DEPOIS que esta Order + o tracking_token existem.
	// A materialização do token acima é justamente a garantia de ordenação que o
	// recibo consome.
	logger.From(ctx, s.logger).Info("post-checkout flow completed",
		zap.String("cart_id", cartID),
	)
}

// SendPaidReceipt sends the buyer's "pagamento confirmado" receipt for a paid
// cart. It is the Notification reactor's worker (invoked via ReceiptSender), NOT
// part of the OnCartPaid fan-out: the email left OnCartPaid so Notification can
// REACT to cart.paid on its own instead of being called inline.
//
// Ordering invariant (the whole reason this is separate): the receipt carries a
// public tracking link keyed by order_logistics.tracking_token, generated by
// OnCartPaid once the Order is materialised. If the Order/token are not ready
// yet (card sync path racing the async materialisation), it returns
// ErrReceiptNotReady so asynq retries — never mailing a link that would 404.
//
// Exactly-once: a `receipt_sent` timeline marker (unique per order_id+event_type)
// is claimed before sending, so an at-least-once redelivery is a no-op. The
// marker is internal — the public timeline filters it out.
func (s *Service) SendPaidReceipt(ctx context.Context, cartID string) error {
	snapshot, err := s.repo.LoadCart(ctx, cartID)
	if err != nil {
		// Cart vanished (hard-deleted test data, etc.) — nothing to mail, and no
		// retry would fix it. Best-effort: log and stop.
		logger.From(ctx, s.logger).Warn("receipt skipped: load cart failed",
			zap.String("cart_id", cartID), zap.Error(err))
		return nil
	}
	ctx = logger.WithStore(ctx, uuidStr(snapshot.Store.ID), snapshot.Store.Slug)

	// Resolve the Order + its tracking token (source of truth: order_logistics).
	token, materialised, err := s.repo.GetOrderLogisticsTrackingToken(ctx, cartID)
	if err != nil {
		return fmt.Errorf("receipt: resolve tracking token for cart %s: %w", cartID, err)
	}
	if !materialised || token == "" {
		// Order not materialised yet, or OnCartPaid hasn't set the token — retry
		// until it is (asynq re-enfileira). Guards the ordering invariant.
		return fmt.Errorf("receipt for cart %s: %w", cartID, ErrReceiptNotReady)
	}

	// Exactly-once: claim the `receipt_sent` marker up front (unique per
	// order_id+event_type). If it already exists, a prior delivery already mailed
	// the receipt → idempotent skip. Insert failures are retryable.
	inserted, err := s.insertOrderEvent(ctx, cartID, "receipt_sent", "system", nil)
	if err != nil {
		return fmt.Errorf("receipt: claim idempotency marker for cart %s: %w", cartID, err)
	}
	if !inserted {
		logger.From(ctx, s.logger).Debug("receipt already sent for cart, skipping",
			zap.String("cart_id", cartID))
		return nil
	}

	// No customer email captured (rare — checkout requires it, but this guards
	// data drift). The marker stays claimed: there is nothing to mail.
	customerEmail := snapshot.Cart.CustomerEmail.String
	if customerEmail == "" {
		logger.From(ctx, s.logger).Info("cart has no customer email, skipping receipt",
			zap.String("cart_id", cartID))
		return nil
	}

	input := buildEmailInput(snapshot, token)
	s.applyPaidOverride(ctx, snapshot, &input)
	if err := s.email.SendOrderPaid(ctx, input); err != nil {
		// Send is best-effort (readiness is the retryable condition, not Resend
		// hiccups) — mirrors the other transactional flows. The marker is already
		// claimed, so a retry won't double-mail.
		logger.From(ctx, s.logger).Warn("failed to send order paid email",
			zap.String("cart_id", cartID),
			zap.String("to", customerEmail),
			zap.Error(err),
		)
		return nil
	}

	logger.From(ctx, s.logger).Info("receipt sent",
		zap.String("cart_id", cartID),
		zap.String("to", customerEmail),
	)
	return nil
}

// OnCartCancelled sends the "pedido cancelado" email. Exactly-once via the
// timeline insert (unique order_id+event_type) — webhook retries are no-ops.
func (s *Service) OnCartCancelled(ctx context.Context, cartID string) {
	snapshot, err := s.repo.LoadCart(ctx, cartID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("cancellation email skipped: load cart failed",
			zap.String("cart_id", cartID), zap.Error(err))
		return
	}
	ctx = logger.WithStore(ctx, uuidStr(snapshot.Store.ID), snapshot.Store.Slug)
	if !snapshot.Cart.CustomerEmail.Valid || snapshot.Cart.CustomerEmail.String == "" {
		logger.From(ctx, s.logger).Warn("cancellation email skipped: cart has no customer email",
			zap.String("cart_id", cartID))
		return
	}
	inserted, err := s.insertOrderEvent(ctx, cartID, "payment_cancelled", "system", nil)
	if err != nil {
		logger.From(ctx, s.logger).Error("cancellation email skipped: insert timeline event failed",
			zap.String("cart_id", cartID), zap.Error(err))
		return
	}
	if !inserted {
		logger.From(ctx, s.logger).Info("cancellation email skipped: timeline event already exists (idempotent)",
			zap.String("cart_id", cartID))
		return
	}

	input := email.OrderCancelledEmailInput{
		StoreName:      snapshot.Store.Name,
		StoreLogoURL:   storeLogo(snapshot.Store),
		ToEmail:        snapshot.Cart.CustomerEmail.String,
		ToName:         customerDisplayName(snapshot),
		OrderShortID:   fmt.Sprintf("%d", snapshot.Cart.ShortID),
		TotalFormatted: formatTotal(snapshot),
		// cancelamento chega quando a cobrança não completou; estorno de
		// pagamento concluído entra pelo OnCartRefunded
		WasCharged: false,
		ReplyTo:    storeReplyTo(snapshot.Store),
		StoreID:    uuidStr(snapshot.Store.ID),
		CartID:     uuidStr(snapshot.Cart.ID),
		EventID:    uuidStr(snapshot.Cart.EventID),
	}
	s.applyEmailOverride(ctx, snapshot, s.settingsFor(ctx, snapshot, "cancelled"), &input.OverrideSubject, &input.OverrideBodyHTML, "", "")

	if err := s.email.SendOrderCancelled(ctx, input); err != nil {
		logger.From(ctx, s.logger).Warn("failed to send order cancelled email",
			zap.String("cart_id", cartID), zap.Error(err))
	}
}

// SendRefundEmail sends the buyer's "pedido estornado" email. Like
// SendPaidReceipt it is the Notification reactor's worker (invoked via
// ReceiptSender on cart.refunded), NOT part of the ReactCartRefunded fan-out —
// the send left that fan-out so Notification reacts to the fact on its own.
//
// Exactly-once via the `payment_refunded` timeline marker (unique per
// order_id+event_type): an at-least-once redelivery is a no-op. There is no
// readiness dependency (the refund email carries no tracking token), so it never
// returns ErrReceiptNotReady; the send itself is best-effort.
func (s *Service) SendRefundEmail(ctx context.Context, cartID string) error {
	snapshot, err := s.repo.LoadCart(ctx, cartID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("refund email skipped: load cart failed",
			zap.String("cart_id", cartID), zap.Error(err))
		return nil
	}
	ctx = logger.WithStore(ctx, uuidStr(snapshot.Store.ID), snapshot.Store.Slug)
	if !snapshot.Cart.CustomerEmail.Valid || snapshot.Cart.CustomerEmail.String == "" {
		logger.From(ctx, s.logger).Warn("refund email skipped: cart has no customer email",
			zap.String("cart_id", cartID))
		return nil
	}
	inserted, err := s.insertOrderEvent(ctx, cartID, "payment_refunded", "system", nil)
	if err != nil {
		return fmt.Errorf("refund email: insert timeline event for cart %s: %w", cartID, err)
	}
	if !inserted {
		logger.From(ctx, s.logger).Info("refund email skipped: timeline event already exists (idempotent)",
			zap.String("cart_id", cartID))
		return nil
	}

	method := ""
	if snapshot.Cart.PaymentMethod.Valid {
		method = snapshot.Cart.PaymentMethod.String
	}
	input := email.OrderRefundedEmailInput{
		StoreName:      snapshot.Store.Name,
		StoreLogoURL:   storeLogo(snapshot.Store),
		ToEmail:        snapshot.Cart.CustomerEmail.String,
		ToName:         customerDisplayName(snapshot),
		OrderShortID:   fmt.Sprintf("%d", snapshot.Cart.ShortID),
		TotalFormatted: formatTotal(snapshot),
		PaymentMethod:  method,
		ReplyTo:        storeReplyTo(snapshot.Store),
		StoreID:        uuidStr(snapshot.Store.ID),
		CartID:         uuidStr(snapshot.Cart.ID),
		EventID:        uuidStr(snapshot.Cart.EventID),
	}
	s.applyEmailOverride(ctx, snapshot, s.settingsFor(ctx, snapshot, "refunded"), &input.OverrideSubject, &input.OverrideBodyHTML, "", "")

	if err := s.email.SendOrderRefunded(ctx, input); err != nil {
		// Best-effort: the marker is already claimed, so a retry won't double-mail.
		logger.From(ctx, s.logger).Warn("failed to send order refunded email",
			zap.String("cart_id", cartID), zap.Error(err))
	}
	return nil
}

// settingsFor resolve o override configurado pro tipo de e-mail.
func (s *Service) settingsFor(ctx context.Context, snap *CartSnapshot, kind string) *notification.EmailTemplateSettings {
	settings := s.loadEmailSettings(ctx, snap.Store.ID.String())
	if settings == nil {
		return nil
	}
	switch kind {
	case "cancelled":
		return settings.PaymentCancelled
	case "refunded":
		return settings.PaymentRefunded
	}
	return nil
}

// applyEmailOverride renderiza subject/body customizados quando habilitados.
func (s *Service) applyEmailOverride(_ context.Context, snap *CartSnapshot, tpl *notification.EmailTemplateSettings, subject, body *string, trackingCode, trackingURL string) {
	if tpl == nil || !tpl.Enabled {
		return
	}
	v := vars(snap, trackingCode, trackingURL)
	if tpl.Subject != "" {
		*subject = notification.RenderTemplate(tpl.Subject, v)
	}
	if tpl.BodyHTML != "" {
		*body = notification.RenderTemplate(tpl.BodyHTML, v)
	}
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
		logger.From(ctx, s.logger).Warn("post-checkout delivered hook could not load cart",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}
	ctx = logger.WithStore(ctx, uuidStr(snapshot.Store.ID), snapshot.Store.Slug)

	metadata := json.RawMessage(fmt.Sprintf(`{"source":%q}`, source))
	inserted, err := s.insertOrderEvent(ctx, cartID, "delivered", source, metadata)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to record delivered event",
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
		logger.From(ctx, s.logger).Info("delivery confirmed by customer, skipping email",
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
		StoreID:      uuidStr(snapshot.Store.ID),
		CartID:       uuidStr(snapshot.Cart.ID),
		EventID:      uuidStr(snapshot.Cart.EventID),
	}
	s.applyDeliveredOverride(ctx, snapshot, &deliveredInput)
	if err := s.email.SendOrderDelivered(ctx, deliveredInput); err != nil {
		logger.From(ctx, s.logger).Warn("failed to send order delivered email",
			zap.String("cart_id", cartID),
			zap.String("to", customerEmail),
			zap.Error(err),
		)
		return
	}

	logger.From(ctx, s.logger).Info("delivered flow completed",
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
		logger.From(ctx, s.logger).Warn("post-checkout shipped hook could not load cart",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
		return
	}
	ctx = logger.WithStore(ctx, uuidStr(snapshot.Store.ID), snapshot.Store.Slug)

	// Idempotency: one shipped event per cart. Subsequent calls (label re-
	// generation, webhook retry) become no-ops without ever touching email.
	metadata := json.RawMessage(fmt.Sprintf(`{"tracking_code":%q}`, trackingCode))
	inserted, err := s.insertOrderEvent(ctx, cartID, "shipped", "merchant", metadata)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to record shipped event",
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
	// O link de rastreio do e-mail usa o token da fonte da verdade (order_logistics).
	trackingToken, materialised, err := s.repo.GetOrderLogisticsTrackingToken(ctx, cartID)
	if err != nil || !materialised || trackingToken == "" {
		// Defensive: shipped before paid would mean the tracking token wasn't
		// generated (Order not materialised / paid hook não rodou). Skip the
		// email; the order_event row remains as audit.
		return
	}

	input := buildShippedEmailInput(snapshot, trackingCode, trackingToken)
	s.applyShippedOverride(ctx, snapshot, &input, trackingCode)
	if err := s.email.SendOrderShipped(ctx, input); err != nil {
		logger.From(ctx, s.logger).Warn("failed to send order shipped email",
			zap.String("cart_id", cartID),
			zap.String("to", customerEmail),
			zap.Error(err),
		)
		return
	}

	logger.From(ctx, s.logger).Info("shipped flow completed",
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
		StoreID:        uuidStr(s.Store.ID),
		CartID:         uuidStr(s.Cart.ID),
		EventID:        uuidStr(s.Cart.EventID),
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
		logger.From(ctx, s.logger).Debug("notification settings load failed",
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

	formaPagamento := ""
	if s.Cart.PaymentMethod.Valid {
		switch s.Cart.PaymentMethod.String {
		case "pix":
			formaPagamento = "PIX"
		case "credit_card", "card":
			formaPagamento = "Cartão"
		default:
			formaPagamento = s.Cart.PaymentMethod.String
		}
	}

	nomeCliente := s.Cart.PlatformHandle
	if s.Cart.CustomerName.Valid && s.Cart.CustomerName.String != "" {
		nomeCliente = s.Cart.CustomerName.String
	}

	prazo := ""
	if s.Cart.ShippingDeadlineDays.Valid && s.Cart.ShippingDeadlineDays.Int32 > 0 {
		prazo = fmt.Sprintf("até %d dias úteis", s.Cart.ShippingDeadlineDays.Int32)
	}

	valorFrete := ""
	if s.Cart.ShippingCostCents.Valid {
		valorFrete = fmt.Sprintf("R$ %d,%02d", s.Cart.ShippingCostCents.Int64/100, s.Cart.ShippingCostCents.Int64%100)
	}

	// Tabela completa pros overrides de e-mail (corpo é HTML). Envelopa as
	// <tr> do renderItemsHTML numa <table> própria — dentro de um override o
	// lojista não tem a tabela do shell default.
	listaProdutos := ""
	if len(s.Items) > 0 {
		listaProdutos = `<table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="border-collapse:collapse;">` +
			renderItemsHTML(s.Items) + `</table>`
	}

	return notification.TemplateVariables{
		Handle:          s.Cart.PlatformHandle,
		FormaPagamento:  formaPagamento,
		NomeCliente:     nomeCliente,
		ListaProdutos:   listaProdutos,
		EnderecoEntrega: formatShippingLine(ParseShippingAddress(s.Cart.ShippingAddress)),
		PrazoEntrega:    prazo,
		ValorFrete:      valorFrete,
		Loja:            s.Store.Name,
		Total:           fmt.Sprintf("R$ %d,%02d", totalCents/100, totalCents%100),
		TotalCents:      totalCents,
		NumeroPedido:    fmt.Sprintf("%d", s.Cart.ShortID),
		TrackingCode:    trackingCode,
		Transportadora:  carrier,
		LinkPedido:      trackingURL,
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

func buildShippedEmailInput(s *CartSnapshot, trackingCode, trackingToken string) email.OrderShippedEmailInput {
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
		trackingToken,
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
		StoreID:      uuidStr(s.Store.ID),
		CartID:       uuidStr(s.Cart.ID),
		EventID:      uuidStr(s.Cart.EventID),
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

// storeReplyTo devolve o e-mail de contato da loja para o reply-to dos
// e-mails transacionais — respostas do cliente caem no lojista, não na
// caixa da plataforma.
func storeReplyTo(store sqlc.Store) string {
	if store.EmailAddress.Valid {
		return store.EmailAddress.String
	}
	return ""
}

func storeLogo(store sqlc.Store) string {
	if store.LogoUrl.Valid {
		return store.LogoUrl.String
	}
	return ""
}

func customerDisplayName(snap *CartSnapshot) string {
	if snap.Cart.CustomerName.Valid && snap.Cart.CustomerName.String != "" {
		return snap.Cart.CustomerName.String
	}
	return "cliente"
}

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
