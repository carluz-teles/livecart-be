package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/internal/payment"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/logger"
)

// CouponLifecycle is the cart-mutation hook the coupon package implements
// to keep its discount snapshot in sync with the rest of the cart. We define
// the interface here so the checkout package compiles without a coupon
// import; the concrete implementation is wired at boot via SetCouponLifecycle.
type CouponLifecycle interface {
	// OnShippingChanged is invoked after a successful UpdateCartShipping so
	// a free-shipping coupon's discount can be re-snapshotted against the
	// new shipping_cost_cents. Implementation must be a no-op for percent /
	// fixed coupons (their snapshot is stable across shipping changes).
	OnShippingChanged(ctx context.Context, cartID string) error

	// OnCartMutated is invoked after every successful cart-item edit
	// (add / quantity update / remove) so the coupon snapshot stays in sync
	// with the new subtotal. The implementation must:
	//   - auto-remove the coupon when the new subtotal falls below the
	//     coupon's min_purchase_cents (slot returns to circulation), and
	//   - re-snapshot coupon_discount_cents against the new subtotal for
	//     percent / fixed coupons.
	// No-op when the cart has no coupon attached.
	OnCartMutated(ctx context.Context, cartID string) error
}

// PostCheckoutHook mirrors integration.PostCheckoutHook so the synchronous
// card path in this service can fire the same customer-facing post-payment
// flow as the asynchronous webhook path.
//
// OnCartPaid saiu daqui (Fatia A4): o tracking_token e a timeline
// `payment_confirmed` nascem na materialização da Order (order/listeners.OnCartPaid),
// disparada pelo fato cart.paid do webhook — não mais inline no path do cartão.
type PostCheckoutHook interface {
	OnShipmentPosted(ctx context.Context, cartID, trackingCode string)
	OnDelivered(ctx context.Context, cartID, source string)
}

// PaymentService is the slice of payment.Service the checkout flow calls: the
// stateless payment fast-paths (hosted checkout, transparent card/PIX, and the
// per-integration checkout config). Declared here — in the consumer package —
// and implemented by *payment.Service, so checkout depends on the payment
// domain through a narrow interface instead of the integration monolith
// (strangler-fig B1e). The non-payment integration calls (waitlist, stock
// reservation, ERP prewarm) still go through integrationService.
type PaymentService interface {
	CreateCheckout(ctx context.Context, input payment.CreateCheckoutInput) (*payment.CreateCheckoutOutput, error)
	GetCheckoutConfig(ctx context.Context, integrationID, storeID string) (publicKey string, methods []string, err error)
	ProcessCardPayment(ctx context.Context, input payment.ProcessCardPaymentInput) (*payment.ProcessCardPaymentOutput, error)
	GeneratePixPayment(ctx context.Context, input payment.GeneratePixPaymentInput) (*payment.GeneratePixPaymentOutput, error)
	// CancelPixPayment invalida no gateway uma cobranca PIX ainda nao paga.
	CancelPixPayment(ctx context.Context, integrationID, storeID, cancelID string) error
}

// Service handles business logic for public checkout.
type Service struct {
	repo               *Repository
	pool               *pgxpool.Pool
	integrationService *integration.Service
	paymentService     PaymentService
	couponLifecycle    CouponLifecycle
	postCheckoutHook   PostCheckoutHook
	logger             *zap.Logger
}

// NewService creates a new checkout service.
func NewService(
	repo *Repository,
	pool *pgxpool.Pool,
	integrationService *integration.Service,
	paymentService PaymentService,
	logger *zap.Logger,
) *Service {
	return &Service{
		repo:               repo,
		pool:               pool,
		integrationService: integrationService,
		paymentService:     paymentService,
		logger:             logger.Named("checkout"),
	}
}

// SetCouponLifecycle wires the redemption hook from the coupon package.
// Optional — when unset, shipping changes don't re-evaluate coupons (the
// FE will see a stale free-shipping discount until the cart is mutated
// some other way).
func (s *Service) SetCouponLifecycle(lifecycle CouponLifecycle) {
	s.couponLifecycle = lifecycle
}

// SetPostCheckoutHook wires the customer-facing post-payment flow.
func (s *Service) SetPostCheckoutHook(hook PostCheckoutHook) {
	s.postCheckoutHook = hook
}

// fireCheckoutInitHooks fires the payment-initiation side-effects in a
// background goroutine so the checkout call never blocks on them.
// PrepareCartForPayment (ERP Design C) runs for stores with the flag enabled.
func (s *Service) fireCheckoutInitHooks(cartID, storeID, storeSlug string) {
	bgCtx := logger.WithStore(context.Background(), storeID, storeSlug)
	go func() {
		s.integrationService.PrepareCartForPayment(bgCtx, cartID, storeID)
	}()
}

