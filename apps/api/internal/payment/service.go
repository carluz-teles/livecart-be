// Package payment owns payment-provider resolution and the stateless payment
// fast-paths for integrations. It is the strangler-fig extraction of the
// payment domain out of the internal/integration monolith: B1a moved provider
// resolution (GetProvider); B1b moved the stateless fast-paths (CreateCheckout,
// GetPaymentStatus, RefundPayment, GetCheckoutConfig, ProcessCardPayment,
// GeneratePixPayment). The former CRUD service over the vestigial `payments`
// table (dead code, 0 imports) has been removed and replaced by this package.
package payment

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/config"
	"livecart/apps/api/lib/httpx"
	"livecart/apps/api/lib/idempotency"
	"livecart/apps/api/lib/logger"
)

// Service resolves payment providers and runs the stateless payment fast-paths
// for integrations.
type Service struct {
	resolver    IntegrationResolver
	idempotency *idempotency.Service
	logger      *zap.Logger

	// gateway backs the payment webhook consumer (ProcessPaymentNotification /
	// DispatchPaymentProcess, B1d). It is wired at boot via SetCartPaymentGateway
	// and is implemented by *integration.Service so the consumer runs against the
	// SAME integration.Repository — no second repo, no second advisory lock.
	gateway CartPaymentGateway
}

// NewService builds the payment Service.
//
// resolver is implemented by integration.Service, which owns the shared
// provider-construction and provider-error-handling logic. idempotency and
// logger are injected directly (both leaf libs) so the fast-paths keep the
// exact behaviour they had inside integration.Service.
func NewService(resolver IntegrationResolver, idempotency *idempotency.Service, logger *zap.Logger) *Service {
	return &Service{
		resolver:    resolver,
		idempotency: idempotency,
		logger:      logger,
	}
}

// GetProvider returns a PaymentProvider for the given integration.
//
// It mirrors the former integration.Service.GetPaymentProvider exactly: fetch
// the integration, reject it if it is not a payment integration, build the
// provider, then cast it to providers.PaymentProvider — surfacing the same
// httpx.ErrUnprocessable errors on the not-a-payment and cast-failure paths.
func (s *Service) GetProvider(ctx context.Context, integrationID, storeID string) (providers.PaymentProvider, error) {
	integrationType, build, err := s.resolver.ResolveIntegration(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	if integrationType != string(providers.ProviderTypePayment) {
		return nil, httpx.ErrUnprocessable("integration is not a payment provider")
	}

	provider, err := build()
	if err != nil {
		return nil, err
	}

	paymentProvider, ok := provider.(providers.PaymentProvider)
	if !ok {
		return nil, httpx.ErrUnprocessable("failed to cast to payment provider")
	}

	return paymentProvider, nil
}

// CreateCheckout creates a hosted checkout with the store's payment provider.
// It is idempotent on input.IdempotencyKey: a matching prior response is
// replayed from the idempotency store instead of hitting the provider again.
func (s *Service) CreateCheckout(ctx context.Context, input CreateCheckoutInput) (*CreateCheckoutOutput, error) {
	// Check idempotency
	idemReq := idempotency.CheckRequest{
		IdempotencyKey: input.IdempotencyKey,
		StoreID:        input.StoreID,
		IntegrationID:  input.IntegrationID,
		Operation:      "create_checkout",
		Payload:        input,
	}

	cached, err := s.idempotency.Check(ctx, idemReq)
	if err != nil {
		logger.From(ctx, s.logger).Warn("idempotency check failed", zap.Error(err))
	}
	if cached != nil && cached.Found {
		var output CreateCheckoutOutput
		if err := json.Unmarshal(cached.Response, &output); err == nil {
			logger.From(ctx, s.logger).Debug("returning cached checkout response",
				zap.String("idempotency_key", input.IdempotencyKey),
			)
			return &output, nil
		}
	}

	// Start idempotency tracking
	var idemRecord *idempotency.Record
	if input.IdempotencyKey != "" || s.idempotency != nil {
		idemRecord, err = s.idempotency.Start(ctx, idemReq)
		if err != nil {
			logger.From(ctx, s.logger).Warn("idempotency start failed", zap.Error(err))
		}
	}

	// Get payment provider
	paymentProvider, err := s.GetProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		if idemRecord != nil {
			_ = s.idempotency.Fail(ctx, idemRecord.ID, err)
		}
		return nil, err
	}

	// Build notify URL
	notifyURL := input.NotifyURL
	if notifyURL == "" {
		baseURL := config.WebhookBaseURL.String()
		if baseURL != "" {
			notifyURL = fmt.Sprintf("%s/api/webhooks/%s/%s",
				baseURL,
				paymentProvider.Name(),
				input.StoreID,
			)
		}
	}

	// Create checkout
	result, err := paymentProvider.CreateCheckout(ctx, providers.CheckoutOrder{
		ExternalID:  input.CartID,
		Items:       input.Items,
		Customer:    input.Customer,
		TotalAmount: input.TotalAmount,
		Currency:    input.Currency,
		NotifyURL:   notifyURL,
		SuccessURL:  input.SuccessURL,
		FailureURL:  input.FailureURL,
		Metadata:    input.Metadata,
	})
	if err != nil {
		s.resolver.HandleProviderError(ctx, input.IntegrationID, "create_checkout", err)
		if idemRecord != nil {
			_ = s.idempotency.Fail(ctx, idemRecord.ID, err)
		}
		return nil, httpx.InfrastructureError(err, "create_checkout")
	}

	output := &CreateCheckoutOutput{
		CheckoutID:  result.CheckoutID,
		CheckoutURL: result.CheckoutURL,
		ExpiresAt:   result.ExpiresAt,
	}

	// Complete idempotency
	if idemRecord != nil {
		_ = s.idempotency.Complete(ctx, idemRecord.ID, output)
	}

	return output, nil
}

