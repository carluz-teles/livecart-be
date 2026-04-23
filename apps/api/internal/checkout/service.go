package checkout

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
)

// Service handles business logic for public checkout.
type Service struct {
	repo               *Repository
	pool               *pgxpool.Pool
	integrationService *integration.Service
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

// GetCartForCheckout retrieves a cart for the public checkout page.
func (s *Service) GetCartForCheckout(ctx context.Context, input GetCartForCheckoutInput) (*GetCartForCheckoutOutput, error) {
	// Get cart with event/store info
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
			AllowEdit:          cart.AllowEdit,
			MaxQuantityPerItem: cart.MaxQuantityPerItem,
			Shipping:           shippingSel,
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

	// Build notify URL
	notifyURL := fmt.Sprintf("%s/api/v1/integrations/webhooks/%s/%s",
		config.WebhookBaseURL.String(),
		paymentIntegration.ProviderName,
		paymentIntegration.ID.String(),
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

	// Update cart payment status if approved
	if result.Status == "approved" {
		if err := s.repo.UpdatePaymentStatus(ctx, cart.ID, "paid", result.PaymentID); err != nil {
			s.logger.Error("failed to update payment status",
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
		PaymentID:      result.PaymentID,
		Status:         result.Status,
		StatusDetail:   result.StatusDetail,
		Message:        result.Message,
		Amount:         result.Amount,
		Installments:   result.Installments,
		LastFourDigits: result.LastFourDigits,
		CardBrand:      result.CardBrand,
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

	// Build notify URL
	notifyURL := fmt.Sprintf("%s/api/v1/integrations/webhooks/%s/%s",
		config.WebhookBaseURL.String(),
		paymentIntegration.ProviderName,
		paymentIntegration.ID.String(),
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
