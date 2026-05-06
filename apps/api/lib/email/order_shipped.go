package email

import (
	"bytes"
	"context"
	"fmt"
	"html/template"

	"go.uber.org/zap"
)

// OrderShippedEmailInput is the payload for the "Pedido enviado" email.
// Carrier line is optional — some shipments only have a tracking_code (raw
// string) without a friendly carrier name.
type OrderShippedEmailInput struct {
	StoreName    string
	StoreLogoURL string

	ToEmail      string
	ToName       string
	OrderShortID string

	TrackingCode string
	CarrierLine  string

	TrackingURL string // public order page on our app
}

// SendOrderShipped notifies the customer that the merchant has dispatched the
// order. Carries the tracking_code prominently so the customer can paste it
// into the carrier's site if they want carrier-side tracking detail.
func (c *Client) SendOrderShipped(ctx context.Context, input OrderShippedEmailInput) error {
	if !c.IsConfigured() {
		c.logger.Warn("Resend not configured, skipping order shipped email",
			zap.String("to", input.ToEmail),
			zap.String("order", input.OrderShortID),
		)
		return nil
	}

	subject := fmt.Sprintf("Pedido #%s a caminho · %s", input.OrderShortID, input.StoreName)

	htmlContent, err := c.renderOrderShippedHTML(input)
	if err != nil {
		return fmt.Errorf("rendering order shipped html: %w", err)
	}
	textContent := c.renderOrderShippedText(input)

	return c.send(ctx, SendEmailInput{
		ToEmail:     input.ToEmail,
		ToName:      input.ToName,
		Subject:     subject,
		HTMLContent: htmlContent,
		TextContent: textContent,
	})
}

func (c *Client) renderOrderShippedHTML(input OrderShippedEmailInput) (string, error) {
	t, err := template.New("order_shipped").Parse(orderShippedShellHTML)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, input); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (c *Client) renderOrderShippedText(input OrderShippedEmailInput) string {
	carrier := input.CarrierLine
	if carrier == "" {
		carrier = "Transportadora"
	}
	return fmt.Sprintf(`Olá %s,

Boas notícias! Seu pedido #%s acabou de ser enviado.

%s
Código de rastreio: %s

Acompanhe o pedido: %s

%s
`, input.ToName, input.OrderShortID, carrier, input.TrackingCode, input.TrackingURL, input.StoreName)
}

const orderShippedShellHTML = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Pedido enviado</title>
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
                            <p style="margin: 0; color: #3b82f6; font-size: 13px; font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase;">
                                ✈ Pedido enviado
                            </p>
                            <h1 style="margin: 8px 0 0; color: #111827; font-size: 24px; font-weight: 600; letter-spacing: -0.01em;">
                                Pedido #{{.OrderShortID}} a caminho
                            </h1>
                        </td>
                    </tr>

                    <!-- Greeting -->
                    <tr>
                        <td style="padding: 16px 40px 0;">
                            <p style="margin: 0; color: #4b5563; font-size: 16px; line-height: 1.5;">
                                Olá {{.ToName}}, seu pedido acabou de ser despachado.
                                {{if .CarrierLine}}A entrega vai ser feita por <strong>{{.CarrierLine}}</strong>.{{end}}
                            </p>
                        </td>
                    </tr>

                    <!-- Tracking code box -->
                    <tr>
                        <td style="padding: 28px 40px 0;">
                            <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="background-color: #f9fafb; border: 1px solid #e5e7eb; border-radius: 12px;">
                                <tr>
                                    <td style="padding: 16px 20px;">
                                        <p style="margin: 0 0 4px; color: #6b7280; font-size: 11px; font-weight: 600; letter-spacing: 0.08em; text-transform: uppercase;">
                                            Código de rastreio
                                        </p>
                                        <p style="margin: 0; color: #111827; font-family: 'SF Mono', 'Roboto Mono', Menlo, monospace; font-size: 18px; font-weight: 600; letter-spacing: 0.02em;">
                                            {{.TrackingCode}}
                                        </p>
                                    </td>
                                </tr>
                            </table>
                            <p style="margin: 10px 0 0; color: #9ca3af; font-size: 12px;">
                                Cole esse código no site da transportadora pra ver detalhes da rota.
                            </p>
                        </td>
                    </tr>

                    <!-- CTA -->
                    <tr>
                        <td style="padding: 28px 40px 8px;">
                            <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%">
                                <tr>
                                    <td style="text-align: center;">
                                        <a href="{{.TrackingURL}}" style="display: inline-block; padding: 14px 28px; background-color: #111827; color: #ffffff; text-decoration: none; font-size: 15px; font-weight: 600; border-radius: 10px;">
                                            Ver detalhes do pedido →
                                        </a>
                                    </td>
                                </tr>
                            </table>
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