// GetCartForCheckout retrieves a cart for the public checkout page.
func (s *Service) GetCartForCheckout(ctx context.Context, input GetCartForCheckoutInput) (*GetCartForCheckoutOutput, error) {
	// Get cart with event/store info
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}
	// Rota pública (sem middleware de store): o store é resolvido pelo token.
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)

	// Validate cart status (allow viewing paid carts so frontend can show "paid" message)
	if cart.Status == "expired" {
		return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
	}
	// Prazo de pagamento vencido (armado ao encerrar a live). Só bloqueia
	// carts não pagos — quem pagou dentro do prazo continua vendo o pedido
	// mesmo depois que o expires_at passa.
	if cart.PaymentStatus != "paid" && cart.ExpiresAt != nil && cart.ExpiresAt.Before(time.Now()) {
		return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
	}
	// Carts cancelled because the buyer was blocked should look like they
	// never existed — return 404 so the public /cart/[token] page hits the
	// generic "carrinho não existe" branch.
	if cart.Status == "cancelled" && cart.CancelledReason == "customer_blocked" {
		return nil, httpx.ErrNotFound("carrinho não encontrado")
	}
	// Cancelado pela loja: nada do carrinho é devolvido (nem itens, nem valores)
	// — o comprador vê só a mensagem. Ela diz CANCELADO, e não "expirado", que é
	// justamente a confusão que essa distinção existe para evitar: ninguém
	// perdeu prazo nenhum aqui.
	if cart.Status == "cancelled" {
		return nil, httpx.ErrUnprocessable("Este carrinho foi cancelado pela loja. Fale com a loja para receber um novo link.")
	}
	// Note: paid carts are allowed - frontend will show a "paid" dialog

	// Freeze the initial cart on the very first GET (idempotent — subsequent
	// calls are no-ops). Failure here is non-fatal: the buyer must still be
	// able to pay even if snapshotting blew up; the upsell card just won't
	// have a baseline to compare against.
	if cart.PaymentStatus != "paid" {
		if err := s.repo.EnsureInitialSnapshot(ctx, cart.ID); err != nil {
			logger.From(ctx, s.logger).Warn("failed to snapshot initial cart",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		}
	}

	// Get cart items
	items, err := s.repo.ListCartItems(ctx, cart.ID)
	if err != nil {
		return nil, err
	}

	// Enrich variant items with their group name + chosen options so the
	// checkout renders a short title + "Cor: Preto · Tam: M" rather than a giant
	// product name. Best-effort — failure leaves items unchanged.
	productIDs := make([]string, 0, len(items))
	for _, it := range items {
		productIDs = append(productIDs, it.ProductID)
	}
	if vinfo, vErr := s.repo.LoadVariantInfo(ctx, s.pool, productIDs); vErr != nil {
		logger.From(ctx, s.logger).Warn("failed to load variant info for checkout", zap.Error(vErr))
	} else {
		for i := range items {
			if vi, ok := vinfo[items[i].ProductID]; ok {
				items[i].GroupName = vi.GroupName
				items[i].Variant = vi.Variant
			}
		}
	}

	// Load shipping selection (may be nil if not chosen yet)
	shippingSel, err := s.repo.ReadCartShipping(ctx, s.pool, cart.ID)
	if err != nil {
		return nil, err
	}

	// Hydrate the applied coupon's type, merchant cap and min-purchase so
	// the FE can render contextual copy (partial free-shipping explanation,
	// auto-removal toast referring to the threshold). Best-effort: a missing
	// / hard-deleted coupon row leaves the fields at zero (treated by the
	// FE as "no extra context to show").
	if cart.CouponID != nil {
		summary, cerr := s.repo.ReadCouponSummary(ctx, s.pool, *cart.CouponID)
		if cerr == nil {
			cart.CouponType = summary.Type
			cart.CouponMaxDiscountCents = summary.MaxDiscountCents
			cart.CouponMinPurchaseCents = summary.MinPurchaseCents
		}
	}

	// Hydrate the event-level checkout flags (free_shipping + pix_discount_percent).
	// These columns are not yet wired through sqlc, so we read them via raw SQL.
	if freeShipping, pixPct, err := s.repo.LoadEventCheckoutFlags(ctx, s.pool, cart.EventID); err == nil {
		cart.EventFreeShipping = freeShipping
		cart.EventPixDiscountPercent = pixPct
	} else {
		logger.From(ctx, s.logger).Warn("failed to load event checkout flags",
			zap.String("event_id", cart.EventID),
			zap.Error(err),
		)
	}
	if eventType, err := s.repo.LoadEventType(ctx, s.pool, cart.EventID); err == nil {
		cart.EventType = eventType
	}

	// Convert to output
	output := &GetCartForCheckoutOutput{
		Cart: CartDetails{
			ID:                      cart.ID,
			EventID:                 cart.EventID,
			PlatformUserID:          cart.PlatformUserID,
			PlatformHandle:          cart.PlatformHandle,
			Token:                   cart.Token,
			Status:                  cart.Status,
			CheckoutURL:             cart.CheckoutURL,
			CheckoutID:              cart.CheckoutID,
			CustomerEmail:           cart.CustomerEmail,
			PaymentStatus:           cart.PaymentStatus,
			PaidAt:                  cart.PaidAt,
			CreatedAt:               cart.CreatedAt,
			ExpiresAt:               cart.ExpiresAt,
			EventTitle:              cart.EventTitle,
			EventType:               cart.EventType,
			EventFreeShipping:       cart.EventFreeShipping,
			EventPixDiscountPercent: cart.EventPixDiscountPercent,
			StoreID:                 cart.StoreID,
			StoreName:               cart.StoreName,
			StoreLogoURL:            cart.StoreLogoURL,
			AllowEdit:               cart.AllowEdit,
			MaxQuantityPerItem:      cart.MaxQuantityPerItem,
			Shipping:                shippingSel,
			CouponID:                cart.CouponID,
			CouponCode:              cart.CouponCode,
			CouponDiscountCents:     cart.CouponDiscountCents,
			CouponType:              cart.CouponType,
			CouponMaxDiscountCents:  cart.CouponMaxDiscountCents,
			CouponMinPurchaseCents:  cart.CouponMinPurchaseCents,
		},
		Items: make([]CartItemDetails, len(items)),
	}

	for i, item := range items {
		output.Items[i] = CartItemDetails{
			ID:                 item.ID,
			CartID:             item.CartID,
			ProductID:          item.ProductID,
			Quantity:           item.Quantity,
			UnitPrice:          item.UnitPrice,
			WaitlistedQuantity: item.WaitlistedQuantity,
			Name:               item.Name,
			ImageURL:           item.ImageURL,
			Keyword:            item.Keyword,
			AvailableStock:     item.AvailableStock,
			GroupName:          item.GroupName,
			Variant:            item.Variant,
		}
	}

	// Lazy expiration: antes de listar a waitlist, varre 'notified' com
	// expires_at vencido e devolve o estoque para o próximo da fila.
	// Substitui um worker dedicado — a fila só precisa "andar" quando
	// alguém está olhando para o checkout. Best-effort: erro aqui só vira
	// log, a leitura segue mesmo com o sweep meio rodado.
	if processed, err := s.integrationService.ExpireNotifiedWaitlistSweep(ctx); err != nil {
		logger.From(ctx, s.logger).Warn("inline waitlist expiration sweep failed",
			zap.String("cart_id", cart.ID),
			zap.Int("processed_before_err", processed),
			zap.Error(err),
		)
	}

	// Hidrata a fila de espera vinculada ao cart (waiting + notified). É
	// tolerante a falha — só faz log e devolve a lista vazia se algo der
	// errado, porque a UI degrada graciosamente sem essa seção.
	if waitlist, wlErr := s.integrationService.ListActiveWaitlistByCart(ctx, cart.ID); wlErr != nil {
		logger.From(ctx, s.logger).Warn("failed to load waitlist for checkout",
			zap.String("cart_id", cart.ID),
			zap.Error(wlErr),
		)
	} else {
		output.WaitlistItems = make([]WaitlistItemDetails, 0, len(waitlist))
		for _, w := range waitlist {
			img := w.ProductImageURL
			var imgPtr *string
			if img != "" {
				imgPtr = &img
			}
			output.WaitlistItems = append(output.WaitlistItems, WaitlistItemDetails{
				ID:           w.ID,
				ProductID:    w.ProductID,
				ProductName:  w.ProductName,
				ProductImage: imgPtr,
				UnitPrice:    w.ProductPrice,
				Quantity:     w.Quantity,
				Position:     w.Position,
				Status:       w.Status,
				NotifiedAt:   w.NotifiedAt,
				ExpiresAt:    w.ExpiresAt,
				CreatedAt:    w.CreatedAt,
			})
		}
	}

	// Populate receipt fields (customer + shipping address + payment) only for
	// paid carts. The public checkout token is reachable by anyone — exposing
	// these on unpaid carts would leak the buyer's PII while payment is still
	// pending. Errors here are non-fatal; the receipt page just renders
	// whatever it received.
	if cart.PaymentStatus == "paid" {
		customer, address, payment, err := s.repo.ReadCartPaymentDetails(ctx, s.pool, cart.ID)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to read paid-cart receipt details",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		} else {
			output.Customer = customer
			output.ShippingAddress = address
			output.Payment = payment
		}
	} else if cart.PlatformUserID != "" {
		// Returning-buyer prefill: the same Instagram user already has a paid
		// cart on this store, so reuse that snapshot to populate name / CPF /
		// phone / address. Same trust boundary as the existing email prefill —
		// the cart token is unguessable and only ever delivered to the buyer.
		customer, address, err := s.repo.GetLatestPaidCustomerSnapshot(ctx, s.pool, cart.StoreID, cart.PlatformUserID, cart.ID)
		if err != nil {
			logger.From(ctx, s.logger).Warn("failed to read returning-buyer snapshot",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		} else if customer != nil || address != nil {
			output.Customer = customer
			output.ShippingAddress = address
			output.Cart.IsReturningCustomer = true

			// Cliente recorrente com dados conhecidos: pré-aquece o contato no
			// Tiny em background para a conversão na iniciação do pagamento
			// encontrar o cache quente (design C). Best-effort.
			if customer != nil {
				go s.integrationService.PrewarmERPContact(
					logger.WithStore(context.Background(), cart.StoreID, cart.StoreSlug),
					cart.StoreID, cart.PlatformUserID, cart.PlatformHandle,
					customer.Name, customer.Document, customer.Email, customer.Phone)
			}
		}
	}

	return output, nil
}

// GenerateCheckout creates a payment link for the cart.
func (s *Service) GenerateCheckout(ctx context.Context, input GenerateCheckoutInput) (*GenerateCheckoutOutput, error) {
	// Get cart
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)

	// Validate cart status
	if cart.Status == "expired" {
		return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.DomainError(422, httpx.CodeCartAlreadyPaid, "carrinho já foi pago")
	}
	// Allow checkout for both 'active' (live ongoing) and 'checkout' (live ended) status
	if cart.Status != "checkout" && cart.Status != "active" {
		return nil, httpx.DomainError(422, httpx.CodeCartNotPayable, "carrinho não está disponível para checkout")
	}

	// Update customer email
	if err := s.repo.UpdateCustomerEmail(ctx, input.Token, input.Email); err != nil {
		return nil, err
	}

	// Iniciação de pagamento via link hospedado: mesmo gancho de conversão do
	// pedido-como-reserva (design C, flag por loja) + draft da Order.
	s.fireCheckoutInitHooks(cart.ID, cart.StoreID, cart.StoreSlug)

	// Get cart items
	items, err := s.repo.ListCartItems(ctx, cart.ID)
	if err != nil {
		return nil, err
	}

	// Filter out fully waitlisted items and build checkout items with available quantities
	var checkoutItems []providers.CheckoutItem
	var totalAmount int64
	for _, item := range items {
		// Calculate available quantity (total - waitlisted)
		availableQty := item.Quantity - item.WaitlistedQuantity
		if availableQty <= 0 {
			continue // Skip items that are fully waitlisted
		}
		checkoutItems = append(checkoutItems, providers.CheckoutItem{
			ID:        item.ProductID,
			Name:      item.Name,
			Quantity:  availableQty, // Only the available quantity
			UnitPrice: item.UnitPrice,
			ImageURL:  derefString(item.ImageURL),
		})
		totalAmount += item.UnitPrice * int64(availableQty)
	}

	if len(checkoutItems) == 0 {
		return nil, httpx.DomainError(422, httpx.CodeCartNoItemsPayable, "carrinho não tem itens disponíveis para pagamento")
	}

	// Add selected shipping cost to the total charged at the gateway.
	if shippingSel, _ := s.repo.ReadCartShipping(ctx, s.pool, cart.ID); shippingSel != nil {
		totalAmount += shippingSel.CostCents
	}

	// Apply coupon discount snapshot. The coupon module captured a stable
	// discount-in-cents at apply time; we subtract here, capped at zero so
	// a percent-on-edge or stale free-shipping snapshot can never produce
	// a negative transaction_amount.
	totalAmount = applyCouponDiscount(totalAmount, cart.CouponDiscountCents)

	// Get payment integration — honors cart binding from GetCheckoutConfig.
	paymentIntegration, err := s.resolvePaymentIntegration(ctx, cart)
	if err != nil {
		return nil, err
	}

	// Get cart expiration minutes from store settings
	expirationMinutes, _ := s.repo.GetStoreCartExpirationMinutes(ctx, cart.StoreID)
	expiresAt := GetExpiresAtMinutes(expirationMinutes)

	// Build URLs
	baseURL := config.WebhookBaseURL.String()
	successURL := fmt.Sprintf("%s/cart/%s?status=success", baseURL, cart.Token)
	failureURL := fmt.Sprintf("%s/cart/%s?status=failure", baseURL, cart.Token)

	// Create checkout via integration service
	checkoutResult, err := s.paymentService.CreateCheckout(ctx, payment.CreateCheckoutInput{
		IntegrationID:  paymentIntegration.ID.String(),
		StoreID:        cart.StoreID,
		CartID:         cart.ID,
		IdempotencyKey: fmt.Sprintf("checkout-%s-%d", cart.ID, totalAmount),
		Items:          checkoutItems,
		Customer: providers.CheckoutCustomer{
			Email: input.Email,
			Name:  cart.PlatformHandle,
		},
		TotalAmount: totalAmount,
		Currency:    "BRL",
		SuccessURL:  successURL,
		FailureURL:  failureURL,
		Metadata: map[string]any{
			"cart_id":    cart.ID,
			"cart_token": cart.Token,
			"event_id":   cart.EventID,
			"store_id":   cart.StoreID,
		},
	})
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to create checkout",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
		return nil, httpx.DomainError(422, httpx.CodePaymentLinkFailed, "erro ao gerar link de pagamento. Tente novamente.")
	}

	// Update cart with checkout info
	if err := s.repo.UpdateCheckoutInfo(ctx, UpdateCheckoutParams{
		CartID:            cart.ID,
		CheckoutURL:       checkoutResult.CheckoutURL,
		CheckoutID:        checkoutResult.CheckoutID,
		CheckoutExpiresAt: expiresAt,
	}); err != nil {
		logger.From(ctx, s.logger).Error("failed to update checkout info",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
		// Don't fail - we already have the checkout URL
	}

	logger.From(ctx, s.logger).Info("checkout generated",
		zap.String("cart_id", cart.ID),
		zap.String("checkout_id", checkoutResult.CheckoutID),
		zap.Int64("total_amount", totalAmount),
	)

	return &GenerateCheckoutOutput{
		CheckoutURL: checkoutResult.CheckoutURL,
		ExpiresAt:   expiresAt,
	}, nil
}

