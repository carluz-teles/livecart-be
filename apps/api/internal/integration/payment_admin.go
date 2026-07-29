package integration

import (
	"context"

	"livecart/apps/api/internal/payment"
)

// paymentAdmin adapts *Service to payment.PagarmeAdminService so the extracted
// payment.Handler (Bloco B1c) can drive the Pagar.me admin endpoints whose
// service logic still lives in integration.Service.
//
// integration already imports payment (for the fast-path delegations), so the
// adapter lives here — the payment package never imports integration, keeping
// the dependency graph acyclic. It reuses toIntegrationResponse and the
// existing service methods verbatim; no logic is duplicated, only translated
// across the package boundary.
type paymentAdmin struct {
	svc *Service
}

// NewPaymentAdmin wraps the integration Service as the Pagar.me admin backend
// for the payment.Handler.
func NewPaymentAdmin(svc *Service) payment.PagarmeAdminService {
	return &paymentAdmin{svc: svc}
}

func (a *paymentAdmin) ConnectPagarme(ctx context.Context, in payment.ConnectPagarmeInput) (any, error) {
	out, err := a.svc.ConnectPagarme(ctx, ConnectPagarmeInput{
		StoreID:         in.StoreID,
		SecretKey:       in.SecretKey,
		PublicKey:       in.PublicKey,
		WebhookUsername: in.WebhookUsername,
		WebhookPassword: in.WebhookPassword,
	})
	if err != nil {
		return nil, err
	}
	return toIntegrationResponse(out), nil
}

func (a *paymentAdmin) GetPagarmeWebhookStatus(ctx context.Context, integrationID, storeID string) (*payment.PagarmeWebhookStatusResponse, error) {
	out, err := a.svc.GetPagarmeWebhookStatus(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	resp := &payment.PagarmeWebhookStatusResponse{
		ExpectedURL:        out.ExpectedURL,
		Configured:         out.Configured,
		MatchCount:         out.MatchCount,
		LastDeliveryStatus: out.LastDeliveryStatus,
		LastResponseStatus: out.LastResponseStatus,
		LastEvent:          out.LastEvent,
	}
	if !out.LastDeliveryAt.IsZero() {
		t := out.LastDeliveryAt
		resp.LastDeliveryAt = &t
	}
	return resp, nil
}

func (a *paymentAdmin) TestPagarmeWebhook(ctx context.Context, integrationID, storeID string) (*payment.PagarmeWebhookTestResponse, error) {
	out, err := a.svc.TestPagarmeWebhookEndpoint(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	return &payment.PagarmeWebhookTestResponse{
		URL:            out.URL,
		Reachable:      out.Reachable,
		Healthy:        out.Healthy,
		HTTPStatus:     out.HTTPStatus,
		AuthConfigured: out.AuthConfigured,
		LatencyMs:      out.LatencyMs,
		Message:        out.Message,
	}, nil
}

func (a *paymentAdmin) RunPagarmeWebhookLiveTest(ctx context.Context, integrationID, storeID string) (*payment.PagarmeWebhookLiveTestResponse, error) {
	out, err := a.svc.RunPagarmeWebhookLiveTest(ctx, integrationID, storeID)
	if err != nil {
		return nil, err
	}

	return &payment.PagarmeWebhookLiveTestResponse{
		ExpectedURL:  out.ExpectedURL,
		OrderCode:    out.OrderCode,
		Delivered:    out.Delivered,
		Healthy:      out.Healthy,
		HTTPStatus:   out.HTTPStatus,
		Event:        out.Event,
		DeliveredURL: out.DeliveredURL,
		ResponseRaw:  out.ResponseRaw,
		Message:      out.Message,
	}, nil
}