// GetPaymentStatus retrieves the status of a payment.
func (s *Service) GetPaymentStatus(ctx context.Context, input GetPaymentStatusInput) (*GetPaymentStatusOutput, error) {
	paymentProvider, err := s.GetProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	status, err := paymentProvider.GetPaymentStatus(ctx, input.PaymentID)
	if err != nil {
		s.resolver.HandleProviderError(ctx, input.IntegrationID, "get_payment_status", err)
		return nil, httpx.InfrastructureError(err, "get_payment_status")
	}

	return &GetPaymentStatusOutput{
		PaymentID:     status.PaymentID,
		Status:        string(status.Status),
		Amount:        status.Amount,
		PaidAt:        status.PaidAt,
		RefundedAt:    status.RefundedAt,
		FailureReason: status.FailureReason,
		Metadata:      status.Metadata,
	}, nil
}

// RefundPayment initiates a refund.
func (s *Service) RefundPayment(ctx context.Context, input RefundPaymentInput) (*RefundPaymentOutput, error) {
	paymentProvider, err := s.GetProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	result, err := paymentProvider.RefundPayment(ctx, input.PaymentID, input.Amount)
	if err != nil {
		s.resolver.HandleProviderError(ctx, input.IntegrationID, "refund_payment", err)
		return nil, httpx.InfrastructureError(err, "refund_payment")
	}

	return &RefundPaymentOutput{
		RefundID:  result.RefundID,
		Status:    result.Status,
		Amount:    result.Amount,
		CreatedAt: result.CreatedAt,
	}, nil
}

// GetCheckoutConfig retrieves the transparent-checkout configuration (public
// key + supported payment methods) for a store.
func (s *Service) GetCheckoutConfig(ctx context.Context, integrationID, storeID string) (string, []string, error) {
	paymentProvider, err := s.GetProvider(ctx, integrationID, storeID)
	if err != nil {
		return "", nil, err
	}

	publicKey, err := paymentProvider.GetPublicKey(ctx)
	if err != nil {
		s.resolver.HandleProviderError(ctx, integrationID, "get_public_key", err)
		return "", nil, httpx.InfrastructureError(err, "get_public_key")
	}

	methods, err := paymentProvider.GetPaymentMethods(ctx)
	if err != nil {
		s.resolver.HandleProviderError(ctx, integrationID, "get_payment_methods", err)
		return "", nil, httpx.InfrastructureError(err, "get_payment_methods")
	}

	return publicKey, methods, nil
}