// jsonOrEmpty marshals v, falling back to an empty JSON object on error so a
// best-effort emit never carries a nil payload.
func jsonOrEmpty(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// applyCouponDiscount subtracts the persisted coupon discount from the
// total being sent to the gateway, capped at zero. The discount snapshot
// lives on the cart (carts.coupon_discount_cents) and was computed by the
// coupon service under a row lock; we trust it here without re-evaluating.
func applyCouponDiscount(totalAmount, discountCents int64) int64 {
	if discountCents <= 0 {
		return totalAmount
	}
	out := totalAmount - discountCents
	if out < 0 {
		return 0
	}
	return out
}

// toProviderAddress maps the buyer's shipping address into the provider-
// agnostic CheckoutAddress shape. Returns nil when no address was captured
// (typical of Checkout Pro flows that hand the form off to the gateway).
func toProviderAddress(addr *ShippingAddress) *providers.CheckoutAddress {
	if addr == nil {
		return nil
	}
	return &providers.CheckoutAddress{
		ZipCode:      addr.ZipCode,
		Street:       addr.Street,
		Number:       addr.Number,
		Complement:   addr.Complement,
		Neighborhood: addr.Neighborhood,
		City:         addr.City,
		State:        addr.State,
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// =============================================================================
// TRANSPARENT CHECKOUT METHODS
// =============================================================================

// GetCheckoutConfig retrieves the checkout configuration for the frontend.
func (s *Service) GetCheckoutConfig(ctx context.Context, input GetCheckoutConfigInput) (*GetCheckoutConfigOutput, error) {
	// Get cart
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)

	// Validate cart status
	if cart.Status == "expired" {
		return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.DomainError(422, httpx.CodeCartAlreadyPaid, "carrinho já foi pago")
	}
	// Allow checkout for both 'active' (live ongoing) and 'checkout' (live ended) status
	if cart.Status != "checkout" && cart.Status != "active" {
		return nil, httpx.DomainError(422, httpx.CodeCartNotPayable, "carrinho não está disponível para checkout")
	}

	// Get cart items to calculate total
	items, err := s.repo.ListCartItems(ctx, cart.ID)
	if err != nil {
		return nil, err
	}

	var totalAmount int64
	for _, item := range items {
		// Calculate available quantity (total - waitlisted)
		availableQty := item.Quantity - item.WaitlistedQuantity
		if availableQty > 0 {
			totalAmount += item.UnitPrice * int64(availableQty)
		}
	}

	if totalAmount == 0 {
		return nil, httpx.DomainError(422, httpx.CodeCartNoItemsPayable, "carrinho não tem itens disponíveis para pagamento")
	}

	// Add selected shipping cost to the total for the gateway config.
	if shippingSel, _ := s.repo.ReadCartShipping(ctx, s.pool, cart.ID); shippingSel != nil {
		totalAmount += shippingSel.CostCents
	}
	totalAmount = applyCouponDiscount(totalAmount, cart.CouponDiscountCents)

	// Resolve the payment integration to render the FE widget for. Walks the
	// store's priority list as a fallback chain: first integration whose
	// provider can be instantiated (credentials decryptable, provider class
	// supported) wins. The choice is persisted via BindCartPaymentIntegration
	// so subsequent ProcessCardPayment / GeneratePixPayment calls reach the
	// SAME provider — card tokens are gateway-bound.
	chosen, publicKey, methods, err := s.resolveCheckoutIntegration(ctx, cart)
	if err != nil {
		return nil, err
	}

	return &GetCheckoutConfigOutput{
		Provider:         chosen.ProviderName,
		PublicKey:        publicKey,
		AvailableMethods: methods,
		TotalAmount:      totalAmount,
		Currency:         "BRL",
		MaxInstallments:  MaxInstallmentsFor(totalAmount, cart.MinInstallmentCents),
	}, nil
}

// resolveCheckoutIntegration walks the store's payment integrations in
// priority order, picks the first one we can build a config for, and binds it
// to the cart. Honors an existing cart.PaymentIntegrationID — re-binding
// mid-checkout would break card tokens already minted by the FE.
//
// Returns an httpx 422 only when *every* candidate fails, so the merchant
// sees a clear "no working provider" error instead of a generic 500.
func (s *Service) resolveCheckoutIntegration(ctx context.Context, cart *CartRow) (*IntegrationRow, string, []string, error) {
	// Honor a previously chosen integration: the FE may have tokenized cards
	// against its public key already.
	if cart.PaymentIntegrationID != nil && *cart.PaymentIntegrationID != "" {
		bound, err := s.repo.GetIntegrationByID(ctx, *cart.PaymentIntegrationID)
		if err == nil && bound != nil && bound.Status == "active" {
			publicKey, methods, configErr := s.paymentService.GetCheckoutConfig(ctx, bound.ID.String(), cart.StoreID)
			if configErr == nil {
				return bound, publicKey, methods, nil
			}
			logger.From(ctx, s.logger).Warn("bound payment integration failed config, falling through to priority chain",
				zap.String("cart_id", cart.ID),
				zap.String("integration_id", bound.ID.String()),
				zap.Error(configErr),
			)
		}
	}

	candidates, err := s.repo.ListPaymentIntegrations(ctx, cart.StoreID)
	if err != nil {
		return nil, "", nil, err
	}
	if len(candidates) == 0 {
		return nil, "", nil, httpx.DomainError(422, httpx.CodePaymentNotConfigured, "loja não possui integração de pagamento configurada")
	}

	var lastErr error
	for i := range candidates {
		candidate := candidates[i]
		publicKey, methods, configErr := s.paymentService.GetCheckoutConfig(ctx, candidate.ID.String(), cart.StoreID)
		if configErr != nil {
			logger.From(ctx, s.logger).Warn("payment integration failed checkout config, trying next",
				zap.String("cart_id", cart.ID),
				zap.String("integration_id", candidate.ID.String()),
				zap.String("provider", candidate.ProviderName),
				zap.Error(configErr),
			)
			lastErr = configErr
			continue
		}
		if bindErr := s.repo.BindCartPaymentIntegration(ctx, cart.ID, candidate.ID.String()); bindErr != nil {
			// Binding failure is recoverable — the FE still gets a working
			// config; subsequent calls will just re-resolve via the same
			// priority walk and (hopefully) land on the same provider.
			logger.From(ctx, s.logger).Warn("failed to bind cart to payment integration",
				zap.String("cart_id", cart.ID),
				zap.String("integration_id", candidate.ID.String()),
				zap.Error(bindErr),
			)
		}
		return &candidate, publicKey, methods, nil
	}

	if lastErr != nil {
		logger.From(ctx, s.logger).Error("all payment integrations failed checkout config",
			zap.String("cart_id", cart.ID),
			zap.Int("candidates", len(candidates)),
			zap.Error(lastErr),
		)
	}
	return nil, "", nil, httpx.DomainError(422, httpx.CodePaymentUnavailable, "nenhuma integração de pagamento está respondendo agora")
}

// resolvePaymentIntegration returns the integration that should process this
// cart's payment. Prefers the cart's bound integration (set during
// GetCheckoutConfig) so the gateway matches the public key the FE used.
// Falls back to the store's primary when the cart has no binding yet (e.g.,
// legacy carts that started checkout before this rollout).
func (s *Service) resolvePaymentIntegration(ctx context.Context, cart *CartRow) (*IntegrationRow, error) {
	if cart.PaymentIntegrationID != nil && *cart.PaymentIntegrationID != "" {
		bound, err := s.repo.GetIntegrationByID(ctx, *cart.PaymentIntegrationID)
		if err == nil && bound != nil {
			return bound, nil
		}
	}
	primary, err := s.repo.GetPaymentIntegration(ctx, cart.StoreID)
	if err != nil {
		return nil, err
	}
	if primary == nil {
		return nil, httpx.DomainError(422, httpx.CodePaymentNotConfigured, "loja não possui integração de pagamento configurada")
	}
	return primary, nil
}

// ProcessCardPayment processes a card payment with a tokenized card.
func (s *Service) ProcessCardPayment(ctx context.Context, input ProcessCardPaymentInput) (*ProcessCardPaymentOutput, error) {
	// Get cart
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)

	// Validate cart status
	if cart.Status == "expired" {
		return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.DomainError(422, httpx.CodeCartAlreadyPaid, "carrinho já foi pago")
	}
	// Allow checkout for both 'active' (live ongoing) and 'checkout' (live ended) status
	if cart.Status != "checkout" && cart.Status != "active" {
		return nil, httpx.DomainError(422, httpx.CodeCartNotPayable, "carrinho não está disponível para checkout")
	}

	// Persist customer identity + shipping address before requesting payment —
	// the webhook handler will read this when creating the paid order in the ERP.
	if err := s.repo.UpdateCheckoutCustomer(ctx, cart.ID, input.Email, input.CustomerName, input.CustomerDocument, input.CustomerPhone, input.ShippingAddress, input.WhatsappConsent); err != nil {
		return nil, err
	}

	// Iniciação de pagamento = ponto de conversão do pedido-como-reserva
	// (design C, flag por loja) + draft da Order (todas as lojas, best-effort).
	s.fireCheckoutInitHooks(cart.ID, cart.StoreID, cart.StoreSlug)

	// Get cart items
	items, err := s.repo.ListCartItems(ctx, cart.ID)
	if err != nil {
		return nil, err
	}

	// Filter out fully waitlisted items and build checkout items with available quantities
	var checkoutItems []providers.CheckoutItem
	var totalAmount int64
	for _, item := range items {
		// Calculate available quantity (total - waitlisted)
		availableQty := item.Quantity - item.WaitlistedQuantity
		if availableQty <= 0 {
			continue // Skip items that are fully waitlisted
		}
		checkoutItems = append(checkoutItems, providers.CheckoutItem{
			ID:        item.ProductID,
			Name:      item.Name,
			Quantity:  availableQty, // Only the available quantity
			UnitPrice: item.UnitPrice,
			ImageURL:  derefString(item.ImageURL),
		})
		totalAmount += item.UnitPrice * int64(availableQty)
	}

	if len(checkoutItems) == 0 {
		return nil, httpx.DomainError(422, httpx.CodeCartNoItemsPayable, "carrinho não tem itens disponíveis para pagamento")
	}

	// Add selected shipping cost to the total charged at the gateway.
	if shippingSel, _ := s.repo.ReadCartShipping(ctx, s.pool, cart.ID); shippingSel != nil {
		totalAmount += shippingSel.CostCents
	}
	totalAmount = applyCouponDiscount(totalAmount, cart.CouponDiscountCents)

	// O parcelamento pedido tem de caber na regra da loja.
	//
	// A lista que o comprador vê já vem limitada pelo GetCheckoutConfig, mas ela
	// é do navegador: um POST direto com `installments: 12` numa venda de R$ 4,90
	// passaria pela validação da borda (min=1,max=12) e faria o lojista pagar a
	// MDR de doze parcelas sobre cinco reais. A regra é a mesma função dos dois
	// lados, para a tela e o servidor nunca discordarem.
	if max := MaxInstallmentsFor(totalAmount, cart.MinInstallmentCents); input.Installments > max {
		return nil, httpx.DomainError(422, httpx.CodeInstallmentsAboveLimit,
			fmt.Sprintf("esta compra aceita no máximo %d parcela(s)", max))
	}


	// Use the integration the cart was bound to during GetCheckoutConfig so
	// card tokens reach the gateway that issued the public key on the FE.
	paymentIntegration, err := s.resolvePaymentIntegration(ctx, cart)
	if err != nil {
		return nil, err
	}

	// Build customer name
	customerName := input.CustomerName
	if customerName == "" {
		customerName = cart.PlatformHandle
	}

	// Build notify URL — must match the public webhook route:
	// POST /api/webhooks/:provider/:storeId  (no /v1/integrations prefix; that
	// group requires auth and would 401 the provider's notification, dropping
	// the post-payment ERP finalisation entirely).
	notifyURL := fmt.Sprintf("%s/api/webhooks/%s/%s",
		config.WebhookBaseURL.String(),
		paymentIntegration.ProviderName,
		cart.StoreID,
	)

	// Process payment via integration service
	result, err := s.paymentService.ProcessCardPayment(ctx, payment.ProcessCardPaymentInput{
		IntegrationID: paymentIntegration.ID.String(),
		StoreID:       cart.StoreID,
		CartID:        cart.ID,
		CardToken:     input.CardToken,
		Installments:  input.Installments,
		Customer: providers.CheckoutCustomer{
			Email:    input.Email,
			Name:     customerName,
			Phone:    input.CustomerPhone,
			Document: input.CustomerDocument,
			Address:  toProviderAddress(input.ShippingAddress),
		},
		Items:           checkoutItems,
		TotalAmount:     totalAmount,
		Currency:        "BRL",
		NotifyURL:       notifyURL,
		PaymentMethodID: input.PaymentMethodID,
		IssuerID:        input.IssuerID,
		DeviceID:        input.DeviceID,
		IPAddress:       input.IPAddress,
		Metadata: map[string]any{
			"cart_id":    cart.ID,
			"cart_token": cart.Token,
			"event_id":   cart.EventID,
			"store_id":   cart.StoreID,
		},
	})
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to process card payment",
			zap.String("cart_id", cart.ID),
			zap.String("integration_id", paymentIntegration.ID.String()),
			zap.String("provider", paymentIntegration.ProviderName),
			zap.Error(err),
		)
		// Provider call failed at the transport layer (5xx / network / 429).
		// Unbind the cart from this provider so the next /config call lets
		// resolveCheckoutIntegration pick the next one in the priority chain.
		// Without this, the buyer stays trapped on the failing gateway because
		// the binding wins inside resolvePaymentIntegration regardless of how
		// many times they retry. The FE re-tokenizes against the new public
		// key after seeing this 422 + re-fetching /config.
		if clearErr := s.repo.ClearCartPaymentIntegration(ctx, cart.ID); clearErr != nil {
			logger.From(ctx, s.logger).Warn("failed to clear cart payment binding after provider failure",
				zap.String("cart_id", cart.ID),
				zap.Error(clearErr),
			)
		}
		return nil, httpx.ErrUnprocessable("provedor de pagamento indisponível, tente novamente em instantes")
	}

	// Update cart payment status if approved. ERP finalisation is left to the
	// provider webhook — running it here too caused a real race: both
	// goroutines passed the cart.external_order_id idempotency check while
	// empty and both called Tiny CreateOrder; one got rate-limited (429),
	// and the other could just as easily have produced a duplicate Tiny order.
	if result.Status == "approved" {
		// Prefer the gateway-reported authorization instant (date_approved /
		// charges[0].paid_at) over the server clock so the receipt and the
		// ERP records match what the customer sees on the gateway dashboard
		// and we don't drift if the API host's clock is skewed. Falls back
		// to time.Now() when the provider omitted the field.
		if err := s.repo.UpdatePaymentStatus(ctx, cart.ID, "paid", result.PaymentID, result.PaidAt); err != nil {
			// Guard da corrida com a expiração: o UPDATE guarded volta 0 rows
			// (ErrNotFound) quando o cart expirou/cancelou entre a aprovação no
			// gateway e este write. NÃO finalizamos um cart não-pagável; o
			// dinheiro entrou no gateway, então fica para a reconciliação (E6).
			logger.From(ctx, s.logger).Warn("card approved at gateway but cart not payable (expired/cancelled) — skipping finalization, needs reconciliation",
				zap.String("cart_id", cart.ID),
				zap.String("payment_id", result.PaymentID),
				zap.Error(err),
			)
		} else {
			// Best-effort capture of card details for the post-payment receipt
			// (public checkout `payment` block). Failure here must not break the
			// happy path — the comprovante just renders without these fields.
			if err := s.repo.WriteCartCardPayment(ctx, s.pool, cart.ID, result.CardBrand, result.LastFourDigits, result.Installments, result.AuthorizationCode); err != nil {
				logger.From(ctx, s.logger).Warn("failed to persist card payment details",
					zap.String("cart_id", cart.ID),
					zap.Error(err),
				)
			}
			// Customer-facing post-payment flow (tracking token + timeline +
			// receipt) NÃO roda mais inline aqui (Fatia A4): o token e a timeline
			// `payment_confirmed` nascem na materialização da Order, disparada pelo
			// fato cart.paid do webhook (order/listeners.OnCartPaid), e o recibo é
			// reactor do domínio Notification. Tudo idempotente e ancorado no fato.
			//
			// The canonical cart.paid FACT is emitted only by the payment webhook
			// consumer (ProcessPaymentNotification) — the single source of truth,
			// carrying the fresh gateway snapshot the ERP reactor needs. The card
			// path used to emit its own snapshot-less cart.paid too, which raced the
			// webhook's and finalised ERP without payment details; centralising on
			// the webhook fixes that.
		}
	}

	logger.From(ctx, s.logger).Info("card payment processed",
		zap.String("cart_id", cart.ID),
		zap.String("payment_id", result.PaymentID),
		zap.String("status", result.Status),
		zap.Int64("amount", result.Amount),
	)

	return &ProcessCardPaymentOutput{
		PaymentID:         result.PaymentID,
		Status:            result.Status,
		StatusDetail:      result.StatusDetail,
		Message:           result.Message,
		Amount:            result.Amount,
		Installments:      result.Installments,
		LastFourDigits:    result.LastFourDigits,
		CardBrand:         result.CardBrand,
		AuthorizationCode: result.AuthorizationCode,
	}, nil
}

