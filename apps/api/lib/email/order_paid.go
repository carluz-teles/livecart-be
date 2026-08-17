package email

import (
	"bytes"
	"context"
	"fmt"
	"html/template"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// OrderPaidEmailInput is the data the postcheckout service hands us when a
// cart transitions to paid. Keep it flat — Resend doesn't care, and a flat
// shape keeps the template readable.
type OrderPaidEmailInput struct {
	// Store branding
	StoreName    string
	StoreLogoURL string

	// Customer / order
	ToEmail        string
	ToName         string
	OrderShortID   string // formatted human id (e.g. "A8K2" or "#1283")
	TotalFormatted string // "R$ 247,80"
	ItemsHTML      string // pre-rendered <tr>...</tr> rows for the items table
	ShippingLine   string // "Rua X, 123 — São Paulo/SP" (single-line summary)
	CarrierLine    string // "Sedex via Melhor Envio" (optional)

	// Public tracking page
	TrackingURL string

	// Merchant overrides — when set, replace the body block inside the shell
	// (default header/footer stay). Subject overrides the auto-generated one.
	OverrideSubject  string
	OverrideBodyHTML string

	// Reply-to: e-mail de contato da loja (suporte)
	ReplyTo string

	// Audit context (optional) — links the notification_logs row to the
	// store/cart/event. Callers without these ids can leave them empty.
	StoreID string
	CartID  string
	EventID string
}

// SendOrderPaid sends the "Pagamento confirmado" email. Best-effort: returns
// nil silently if Resend is not configured so a missing API key never blocks
// the payment webhook from acknowledging.
//
// When OverrideBodyHTML is set, the merchant-customized body replaces the
// type-specific layout and is wrapped in a neutral shell. Subject defaults
// to "Pedido #X confirmado · Store" when OverrideSubject is empty.
func (c *Client) SendOrderPaid(ctx context.Context, input OrderPaidEmailInput) error {
	subject := input.OverrideSubject
	if subject == "" {
		subject = fmt.Sprintf("Pedido #%s confirmado · %s", input.OrderShortID, input.StoreName)
	}
	audit := AuditEntry{
		StoreID: input.StoreID,
		CartID:  input.CartID,
		EventID: input.EventID,
		Kind:    "order_paid",
		ToEmail: input.ToEmail,
		Subject: subject,
	}

	if !c.IsConfigured() {
		logger.From(ctx, c.logger).Warn("Resend not configured, skipping order paid email",
			zap.String("to", input.ToEmail),
			zap.String("order", input.OrderShortID),
		)
		c.auditSkipped(ctx, audit)
		return nil
	}

	var htmlContent string
	var err error
	if input.OverrideBodyHTML != "" {
		htmlContent, err = RenderOverrideShell(ShellInput{
			StoreName:    input.StoreName,
			StoreLogoURL: input.StoreLogoURL,
			BodyHTML:     template.HTML(input.OverrideBodyHTML),
		})
	} else {
		htmlContent, err = c.renderOrderPaidHTML(input)
	}
	if err != nil {
		c.auditFailed(ctx, audit, err)
		return fmt.Errorf("rendering order paid html: %w", err)
	}
	textContent := c.renderOrderPaidText(input)

	return c.send(ctx, SendEmailInput{
		ToEmail:     input.ToEmail,
		ToName:      input.ToName,
		Subject:     subject,
		HTMLContent: htmlContent,
		TextContent: textContent,
		ReplyTo:     input.ReplyTo,
		FromName:    input.StoreName,
		Audit:       audit,
	})
}

func (c *Client) renderOrderPaidHTML(input OrderPaidEmailInput) (string, error) {
	t, err := template.New("order_paid").Parse(orderPaidShellHTML)
	if err != nil {
		return "", err
	}

	type tplData struct {
		OrderPaidEmailInput
		ItemsHTML template.HTML
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, tplData{
		OrderPaidEmailInput: input,
		ItemsHTML:           template.HTML(input.ItemsHTML),
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (c *Client) renderOrderPaidText(input OrderPaidEmailInput) string {
	return fmt.Sprintf(`Olá %s,

Recebemos seu pagamento. Seu pedido #%s foi confirmado e está sendo preparado.

Total: %s

Acompanhe o pedido: %s

%s
`, input.ToName, input.OrderShortID, input.TotalFormatted, input.TrackingURL, input.StoreName)
}

// Shell + body for the "Pagamento confirmado" email. Inline styles only —
// table-based layout for broad email-client compatibility (Outlook, Gmail web,
// iOS Mail, etc.). Keep palette neutral and brand-light: the customer is
// receiving from "their store", not from "LiveCart".
const orderPaidShellHTML = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Pedido confirmado</title>
</head>
<body style="margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif; background-color: #f5f5f5; color: #1a1a1a;">
    <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="background-color: #f5f5f5;">
        <tr>
            <td style="padding: 40px 20px;">
                <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="max-width: 600px; margin: 0 auto; background-color: #ffffff; border-radius: 12px; box-shadow: 0 2px 6px rgba(0,0,0,0.06);">
                    <!-- Header -->
                    <tr>
                        <td style="padding: 32px 40px 16px; text-align: center; border-bottom: 1px solid #f0f0f0;">
                            {{if .StoreLogoURL}}
                            <img src="{{.StoreLogoURL}}" alt="{{.StoreName}}" width="56" height="56" style="display: inline-block; border-radius: 12px; object-fit: cover;" />
                            {{end}}
                            <p style="margin: 12px 0 0; color: #6b7280; font-size: 13px; letter-spacing: 0.04em; text-transform: uppercase;">
                                {{.StoreName}}
                            </p>
                        </td>
                    </tr>

                    <!-- Title -->
                    <tr>
                        <td style="padding: 32px 40px 8px;">
                            <p style="margin: 0; color: #10b981; font-size: 13px; font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase;">
                                ✓ Pagamento confirmado
                            </p>
                            <h1 style="margin: 8px 0 0; color: #111827; font-size: 24px; font-weight: 600; letter-spacing: -0.01em;">
                                Pedido #{{.OrderShortID}} confirmado
                            </h1>
                        </td>
                    </tr>

                    <!-- Greeting -->
                    <tr>
                        <td style="padding: 16px 40px 0;">
                            <p style="margin: 0; color: #4b5563; font-size: 16px; line-height: 1.5;">
                                Olá {{.ToName}}, recebemos seu pagamento. Seu pedido foi confirmado e
                                está sendo preparado para envio. A gente te avisa por email cada vez que
                                o pedido andar uma etapa.
                            </p>
                        </td>
                    </tr>

                    <!-- Items -->
                    <tr>
                        <td style="padding: 28px 40px 0;">
                            <p style="margin: 0 0 12px; color: #6b7280; font-size: 13px; font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase;">
                                Itens do pedido
                            </p>
                            <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="border-collapse: collapse;">
                                {{.ItemsHTML}}
                                <tr>
                                    <td colspan="2" style="padding: 12px 0 0; border-top: 1px solid #f0f0f0;">
                                        <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%">
                                            <tr>
                                                <td style="color: #111827; font-size: 16px; font-weight: 600;">Total</td>
                                                <td style="color: #111827; font-size: 16px; font-weight: 600; text-align: right;">{{.TotalFormatted}}</td>
                                            </tr>
                                        </table>
                                    </td>
                                </tr>
                            </table>
                        </td>
                    </tr>

                    <!-- Shipping -->
                    {{if .ShippingLine}}
                    <tr>
                        <td style="padding: 28px 40px 0;">
                            <p style="margin: 0 0 8px; color: #6b7280; font-size: 13px; font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase;">
                                Endereço de entrega
                            </p>
                            <p style="margin: 0; color: #1f2937; font-size: 15px; line-height: 1.5;">
                                {{.ShippingLine}}
                            </p>
                            {{if .CarrierLine}}
                            <p style="margin: 6px 0 0; color: #6b7280; font-size: 13px;">
                                {{.CarrierLine}}
                            </p>
                            {{end}}
                        </td>
                    </tr>
                    {{end}}

                    <!-- CTA -->
                    <tr>
                        <td style="padding: 32px 40px 8px;">
                            <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%">
                                <tr>
                                    <td style="text-align: center;">
                                        <a href="{{.TrackingURL}}" style="display: inline-block; padding: 14px 28px; background-color: #111827; color: #ffffff; text-decoration: none; font-size: 15px; font-weight: 600; border-radius: 10px;">
                                            Acompanhar pedido →
                                        </a>
                                    </td>
                                </tr>
                            </table>
                            <p style="margin: 16px 0 0; color: #9ca3af; font-size: 12px; text-align: center; word-break: break-all;">
                                Ou copie este link: {{.TrackingURL}}
                            </p>
                        </td>
                    </tr>

                    <!-- Footer -->
                    <tr>
                        <td style="padding: 32px 40px; border-top: 1px solid #f0f0f0;">
                            <p style="margin: 0; color: #9ca3af; font-size: 12px; line-height: 1.5; text-align: center;">
                                Você recebeu este email porque comprou em {{.StoreName}}.
                            </p>
                            <p style="margin: 6px 0 0; color: #d1d5db; font-size: 11px; text-align: center;">
                                Powered by LiveCart
                            </p>
                        </td>
                    </tr>
                </table>
            </td>
        </tr>
    </table>
</body>
</html>`
