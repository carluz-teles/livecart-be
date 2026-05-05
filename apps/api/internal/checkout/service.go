package checkout

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
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
}

// Service handles business logic for public checkout.
type Service struct {
	repo               *Repository
	pool               *pgxpool.Pool
	integrationService *integration.Service
	couponLifecycle    CouponLifecycle
	logger             *zap.Logger
}

// NewService creates a new checkout service.
func NewService(
	repo *Repository,
	pool *pgxpool.Pool,
	integrationService *integration.Service,
	logger *zap.Logger,
) *Service {
	return &Service{
		repo:               repo,
		pool:               pool,
		integrationService: integrationService,
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

// GetCartForCheckout retrieves a cart for the public checkout page.
func (s *Service) GetCartForCheckout(ctx context.Context, input GetCartForCheckoutInput) (*GetCartForCheckoutOutput, error) {
	// Get cart with event/store info
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}

	// Validate cart status (allow viewing paid carts so frontend can show "paid" message)
	if cart.Status == "expired" {
		return nil, httpx.ErrUnprocessable("carrinho expirado")
	}
	// Note: paid carts are allowed - frontend will show a "paid" dialog

	// Freeze the initial cart on the very first GET (idempotent — subsequent
	// calls are no-ops). Failure here is non-fatal: the buyer must still be
	// able to pay even if snapshotting blew up; the upsell card just won't
	// have a baseline to compare against.
	if cart.PaymentStatus != "paid" {
		if err := s.repo.EnsureInitialSnapshot(ctx, cart.ID); err != nil {
			s.logger.Warn("failed to snapshot initial cart",
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

	// Load shipping selection (may be nil if not chosen yet)
	shippingSel, err := s.repo.ReadCartShipping(ctx, s.pool, cart.ID)
	if err != nil {
		return nil, err
	}

	// Convert to output
	output := &GetCartForCheckoutOutput{
		Cart: CartDetails{
			ID:                 cart.ID,
			EventID:            cart.EventID,
			PlatformUserID:     cart.PlatformUserID,
			PlatformHandle:     cart.PlatformHandle,
			Token:              cart.Token,
			Status:             cart.Status,
			CheckoutURL:        cart.CheckoutURL,
			CheckoutID:         cart.CheckoutID,
			CustomerEmail:      cart.CustomerEmail,
			PaymentStatus:      cart.PaymentStatus,
			PaidAt:             cart.PaidAt,
			CreatedAt:          cart.CreatedAt,
			ExpiresAt:          cart.ExpiresAt,
			EventTitle:         cart.EventTitle,
			EventFreeShipping:  cart.EventFreeShipping,
			StoreID:            cart.StoreID,
			StoreName:          cart.StoreName,
			StoreLogoURL:       cart.StoreLogoURL,
			AllowEdit:           cart.AllowEdit,
			MaxQuantityPerItem:  cart.MaxQuantityPerItem,
			Shipping:            shippingSel,
			CouponID:            cart.CouponID,
			CouponCode:          cart.CouponCode,
			CouponDiscountCents: cart.CouponDiscountCents,
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
			s.logger.Warn("failed to read paid-cart receipt details",
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
			s.logger.Warn("failed to read returning-buyer snapshot",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		} else if customer != nil || address != nil {
			output.Customer = customer
			output.ShippingAddress = address
			output.Cart.IsReturningCustomer = true
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

	// Validate cart status
	if cart.Status == "expired" {
		return nil, httpx.ErrUnprocessable("carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.ErrUnprocessable("carrinho já foi pago")
	}
	// Allow checkout for both 'active' (live ongoing) and 'checkout' (live ended) status
	if cart.Status != "checkout" && cart.Status != "active" {
		return nil, httpx.ErrUnprocessable("carrinho não está disponível para checkout")
	}

	// Update customer email
	if err := s.repo.UpdateCustomerEmail(ctx, input.Token, input.Email); err != nil {
		return nil, err
	}

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
		return nil, httpx.ErrUnprocessable("carrinho não tem itens disponíveis para pagamento")
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

	// Get payment integration for the store
	paymentIntegration, err := s.repo.GetPaymentIntegration(ctx, cart.StoreID)
	if err != nil {
		return nil, err
	}
	if paymentIntegration == nil {
		return nil, httpx.ErrUnprocessable("loja não possui integração de pagamento configurada")
	}

	// Get cart expiration minutes from store settings
	expirationMinutes, _ := s.repo.GetStoreCartExpirationMinutes(ctx, cart.StoreID)
	expiresAt := GetExpiresAtMinutes(expirationMinutes)

	// Build URLs
	baseURL := config.WebhookBaseURL.String()
	successURL := fmt.Sprintf("%s/cart/%s?status=success", baseURL, cart.Token)
	failureURL := fmt.Sprintf("%s/cart/%s?status=failure", baseURL, cart.Token)

	// Create checkout via integration service
	checkoutResult, err := s.integrationService.CreateCheckout(ctx, integration.CreateCheckoutInput{
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
		s.logger.Error("failed to create checkout",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("erro ao gerar link de pagamento. Tente novamente.")
	}

	// Update cart with checkout info
	if err := s.repo.UpdateCheckoutInfo(ctx, UpdateCheckoutParams{
		CartID:            cart.ID,
		CheckoutURL:       checkoutResult.CheckoutURL,
		CheckoutID:        checkoutResult.CheckoutID,
		CheckoutExpiresAt: expiresAt,
	}); err != nil {
		s.logger.Error("failed to update checkout info",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
		// Don't fail - we already have the checkout URL
	}

	s.logger.Info("checkout generated",
		zap.String("cart_id", cart.ID),
		zap.String("checkout_id", checkoutResult.CheckoutID),
		zap.Int64("total_amount", totalAmount),
	)

	return &GenerateCheckoutOutput{
		CheckoutURL: checkoutResult.CheckoutURL,
		ExpiresAt:   expiresAt,
	}, nil
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

	// Validate cart status
	if cart.Status == "expired" {
		return nil, httpx.ErrUnprocessable("carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.ErrUnprocessable("carrinho já foi pago")
	}
	// Allow checkout for both 'active' (live ongoing) and 'checkout' (live ended) status
	if cart.Status != "checkout" && cart.Status != "active" {
		return nil, httpx.ErrUnprocessable("carrinho não está disponível para checkout")
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
		return nil, httpx.ErrUnprocessable("carrinho não tem itens disponíveis para pagamento")
	}

	// Add selected shipping cost to the total for the gateway config.
	if shippingSel, _ := s.repo.ReadCartShipping(ctx, s.pool, cart.ID); shippingSel != nil {
		totalAmount += shippingSel.CostCents
	}
	totalAmount = applyCouponDiscount(totalAmount, cart.CouponDiscountCents)

	// Get payment integration for the store
	paymentIntegration, err := s.repo.GetPaymentIntegration(ctx, cart.StoreID)
	if err != nil {
		return nil, err
	}
	if paymentIntegration == nil {
		return nil, httpx.ErrUnprocessable("loja não possui integração de pagamento configurada")
	}

	// Get public key and payment methods from integration
	publicKey, methods, err := s.integrationService.GetCheckoutConfig(ctx, paymentIntegration.ID.String(), cart.StoreID)
	if err != nil {
		s.logger.Error("failed to get checkout config",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("erro ao obter configuração de pagamento")
	}

	return &GetCheckoutConfigOutput{
		Provider:         paymentIntegration.ProviderName,
		PublicKey:        publicKey,
		AvailableMethods: methods,
		TotalAmount:      totalAmount,
		Currency:         "BRL",
	}, nil
}

// ProcessCardPayment processes a card payment with a tokenized card.
func (s *Service) ProcessCardPayment(ctx context.Context, input ProcessCardPaymentInput) (*ProcessCardPaymentOutput, error) {
	// Get cart
	cart, err := s.repo.GetCartByToken(ctx, input.Token)
	if err != nil {
		return nil, err
	}

	// Validate cart status
	if cart.Status == "expired" {
		return nil, httpx.ErrUnprocessable("carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.ErrUnprocessable("carrinho já foi pago")
	}
	// Allow checkout for both 'active' (live ongoing) and 'checkout' (live ended) status
	if cart.Status != "checkout" && cart.Status != "active" {
		return nil, httpx.ErrUnprocessable("carrinho não está disponível para checkout")
	}

	// Persist customer identity + shipping address before requesting payment —
	// the webhook handler will read this when creating the paid order in the ERP.
	if err := s.repo.UpdateCheckoutCustomer(ctx, cart.ID, input.Email, input.CustomerName, input.CustomerDocument, input.CustomerPhone, input.ShippingAddress); err != nil {
		return nil, err
	}

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
		return nil, httpx.ErrUnprocessable("carrinho não tem itens disponíveis para pagamento")
	}

	// Add selected shipping cost to the total charged at the gateway.
	if shippingSel, _ := s.repo.ReadCartShipping(ctx, s.pool, cart.ID); shippingSel != nil {
		totalAmount += shippingSel.CostCents
	}
	totalAmount = applyCouponDiscount(totalAmount, cart.CouponDiscountCents)

	// Get payment integration
	paymentIntegration, err := s.repo.GetPaymentIntegration(ctx, cart.StoreID)
	if err != nil {
		return nil, err
	}
	if paymentIntegration == nil {
		return nil, httpx.ErrUnprocessable("loja não possui integração de pagamento configurada")
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
	result, err := s.integrationService.ProcessCardPayment(ctx, integration.ProcessCardPaymentInput{
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
		Metadata: map[string]any{
			"cart_id":    cart.ID,
			"cart_token": cart.Token,
			"event_id":   cart.EventID,
			"store_id":   cart.StoreID,
		},
	})
	if err != nil {
		s.logger.Error("failed to process card payment",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("erro ao processar pagamento")
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
			s.logger.Error("failed to update payment status",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		}
		// Best-effort capture of card details for the post-payment receipt
		// (public checkout `payment` block). Failure here must not break the
		// happy path — the comprovante just renders without these fields.
		if err := s.repo.WriteCartCardPayment(ctx, s.pool, cart.ID, result.CardBrand, result.LastFourDigits, result.Installments, result.AuthorizationCode); err != nil {
			s.logger.Warn("failed to persist card payment details",
				zap.String("cart_id", cart.ID),
				zap.Error(err),
			)
		}
	}

	s.logger.Info("card payment processed",
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

	// Validate cart status
	if cart.Status == "expired" {
		return nil, httpx.ErrUnprocessable("carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.ErrUnprocessable("carrinho já foi pago")
	}
	// Allow checkout for both 'active' (live ongoing) and 'checkout' (live ended) status
	if cart.Status != "checkout" && cart.Status != "active" {
		return nil, httpx.ErrUnprocessable("carrinho não está disponível para checkout")
	}

	// Persist customer identity + shipping address before requesting payment —
	// the webhook handler will read this when creating the paid order in the ERP.
	if err := s.repo.UpdateCheckoutCustomer(ctx, cart.ID, input.Email, input.CustomerName, input.CustomerDocument, input.CustomerPhone, input.ShippingAddress); err != nil {
		return nil, err
	}

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
		return nil, httpx.ErrUnprocessable("carrinho não tem itens disponíveis para pagamento")
	}

	// Add selected shipping cost to the total charged at the gateway.
	if shippingSel, _ := s.repo.ReadCartShipping(ctx, s.pool, cart.ID); shippingSel != nil {
		totalAmount += shippingSel.CostCents
	}
	totalAmount = applyCouponDiscount(totalAmount, cart.CouponDiscountCents)

	// Get payment integration
	paymentIntegration, err := s.repo.GetPaymentIntegration(ctx, cart.StoreID)
	if err != nil {
		return nil, err
	}
	if paymentIntegration == nil {
		return nil, httpx.ErrUnprocessable("loja não possui integração de pagamento configurada")
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
	result, err := s.integrationService.GeneratePixPayment(ctx, integration.GeneratePixPaymentInput{
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
		s.logger.Error("failed to generate pix",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
		return nil, httpx.ErrUnprocessable("erro ao gerar PIX")
	}

	// Update cart with payment ID for tracking
	if err := s.repo.UpdateCheckoutInfo(ctx, UpdateCheckoutParams{
		CartID:     cart.ID,
		CheckoutID: result.PaymentID,
	}); err != nil {
		s.logger.Error("failed to update checkout info",
			zap.String("cart_id", cart.ID),
			zap.Error(err),
		)
	}

	s.logger.Info("pix payment generated",
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
	if input.Quantity < 1 {
		return nil, httpx.ErrUnprocessable("quantidade deve ser pelo menos 1")
	}

	delta := input.Quantity - item.Quantity
	if delta == 0 {
		return s.GetCartForCheckout(ctx, GetCartForCheckoutInput{Token: input.Token})
	}

	if delta > 0 {
		if err := s.validateQuantityCap(ctx, cart, item.ProductID, input.Quantity); err != nil {
			return nil, err
		}
	}

	if err := s.repo.EnsureInitialSnapshot(ctx, cart.ID); err != nil {
		return nil, err
	}
	if err := s.repo.SetCartItemQuantity(ctx, item.ID, input.Quantity); err != nil {
		return nil, err
	}

	movementID, syncErr := s.integrationService.AdjustStockReservationDelta(
		ctx, cart.StoreID, cart.ID, cart.EventID, item.ProductID,
		delta, item.UnitPrice, cart.PlatformHandle,
	)
	if syncErr != nil {
		// Roll back the local change so the buyer sees the failure clearly.
		_ = s.repo.SetCartItemQuantity(ctx, item.ID, item.Quantity)
		s.logger.Error("ERP delta sync failed, rolled back cart item quantity",
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
	if err := s.repo.RecordMutation(ctx, MutationParams{
		CartID:         cart.ID,
		ProductID:      item.ProductID,
		MutationType:   mutationType,
		QuantityBefore: item.Quantity,
		QuantityAfter:  input.Quantity,
		UnitPrice:      item.UnitPrice,
		ERPMovementID:  movementID,
	}); err != nil {
		s.logger.Warn("cart item quantity changed but mutation log write failed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", item.ProductID),
			zap.Error(err),
		)
	}

	return s.GetCartForCheckout(ctx, GetCartForCheckoutInput{Token: input.Token})
}

// RemoveCartItem deletes an item entirely and reverses the ERP reservation.
func (s *Service) RemoveCartItem(ctx context.Context, input MutateCartItemInput) (*GetCartForCheckoutOutput, error) {
	cart, item, err := s.loadEditableCartItem(ctx, input.Token, input.ItemID)
	if err != nil {
		return nil, err
	}

	if err := s.repo.EnsureInitialSnapshot(ctx, cart.ID); err != nil {
		return nil, err
	}
	if err := s.repo.DeleteCartItem(ctx, item.ID); err != nil {
		return nil, err
	}

	movementID, syncErr := s.integrationService.AdjustStockReservationDelta(
		ctx, cart.StoreID, cart.ID, cart.EventID, item.ProductID,
		-item.Quantity, item.UnitPrice, cart.PlatformHandle,
	)
	if syncErr != nil {
		// Re-create the row at the original quantity to keep state consistent.
		if _, restoreErr := s.repo.CreateCartItem(ctx, cart.ID, item.ProductID, item.Quantity, item.UnitPrice); restoreErr != nil {
			s.logger.Error("failed to restore cart item after ERP failure — manual intervention needed",
				zap.String("cart_id", cart.ID),
				zap.String("product_id", item.ProductID),
				zap.Error(restoreErr),
			)
		}
		s.logger.Error("ERP reversal failed on remove, restored cart item",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", item.ProductID),
			zap.Error(syncErr),
		)
		return nil, httpx.ErrUnprocessable("não foi possível remover o item, tente novamente")
	}

	if err := s.repo.RecordMutation(ctx, MutationParams{
		CartID:         cart.ID,
		ProductID:      item.ProductID,
		MutationType:   "item_removed",
		QuantityBefore: item.Quantity,
		QuantityAfter:  0,
		UnitPrice:      item.UnitPrice,
		ERPMovementID:  movementID,
	}); err != nil {
		s.logger.Warn("item removed but mutation log write failed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", item.ProductID),
			zap.Error(err),
		)
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
	if cfg.Stock > 0 && desiredQty > cfg.Stock {
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
		input.Quantity, cfg.UnitPrice, cart.PlatformHandle,
	)
	if syncErr != nil {
		// Roll back. With existing != nil, restore previous qty; with new row, delete it.
		if existing != nil {
			_ = s.repo.SetCartItemQuantity(ctx, existing.ID, currentQty)
		} else if rollback, _ := s.repo.FindCartItemByProduct(ctx, cart.ID, input.ProductID); rollback != nil {
			_ = s.repo.DeleteCartItem(ctx, rollback.ID)
		}
		return nil, httpx.ErrUnprocessable("não foi possível adicionar o produto, tente novamente")
	}

	mutationType := "item_added"
	if existing != nil {
		mutationType = "quantity_increased"
	}
	if err := s.repo.RecordMutation(ctx, MutationParams{
		CartID:         cart.ID,
		ProductID:      input.ProductID,
		MutationType:   mutationType,
		QuantityBefore: currentQty,
		QuantityAfter:  desiredQty,
		UnitPrice:      cfg.UnitPrice,
		ERPMovementID:  movementID,
	}); err != nil {
		s.logger.Warn("cart item added but mutation log write failed",
			zap.String("cart_id", cart.ID),
			zap.String("product_id", input.ProductID),
			zap.Error(err),
		)
	}

	return s.GetCartForCheckout(ctx, GetCartForCheckoutInput{Token: input.Token})
}

// loadEditableCart asserts the cart is in a state that accepts mutations.
func (s *Service) loadEditableCart(ctx context.Context, token string) (*CartRow, error) {
	cart, err := s.repo.GetCartByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if cart.Status == "expired" {
		return nil, httpx.ErrUnprocessable("carrinho expirado")
	}
	if cart.PaymentStatus == "paid" {
		return nil, httpx.ErrConflict("carrinho já foi pago")
	}
	if !cart.AllowEdit {
		return nil, httpx.ErrConflict("edição do carrinho desabilitada para esta loja")
	}
	if cart.ExpiresAt != nil && cart.ExpiresAt.Before(time.Now()) {
		return nil, httpx.ErrUnprocessable("carrinho expirado")
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

// validateQuantityCap re-checks per-item caps + product stock for an increase.
func (s *Service) validateQuantityCap(ctx context.Context, cart *CartRow, productID string, desiredQty int) error {
	cfg, err := s.repo.GetEventProductForCart(ctx, cart.EventID, cart.StoreID, productID)
	if err != nil {
		return err
	}
	if cfg.MaxQuantity > 0 && desiredQty > cfg.MaxQuantity {
		return httpx.ErrUnprocessable(fmt.Sprintf("limite de %d por item", cfg.MaxQuantity))
	}
	if cfg.Stock > 0 && desiredQty > cfg.Stock {
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