// GeneratePix generates a PIX QR code for payment.
func (s *Service) GeneratePix(ctx context.Context, input GeneratePixInput) (*GeneratePixOutput, error) {
	// Get cart
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)

	// Validate cart status
	if cart.Status == "expired" {
		return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.DomainError(422, httpx.CodeCartAlreadyPaid, "carrinho já foi pago")
	}
	// Allow checkout for both 'active' (live ongoing) and 'checkout' (live ended) status
	if cart.Status != "checkout" && cart.Status != "active" {
		return nil, httpx.DomainError(422, httpx.CodeCartNotPayable, "carrinho não está disponível para checkout")
	}

	// Persist customer identity + shipping address before requesting payment —
	// the webhook handler will read this when creating the paid order in the ERP.
	if err := s.repo.UpdateCheckoutCustomer(ctx, cart.ID, input.Email, input.CustomerName, input.CustomerDocument, input.CustomerPhone, input.ShippingAddress, input.WhatsappConsent); err != nil {
		return nil, err
	}

	// Iniciação de pagamento = ponto de conversão do pedido-como-reserva
	// (design C, flag por loja) + draft da Order (todas as lojas, best-effort).
	s.fireCheckoutInitHooks(cart.ID, cart.StoreID, cart.StoreSlug)

	// Get cart items
	items, err := s.repo.ListCartItems(ctx, cart.ID)
	if err != nil {
		return nil, err
	}

	// Filter out fully waitlisted items and build checkout items with available quantities
	var checkoutItems []providers.CheckoutItem
	var totalAmount int64
	for _, item := range items {
		// Calculate available quantity (total - waitlisted)
		availableQty := item.Quantity - item.WaitlistedQuantity
		if availableQty <= 0 {
			continue // Skip items that are fully waitlisted
		}
		checkoutItems = append(checkoutItems, providers.CheckoutItem{
			ID:        item.ProductID,
			Name:      item.Name,
			Quantity:  availableQty, // Only the available quantity
			UnitPrice: item.UnitPrice,
			ImageURL:  derefString(item.ImageURL),
		})
		totalAmount += item.UnitPrice * int64(availableQty)
	}

	if len(checkoutItems) == 0 {
		return nil, httpx.DomainError(422, httpx.CodeCartNoItemsPayable, "carrinho não tem itens disponíveis para pagamento")
	}

	// Add selected shipping cost to the total charged at the gateway.
	shippingPart := int64(0)
	if shippingSel, _ := s.repo.ReadCartShipping(ctx, s.pool, cart.ID); shippingSel != nil {
		shippingPart = shippingSel.CostCents
		totalAmount += shippingSel.CostCents
	}
	totalAmount = applyCouponDiscount(totalAmount, cart.CouponDiscountCents)

	// Apply the event-level Pix discount on the (subtotal − coupon) portion.
	// The free Pix flow always reaches here, so this is the single point that
	// makes the gateway charge the discounted amount.
	if cart.EventPixDiscountPercent == 0 {
		if _, pct, err := s.repo.LoadEventCheckoutFlags(ctx, s.pool, cart.EventID); err == nil {
			cart.EventPixDiscountPercent = pct
		}
	}
	if cart.EventPixDiscountPercent > 0 {
		base := totalAmount - shippingPart
		if base < 0 {
			base = 0
		}
		pixDiscountCents := base * int64(cart.EventPixDiscountPercent) / 100
		totalAmount -= pixDiscountCents
		if totalAmount < 0 {
			totalAmount = 0
		}
		logger.From(ctx, s.logger).Info("pix discount applied",
			zap.String("cart_id", cart.ID),
			zap.Int("percent", cart.EventPixDiscountPercent),
			zap.Int64("discount_cents", pixDiscountCents),
		)
	}

	// Antes de abrir outra, mata a cobranca anterior deste carrinho.
	//
	// Cada geracao abre uma cobranca nova no gateway; sem cancelar a de tras,
	// duas ficam pagaveis ao mesmo tempo e o comprador escolhe, sem saber, qual
	// valor manda. O caminho normal ja invalida na mutacao do carrinho — este
	// aqui cobre gerar de novo sem mudar nada (QR expirado, aba reaberta).
	s.invalidatePendingPix(ctx, cart)

	// Use the integration the cart was bound to during GetCheckoutConfig.
	paymentIntegration, err := s.resolvePaymentIntegration(ctx, cart)
	if err != nil {
		return nil, err
	}

	// Build customer name
	customerName := input.CustomerName
	if customerName == "" {
		customerName = cart.PlatformHandle
	}

	// Build notify URL — must match the public webhook route:
	// POST /api/webhooks/:provider/:storeId  (no /v1/integrations prefix; that
	// group requires auth and would 401 the provider's notification, dropping
	// the post-payment ERP finalisation entirely).
	notifyURL := fmt.Sprintf("%s/api/webhooks/%s/%s",
		config.WebhookBaseURL.String(),
		paymentIntegration.ProviderName,
		cart.StoreID,
	)

	// Generate PIX via integration service
	result, err := s.paymentService.GeneratePixPayment(ctx, payment.GeneratePixPaymentInput{
		IntegrationID: paymentIntegration.ID.String(),
		StoreID:       cart.StoreID,
		CartID:        cart.ID,
		Customer: providers.CheckoutCustomer{
			Email:    input.Email,
			Name:     customerName,
			Phone:    input.CustomerPhone,
			Document: input.CustomerDocument,
			Address:  toProviderAddress(input.ShippingAddress),
		},
		Items:       checkoutItems,
		TotalAmount: totalAmount,
		Currency:    "BRL",
		NotifyURL:   notifyURL,
		Metadata: map[string]any{
			"cart_id":    cart.ID,
			"cart_token": cart.Token,
			"event_id":   cart.EventID,
			"store_id":   cart.StoreID,
		},
	})
	if err != nil {
		logger.From(ctx, s.logger).Error("failed to generate pix",
			zap.String("cart_id", cart.ID),
			zap.String("integration_id", paymentIntegration.ID.String()),
			zap.String("provider", paymentIntegration.ProviderName),
			zap.Error(err),
		)
		// Same self-healing as ProcessCardPayment: drop the binding so the
		// retry can land on the next priority integration. PIX has no token,
		// so the FE doesn't even need a re-tokenization step here.
		if clearErr := s.repo.ClearCartPaymentIntegration(ctx, cart.ID); clearErr != nil {
			logger.From(ctx, s.logger).Warn("failed to clear cart payment binding after pix failure",
				zap.String("cart_id", cart.ID),
				zap.Error(clearErr),
			)
		}
		return nil, httpx.ErrUnprocessable("provedor de pagamento indisponível, tente novamente em instantes")
	}

	// Update cart with payment ID for tracking
	// Guarda o que da para CANCELAR e por quanto. `checkout_id` acima nao serve:
	// no Pagar.me ele e a ordem, e o cancelamento exige a cobranca.
	//
	// Falhar aqui deixa uma cobranca viva que ninguem sabe cancelar, entao o log
	// e de erro — mas nao derruba a geracao: o comprador tem um QR valido na mao
	// e negar isso seria trocar um risco por um prejuizo certo.
	if err := s.repo.SetCartPixCharge(ctx, cart.ID, result.CancelID, totalAmount); err != nil {
		logger.From(ctx, s.logger).Error("PIX charge not recorded: it cannot be invalidated later",
			zap.String("cart_id", cart.ID),
			zap.String("cancel_id", result.CancelID),
			zap.Error(err),
		)
	}

	if err := s.repo.UpdateCheckoutInfo(ctx, UpdateCheckoutParams{
		CartID:     cart.ID,
		CheckoutID: result.PaymentID,
	}); err != nil {
		logger.From(ctx, s.logger).Error("failed to update checkout info",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
	}

	logger.From(ctx, s.logger).Info("pix payment generated",
		zap.String("cart_id", cart.ID),
		zap.String("payment_id", result.PaymentID),
		zap.Int64("amount", result.Amount),
	)

	return &GeneratePixOutput{
		PaymentID:  result.PaymentID,
		QRCode:     result.QRCode,
		QRCodeText: result.QRCodeText,
		Amount:     result.Amount,
		ExpiresAt:  result.ExpiresAt,
		TicketURL:  result.TicketURL,
	}, nil
}