// ProcessCardPayment processes a card payment with a tokenized card.
func (s *Service) ProcessCardPayment(ctx context.Context, input ProcessCardPaymentInput) (*ProcessCardPaymentOutput, error) {
	paymentProvider, err := s.GetProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	result, err := paymentProvider.ProcessCardPayment(ctx, providers.CardPaymentInput{
		CartID:          input.CartID,
		Token:           input.CardToken,
		Installments:    input.Installments,
		Customer:        input.Customer,
		Items:           input.Items,
		TotalAmount:     input.TotalAmount,
		Currency:        input.Currency,
		NotifyURL:       input.NotifyURL,
		Metadata:        input.Metadata,
		PaymentMethodID: input.PaymentMethodID,
		IssuerID:        input.IssuerID,
		DeviceID:        input.DeviceID,
	})
	if err != nil {
		s.resolver.HandleProviderError(ctx, input.IntegrationID, "process_card_payment", err)
		return nil, httpx.InfrastructureError(err, "process_card_payment")
	}

	return &ProcessCardPaymentOutput{
		PaymentID:         result.PaymentID,
		Status:            string(result.Status),
		StatusDetail:      result.StatusDetail,
		Message:           result.Message,
		Amount:            result.Amount,
		Installments:      result.Installments,
		LastFourDigits:    result.LastFourDigits,
		CardBrand:         result.CardBrand,
		AuthorizationCode: result.AuthorizationCode,
		PaidAt:            result.PaidAt,
	}, nil
}

// GeneratePixPayment generates a PIX QR code for payment.
func (s *Service) GeneratePixPayment(ctx context.Context, input GeneratePixPaymentInput) (*GeneratePixPaymentOutput, error) {
	paymentProvider, err := s.GetProvider(ctx, input.IntegrationID, input.StoreID)
	if err != nil {
		return nil, err
	}

	result, err := paymentProvider.GeneratePixPayment(ctx, providers.PixPaymentInput{
		CartID:      input.CartID,
		Customer:    input.Customer,
		Items:       input.Items,
		TotalAmount: input.TotalAmount,
		Currency:    input.Currency,
		NotifyURL:   input.NotifyURL,
		Metadata:    input.Metadata,
	})
	if err != nil {
		s.resolver.HandleProviderError(ctx, input.IntegrationID, "generate_pix_payment", err)
		return nil, httpx.InfrastructureError(err, "generate_pix_payment")
	}

	return &GeneratePixPaymentOutput{
		PaymentID:  result.PaymentID,
		CancelID:   result.CancelID,
		QRCode:     result.QRCode,
		QRCodeText: result.QRCodeText,
		Amount:     result.Amount,
		ExpiresAt:  result.ExpiresAt,
		TicketURL:  result.TicketURL,
	}, nil
}

// CancelPixPayment invalida no gateway uma cobranca PIX ainda nao paga.
//
// Best-effort por contrato: o gateway recusa quando a cobranca ja foi paga, e
// essa recusa e o comportamento correto — nunca queremos apagar um pagamento
// legitimo. Quem chama trata o erro como "nao deu para invalidar" e segue.
func (s *Service) CancelPixPayment(ctx context.Context, integrationID, storeID, cancelID string) error {
	if cancelID == "" {
		return nil
	}
	paymentProvider, err := s.GetProvider(ctx, integrationID, storeID)
	if err != nil {
		return err
	}
	if err := paymentProvider.CancelPixPayment(ctx, cancelID); err != nil {
		s.resolver.HandleProviderError(ctx, integrationID, "cancel_pix_payment", err)
		return err
	}
	return nil
}
