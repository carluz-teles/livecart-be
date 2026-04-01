package checkout

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration"
	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
)

// Service handles business logic for public checkout.
type Service struct {
	repo               *Repository
	integrationService *integration.Service
	logger             *zap.Logger
}

// NewService creates a new checkout service.
func NewService(
	repo *Repository,
	integrationService *integration.Service,
	logger *zap.Logger,
) *Service {
	return &Service{
		repo:               repo,
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

	// Convert to output
	output := &GetCartForCheckoutOutput{
		Cart: CartDetails{
			ID:             cart.ID,
			EventID:        cart.EventID,
			PlatformUserID: cart.PlatformUserID,
			PlatformHandle: cart.PlatformHandle,
			Token:          cart.Token,
			Status:         cart.Status,
			CheckoutURL:    cart.CheckoutURL,
			CheckoutID:     cart.CheckoutID,
			CustomerEmail:  cart.CustomerEmail,
			PaymentStatus:  cart.PaymentStatus,
			PaidAt:         cart.PaidAt,
			CreatedAt:      cart.CreatedAt,
			ExpiresAt:      cart.ExpiresAt,
			EventTitle:     cart.EventTitle,
			StoreID:        cart.StoreID,
			StoreName:      cart.StoreName,
			StoreLogoURL:   cart.StoreLogoURL,
			AllowEdit:      cart.AllowEdit,
		},
		Items: make([]CartItemDetails, len(items)),
	}

	for i, item := range items {
		output.Items[i] = CartItemDetails{
			ID:         item.ID,
			CartID:     item.CartID,
			ProductID:  item.ProductID,
			Quantity:   item.Quantity,
			UnitPrice:  item.UnitPrice,
			Waitlisted: item.Waitlisted,
			Name:       item.Name,
			ImageURL:   item.ImageURL,
			Keyword:    item.Keyword,
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
	if cart.Status != "checkout" {
		return nil, httpx.ErrUnprocessable("carrinho ainda não foi finalizado. Aguarde o fim do evento.")
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

	// Filter out waitlisted items and build checkout items
	var checkoutItems []providers.CheckoutItem
	var totalAmount int64
	for _, item := range items {
		if item.Waitlisted {
			continue // Skip waitlisted items
		}
		checkoutItems = append(checkoutItems, providers.CheckoutItem{
			ID:        item.ProductID,
			Name:      item.Name,
			Quantity:  item.Quantity,
			UnitPrice: item.UnitPrice,
			ImageURL:  derefString(item.ImageURL),
		})
		totalAmount += item.UnitPrice * int64(item.Quantity)
	}

	if len(checkoutItems) == 0 {
		return nil, httpx.ErrUnprocessable("carrinho não tem itens disponíveis para pagamento")
	}

	// Get payment integration for the store
	paymentIntegration, err := s.repo.GetPaymentIntegration(ctx, cart.StoreID)
	if err != nil {
		return nil, err
	}
	if paymentIntegration == nil {
		return nil, httpx.ErrUnprocessable("loja não possui integração de pagamento configurada")
	}

	// Get checkout expiry hours from store settings
	expiryHours, _ := s.repo.GetStoreCheckoutExpiryHours(ctx, cart.StoreID)
	expiresAt := GetExpiresAt(expiryHours)

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