// =============================================================================
// CART ITEM MUTATIONS
// =============================================================================

// UpdateCartItemQuantity changes an existing item's quantity. delta > 0 grows
// (and bumps ERP reservation), delta < 0 shrinks (and reverses ERP). Returns
// the freshly-loaded cart payload so the frontend can swap the React Query
// cache in one call.
func (s *Service) UpdateCartItemQuantity(ctx context.Context, input MutateCartItemInput) (*GetCartForCheckoutOutput, error) {
	cart, item, err := s.loadEditableCartItem(ctx, input.Token, input.ItemID)
	if err != nil {
		return nil, err
	}
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)
	if input.Quantity < 1 {
		return nil, httpx.ErrUnprocessable("quantidade deve ser pelo menos 1")
	}

	delta := input.Quantity - item.Quantity
	if delta == 0 {
		return s.GetCartForCheckout(ctx, GetCartForCheckoutInput{Token: input.Token})
	}

	if delta > 0 {
		if err := s.validateQuantityCap(ctx, cart, item.ProductID, item.Quantity, input.Quantity); err != nil {
			return nil, err
		}
	}

	// A linha tem DUAS parcelas: o que está segurado no estoque e o que está
	// esperando na fila. O que move estoque é só a primeira.
	//
	// Todo este bloco tratava `quantity` como se fosse tudo segurado. Baixar de
	// 5 (com 3 na fila, 2 segurados) para 2 mandava delta -3 ao estoque quando
	// só 2 haviam sido tirados: uma unidade creditada que nunca existiu, nos
	// dois sistemas. E a linha ficava com 2 total e 3 em fila — disponível
	// NEGATIVO, que os outros quatro pontos que calculam
	// `quantity - waitlisted_quantity` leriam como número válido.
	//
	// Ao reduzir, some primeiro a parte em FILA: ela não segura nada, então é a
	// que o comprador abre mão sem custo. Ao aumentar, o acréscimo vira
	// segurado — validateQuantityCap acima já garantiu que há estoque.
	heldAfter, waitlistedAfter, stockDelta := splitQuantityChange(
		item.Quantity, item.WaitlistedQuantity, input.Quantity)
	_ = heldAfter

	if err := s.repo.EnsureInitialSnapshot(ctx, cart.ID); err != nil {
		return nil, err
	}
	// Trava otimista: a escrita só vale se a linha ainda estiver como a lemos.
	//
	// Entre a leitura lá em cima e esta escrita há uma chamada HTTP ao Tiny que
	// passa pelo limitador de ~1 req/s — a janela dura SEGUNDOS. Em 05/08 dois
	// escritores leram quantity=2 com 3s de diferença; o segundo calculou o
	// delta contra um valor obsoleto e uma unidade sumiu do carrinho enquanto
	// uma saída a mais era lançada no ERP.
	ok, err := s.repo.SetCartItemSplitIfUnchanged(ctx, item.ID, item.Quantity, input.Quantity, waitlistedAfter)
	if err != nil {
		return nil, err
	}
	if ok && waitlistedAfter < item.WaitlistedQuantity {
		// A parcela em fila encolheu, então as linhas de waitlist_items que ela
		// representava precisam morrer junto.
		//
		// Sem isto elas ficam órfãs e a próxima promoção as reivindica: debita
		// estoque local, emite uma SAÍDA no Tiny, e não entrega unidade a
		// ninguém — o comprador já tinha desistido daquela parcela. É o gerador
		// crônico do "tirei uma unidade e o estoque ficou errado".
		//
		// Best-effort: falhar aqui não pode desfazer a alteração que o comprador
		// acabou de fazer, e a linha órfã é recuperável pela expiração da fila.
		if n, cancelErr := s.integrationService.CancelWaitlistForCartProduct(ctx, cart.ID, item.ProductID); cancelErr != nil {
			logger.From(ctx, s.logger).Warn("failed to cancel waitlist rows freed by the quantity decrease",
				zap.String("cart_id", cart.ID),
				zap.String("product_id", item.ProductID),
				zap.Error(cancelErr),
			)
		} else if n > 0 {
			logger.From(ctx, s.logger).Info("waitlist rows cancelled after checkout decrease",
				zap.String("cart_id", cart.ID),
				zap.String("product_id", item.ProductID),
				zap.Int("cancelled", n),
			)
		}
	}
	if !ok {
		// Alguém alterou este item entre a leitura e a escrita. Recusar é o
		// único caminho correto: o delta calculado não vale mais, e aplicá-lo
		// corromperia a conta de estoque.
		return nil, httpx.DomainError(409, httpx.CodeCartItemChanged,
			"a quantidade deste item mudou enquanto você editava, recarregue e tente de novo")
	}

	movementID, syncErr := s.integrationService.AdjustStockReservationDelta(
		ctx, cart.StoreID, cart.ID, cart.EventID, item.ProductID,
		stockDelta, item.UnitPrice, cart.PlatformHandle, integration.StockOpUnspecified,
	)
	if syncErr != nil {
		// Roll back the local change so the buyer sees the failure clearly.
		// Restaura as DUAS parcelas: reverter só o total deixaria a fila com o
		// valor novo sobre um total antigo.
		_, _ = s.repo.SetCartItemSplitIfUnchanged(ctx, item.ID, input.Quantity, item.Quantity, item.WaitlistedQuantity)
		// Propagate typed httpx errors verbatim (e.g., "estoque insuficiente")
		// so the buyer sees the actual reason instead of a generic retry copy.
		var svcErr *httpx.ServiceError
		if errors.As(syncErr, &svcErr) {
			return nil, syncErr
		}
		logger.From(ctx, s.logger).Error("ERP delta sync failed, rolled back cart item quantity",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", item.ProductID),
			zap.Int("delta", delta),
			zap.Error(syncErr),
		)
		return nil, httpx.ErrUnprocessable("não foi possível atualizar o estoque, tente novamente")
	}

	mutationType := "quantity_increased"
	if delta < 0 {
		mutationType = "quantity_decreased"
	}
	if err := s.repo.RecordMutation(ctx, s.pool, MutationParams{
		CartID:         cart.ID,
		ProductID:      item.ProductID,
		MutationType:   mutationType,
		QuantityBefore: item.Quantity,
		QuantityAfter:  input.Quantity,
		UnitPrice:      item.UnitPrice,
		ERPMovementID:  movementID,
	}); err != nil {
		logger.From(ctx, s.logger).Warn("cart item quantity changed but mutation log write failed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", item.ProductID),
			zap.Error(err),
		)
	}

	s.reevaluateCouponAfterCartMutation(ctx, cart.ID)
	s.invalidatePendingPix(ctx, cart)

	// Quando a quantidade diminui, parte do estoque foi devolvida — tenta
	// promover o próximo da fila. Best-effort em goroutine para não
	// bloquear o response do checkout.
	if delta < 0 {
		go s.integrationService.ProcessWaitlistForProduct(logger.WithStore(context.Background(), cart.StoreID, cart.StoreSlug), cart.EventID, item.ProductID, cart.StoreID)
	}

	return s.GetCartForCheckout(ctx, GetCartForCheckoutInput{Token: input.Token})
}

// RemoveCartItem deletes an item entirely and reverses the ERP reservation.
func (s *Service) RemoveCartItem(ctx context.Context, input MutateCartItemInput) (*GetCartForCheckoutOutput, error) {
	cart, item, err := s.loadEditableCartItem(ctx, input.Token, input.ItemID)
	if err != nil {
		return nil, err
	}
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)

	if err := s.repo.EnsureInitialSnapshot(ctx, cart.ID); err != nil {
		return nil, err
	}
	if err := s.repo.DeleteCartItem(ctx, item.ID); err != nil {
		return nil, err
	}

	// Devolver ao ERP só o que SAIU dele.
	//
	// A parcela em fila nunca virou saída — ela existe justamente porque não
	// havia estoque. Mandar `item.Quantity` devolvia também essa parcela: um
	// item com quantity=3 e waitlisted=2 tem 1 unidade reservada, e a remoção
	// creditava 3 no Tiny. Duas unidades nascidas do nada, por remoção.
	//
	// É a mesma conta que o PATCH já fazia certo via splitQuantityChange
	// (:1158) — só a remoção ficou de fora.
	held := item.Quantity - item.WaitlistedQuantity
	if held < 0 {
		held = 0
	}

	movementID, syncErr := s.integrationService.AdjustStockReservationDelta(
		ctx, cart.StoreID, cart.ID, cart.EventID, item.ProductID,
		-held, item.UnitPrice, cart.PlatformHandle, integration.StockOpUnspecified,
	)
	if syncErr != nil {
		// Re-create the row at the original quantity to keep state consistent.
		if _, restoreErr := s.repo.CreateCartItem(ctx, cart.ID, item.ProductID, item.Quantity, item.UnitPrice); restoreErr != nil {
			logger.From(ctx, s.logger).Error("failed to restore cart item after ERP failure — manual intervention needed",
				zap.String("cart_id", cart.ID),
				zap.String("product_id", item.ProductID),
				zap.Error(restoreErr),
			)
		}
		var svcErr *httpx.ServiceError
		if errors.As(syncErr, &svcErr) {
			return nil, syncErr
		}
		logger.From(ctx, s.logger).Error("ERP reversal failed on remove, restored cart item",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", item.ProductID),
			zap.Error(syncErr),
		)
		return nil, httpx.ErrUnprocessable("não foi possível remover o item, tente novamente")
	}

	if err := s.repo.RecordMutation(ctx, s.pool, MutationParams{
		CartID:         cart.ID,
		ProductID:      item.ProductID,
		MutationType:   "item_removed",
		QuantityBefore: item.Quantity,
		QuantityAfter:  0,
		UnitPrice:      item.UnitPrice,
		ERPMovementID:  movementID,
	}); err != nil {
		logger.From(ctx, s.logger).Warn("item removed but mutation log write failed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", item.ProductID),
			zap.Error(err),
		)
	}

	s.reevaluateCouponAfterCartMutation(ctx, cart.ID)
	s.invalidatePendingPix(ctx, cart)

	// Estoque liberado pela remoção — promove o próximo da fila se houver.
	go s.integrationService.ProcessWaitlistForProduct(logger.WithStore(context.Background(), cart.StoreID, cart.StoreSlug), cart.EventID, item.ProductID, cart.StoreID)

	return s.GetCartForCheckout(ctx, GetCartForCheckoutInput{Token: input.Token})
}

// DropFromWaitlist é o "sair da fila": cancela uma waitlist entry do
// cart. Aceita carts ativos (mesmo regra de loadEditableCart) — não faz
// sentido cancelar fila de um cart já pago/expirado. Delegate ao
// integrationService que conhece a mecânica de devolver estoque caso a
// entry estivesse 'notified'.
func (s *Service) DropFromWaitlist(ctx context.Context, input DropFromWaitlistInput) (*GetCartForCheckoutOutput, error) {
	cart, err := s.loadEditableCart(ctx, input.Token)
	if err != nil {
		return nil, err
	}
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)
	if input.WaitlistItemID == "" {
		return nil, httpx.ErrBadRequest("waitlist id is required")
	}
	changed, err := s.integrationService.CancelWaitlistItem(ctx, input.WaitlistItemID, cart.ID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("failed to cancel waitlist item",
			zap.String("cart_id", cart.ID),
			zap.String("waitlist_item_id", input.WaitlistItemID),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("não foi possível sair da fila, tente novamente")
	}
	if !changed {
		return nil, httpx.ErrNotFound("item da fila não encontrado neste carrinho")
	}
	return s.GetCartForCheckout(ctx, GetCartForCheckoutInput{Token: input.Token})
}

// AddCartItem adds a brand-new product to the cart (or sums onto an existing
// row for the same product). Validates the product against the event whitelist
// and quantity cap before touching the ERP.
func (s *Service) AddCartItem(ctx context.Context, input MutateCartItemInput) (*GetCartForCheckoutOutput, error) {
	cart, err := s.loadEditableCart(ctx, input.Token)
	if err != nil {
		return nil, err
	}
	ctx = logger.WithStore(ctx, cart.StoreID, cart.StoreSlug)
	if input.Quantity < 1 {
		return nil, httpx.ErrUnprocessable("quantidade deve ser pelo menos 1")
	}

	cfg, err := s.repo.GetEventProductForCart(ctx, cart.EventID, cart.StoreID, input.ProductID)
	if err != nil {
		return nil, err
	}
	if !cfg.Active {
		return nil, httpx.ErrUnprocessable("produto não está ativo")
	}
	if !cfg.IsAllowed {
		return nil, httpx.ErrUnprocessable("produto não disponível neste evento")
	}

	existing, err := s.repo.FindCartItemByProduct(ctx, cart.ID, input.ProductID)
	if err != nil {
		return nil, err
	}
	currentQty := 0
	if existing != nil {
		currentQty = existing.Quantity
	}
	desiredQty := currentQty + input.Quantity
	if cfg.MaxQuantity > 0 && desiredQty > cfg.MaxQuantity {
		return nil, httpx.ErrUnprocessable(fmt.Sprintf("limite de %d por item", cfg.MaxQuantity))
	}
	// Mesma regra do PATCH: o estoque limita o que ESTÁ SENDO ACRESCENTADO, não
	// o total. As unidades já no carrinho deste comprador saíram da prateleira
	// quando entraram nele — cobrá-las de novo aqui as contaria duas vezes.
	if !stockCoversIncrease(cfg.Stock, input.Quantity) {
		return nil, httpx.ErrUnprocessable(fmt.Sprintf("apenas %d em estoque", cfg.Stock))
	}

	if err := s.repo.EnsureInitialSnapshot(ctx, cart.ID); err != nil {
		return nil, err
	}

	if existing != nil {
		if err := s.repo.SetCartItemQuantity(ctx, existing.ID, desiredQty); err != nil {
			return nil, err
		}
	} else {
		if _, err := s.repo.CreateCartItem(ctx, cart.ID, input.ProductID, input.Quantity, cfg.UnitPrice); err != nil {
			return nil, err
		}
	}

	movementID, syncErr := s.integrationService.AdjustStockReservationDelta(
		ctx, cart.StoreID, cart.ID, cart.EventID, input.ProductID,
		input.Quantity, cfg.UnitPrice, cart.PlatformHandle, integration.StockOpUnspecified,
	)
	if syncErr != nil {
		// Roll back. With existing != nil, restore previous qty; with new row, delete it.
		if existing != nil {
			_ = s.repo.SetCartItemQuantity(ctx, existing.ID, currentQty)
		} else if rollback, _ := s.repo.FindCartItemByProduct(ctx, cart.ID, input.ProductID); rollback != nil {
			_ = s.repo.DeleteCartItem(ctx, rollback.ID)
		}
		var svcErr *httpx.ServiceError
		if errors.As(syncErr, &svcErr) {
			return nil, syncErr
		}
		return nil, httpx.ErrUnprocessable("não foi possível adicionar o produto, tente novamente")
	}

	mutationType := "item_added"
	if existing != nil {
		mutationType = "quantity_increased"
	}
	if err := s.repo.RecordMutation(ctx, s.pool, MutationParams{
		CartID:         cart.ID,
		ProductID:      input.ProductID,
		MutationType:   mutationType,
		QuantityBefore: currentQty,
		QuantityAfter:  desiredQty,
		UnitPrice:      cfg.UnitPrice,
		ERPMovementID:  movementID,
	}); err != nil {
		logger.From(ctx, s.logger).Warn("cart item added but mutation log write failed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", input.ProductID),
			zap.Error(err),
		)
	}

	s.reevaluateCouponAfterCartMutation(ctx, cart.ID)
	s.invalidatePendingPix(ctx, cart)

	return s.GetCartForCheckout(ctx, GetCartForCheckoutInput{Token: input.Token})
}

// reevaluateCouponAfterCartMutation calls the coupon lifecycle hook so the
// applied coupon (if any) is kept in sync with the new subtotal. We log
// and swallow the error: the cart mutation already committed and the next
// /checkout fetch (or another mutation) will reconcile, so failing the
// caller would leave the buyer staring at a successful mutation that
// pretends it failed.
// invalidatePendingPix mata, no gateway, o QR que o comprador ja tem na mao.
//
// Sem isto o codigo antigo continua pagavel ate expirar. O comprador que copiou
// o "copia e cola", mexeu no carrinho e pagou pelo app do banco manda o valor
// ANTIGO — e o webhook chega com uma quantia que nao corresponde a nenhum
// carrinho atual. Some o QR da tela nao resolve: ele ja saiu da tela.
//
// A leitura LIMPA a coluna na mesma instrucao, entao duas mutacoes simultaneas
// no mesmo carrinho nao disparam dois cancelamentos para a mesma cobranca.
//
// Best-effort de proposito. Se o gateway recusar — o caso normal e a cobranca ja
// ter sido paga entre a leitura e a chamada — nao ha o que desfazer do lado do
// comprador: a mutacao dele ja aconteceu, e transformar isso em erro seria
// punir quem so mudou de ideia sobre a quantidade. Fica no log, alto, porque
// significa QR vivo cobrando valor obsoleto.
func (s *Service) invalidatePendingPix(ctx context.Context, cart *CartRow) {
	cancelID, amountCents, err := s.repo.TakeCartPixCharge(ctx, cart.ID)
	if err != nil {
		logger.From(ctx, s.logger).Warn("could not read pending pix charge to invalidate",
			zap.String("cart_id", cart.ID), zap.Error(err))
		return
	}
	if cancelID == "" {
		return
	}

	integration, err := s.resolvePaymentIntegration(ctx, cart)
	if err != nil {
		logger.From(ctx, s.logger).Error("PIX charge left alive: no payment integration to cancel it",
			zap.String("cart_id", cart.ID),
			zap.String("cancel_id", cancelID),
			zap.Int64("amount_cents", amountCents),
			zap.Error(err))
		return
	}

	if err := s.paymentService.CancelPixPayment(ctx, integration.ID.String(), cart.StoreID, cancelID); err != nil {
		logger.From(ctx, s.logger).Error("PIX charge left alive: gateway refused the cancellation",
			zap.String("cart_id", cart.ID),
			zap.String("cancel_id", cancelID),
			zap.Int64("amount_cents", amountCents),
			zap.String("provider", integration.ProviderName),
			zap.Error(err))
		return
	}

	logger.From(ctx, s.logger).Info("pending PIX invalidated after cart change",
		zap.String("cart_id", cart.ID),
		zap.String("cancel_id", cancelID),
		zap.Int64("amount_cents", amountCents),
	)
}

func (s *Service) reevaluateCouponAfterCartMutation(ctx context.Context, cartID string) {
	if s.couponLifecycle == nil {
		return
	}
	if err := s.couponLifecycle.OnCartMutated(ctx, cartID); err != nil {
		logger.From(ctx, s.logger).Warn("coupon re-evaluation after cart mutation failed",
			zap.String("cart_id", cartID),
			zap.Error(err),
		)
	}
}

// loadEditableCart asserts the cart is in a state that accepts mutations.
func (s *Service) loadEditableCart(ctx context.Context, token string) (*CartRow, error) {
	cart, err := s.repo.GetCartByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if cart.Status == "expired" {
		return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
	}
	// Cart cancelado (pelo lojista ou por bloqueio do comprador) não aceita mais
	// mutação de item: editar aqui mexeria em estoque e reservas de ERP de um
	// carrinho morto — o estorno já devolveu tudo.
	if cart.Status == "cancelled" {
		return nil, httpx.ErrConflict("carrinho cancelado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.ErrConflict("carrinho já foi pago")
	}
	if !cart.AllowEdit {
		return nil, httpx.ErrConflict("edição do carrinho desabilitada para esta loja")
	}
	if cart.ExpiresAt != nil && cart.ExpiresAt.Before(time.Now()) {
		return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
	}
	return cart, nil
}

// loadEditableCartItem returns the cart and the specific item, asserting the
// item belongs to the cart and the cart is editable.
func (s *Service) loadEditableCartItem(ctx context.Context, token, itemID string) (*CartRow, *CartItemRow, error) {
	cart, err := s.loadEditableCart(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	item, err := s.repo.GetCartItem(ctx, itemID)
	if err != nil {
		return nil, nil, err
	}
	if item.CartID != cart.ID {
		return nil, nil, httpx.ErrNotFound("item não encontrado neste carrinho")
	}
	return cart, item, nil
}

// stockCoversIncrease diz se o saldo restante cobre o ACRÉSCIMO pedido.
//
// Ponto único da regra: o PATCH de quantidade e a adição de item aplicavam a
// mesma conta em dois lugares, e o erro estava nos dois. `stock` é o que sobrou
// na prateleira — as unidades que este comprador já tem saíram dela quando
// entraram no carrinho dele, então comparar o TOTAL contra o saldo as cobra
// duas vezes.
//
// stock <= 0 não bloqueia aqui de propósito: produto sem estoque é caminho de
// fila de espera, decidido antes; esta função só responde "o saldo cobre o que
// está sendo somado".
func stockCoversIncrease(stock, added int) bool {
	if added <= 0 {
		return true
	}
	return stock >= added
}

// validateQuantityCap re-checks per-item caps + product stock for an increase.
//
// As duas checagens usam números DIFERENTES, e confundi-los é o bug que estava
// aqui:
//
//   - o TETO é sobre o total: "no máximo 3 deste produto" fala do carrinho
//     inteiro, então compara com desiredQty;
//   - o ESTOQUE é sobre o acréscimo: cfg.Stock é o que SOBROU na prateleira, e
//     as unidades que este comprador já tem saíram dela quando entraram no
//     carrinho dele.
//
// Comparar o total contra o saldo faz as unidades do próprio comprador
// contarem contra ele. Com 5 no estoque e 3 no carrinho, sobram 2; pedir 4
// virava `4 > 2` e recusava com "apenas 2 em estoque" — ele não conseguia
// passar de 3 num produto que tinha 5.
func (s *Service) validateQuantityCap(ctx context.Context, cart *CartRow, productID string, currentQty, desiredQty int) error {
	cfg, err := s.repo.GetEventProductForCart(ctx, cart.EventID, cart.StoreID, productID)
	if err != nil {
		return err
	}
	if cfg.MaxQuantity > 0 && desiredQty > cfg.MaxQuantity {
		return httpx.ErrUnprocessable(fmt.Sprintf("limite de %d por item", cfg.MaxQuantity))
	}
	if !stockCoversIncrease(cfg.Stock, desiredQty-currentQty) {
		return httpx.ErrUnprocessable(fmt.Sprintf("apenas %d em estoque", cfg.Stock))
	}
	return nil
}

// GetPaymentStatus retrieves the current payment status.
func (s *Service) GetPaymentStatus(ctx context.Context, input GetPaymentStatusInput) (*GetPaymentStatusOutput, error) {
	// Get cart
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}

	message := ""
	switch cart.PaymentStatus {
	case "paid":
		message = "Pagamento confirmado"
	case "pending":
		message = "Aguardando pagamento"
	case "failed":
		message = "Pagamento não aprovado"
	default:
		message = "Status: " + cart.PaymentStatus
	}

	return &GetPaymentStatusOutput{
		Status:        cart.Status,
		PaymentStatus: cart.PaymentStatus,
		PaidAt:        cart.PaidAt,
		Message:       message,
	}, nil
}

// splitQuantityChange reparte a quantidade nova entre o que fica SEGURADO no
// estoque e o que fica esperando na FILA, e devolve quanto o estoque tem de se
// mover.
//
// A linha de carrinho tem duas parcelas e só a primeira move estoque:
//
//	total = segurado + em fila
//
// AO REDUZIR, some primeiro a parte em FILA — ela não segura nada, então é a
// que o comprador abre mão sem custo para ninguém. Só depois de zerá-la é que
// se devolve estoque de verdade.
//
// AO AUMENTAR, o acréscimo inteiro vira segurado: quem chama já validou que há
// saldo (validateQuantityCap).
//
// Isto era feito sobre o TOTAL, ignorando a fila. Baixar de 5 (3 na fila, 2
// segurados) para 2 creditava 3 unidades ao estoque quando só 2 haviam sido
// tiradas — uma unidade inventada, no LiveCart e no Tiny — e deixava a linha
// com disponível negativo.
func splitQuantityChange(currentTotal, currentWaitlisted, newTotal int) (heldAfter, waitlistedAfter, stockDelta int) {
	heldBefore := currentTotal - currentWaitlisted
	if heldBefore < 0 {
		// Linha já inconsistente (o bug acima produzia isso). Tratar como zero
		// segurado é o lado seguro: não devolve estoque que não foi tirado.
		heldBefore = 0
	}

	switch {
	case newTotal < heldBefore:
		// Reduziu abaixo do que segura: a fila zera e sobra estoque a devolver.
		heldAfter = newTotal
	case newTotal > currentTotal:
		// Aumentou: o acréscimo é segurado.
		heldAfter = heldBefore + (newTotal - currentTotal)
	default:
		// Reduziu, mas ainda acima do segurado: só encolhe a fila.
		heldAfter = heldBefore
	}

	waitlistedAfter = newTotal - heldAfter
	if waitlistedAfter < 0 {
		waitlistedAfter = 0
	}
	return heldAfter, waitlistedAfter, heldAfter - heldBefore
}
