package email

import (
	"bytes"
	"context"
	"fmt"
	"html/template"

	"go.uber.org/zap"
)

// OrderDeliveredEmailInput is the payload for the final email in the
// post-purchase flow. Tone is a friendly thank-you with a soft nudge to come
// back — no pushy upsell, no review-bait.
type OrderDeliveredEmailInput struct {
	StoreName    string
	StoreLogoURL string

	ToEmail      string
	ToName       string
	OrderShortID string

	StoreURL string // optional — link back to the store homepage

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

func (c *Client) SendOrderDelivered(ctx context.Context, input OrderDeliveredEmailInput) error {
	subject := input.OverrideSubject
	if subject == "" {
		subject = fmt.Sprintf("Seu pedido #%s chegou · %s", input.OrderShortID, input.StoreName)
	}
	audit := AuditEntry{
		StoreID: input.StoreID,
		CartID:  input.CartID,
		EventID: input.EventID,
		Kind:    "order_delivered",
		ToEmail: input.ToEmail,
		Subject: subject,
	}

	if !c.IsConfigured() {
		c.logger.Warn("Resend not configured, skipping order delivered email",
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
		htmlContent, err = c.renderOrderDeliveredHTML(input)
	}
	if err != nil {
		c.auditFailed(ctx, audit, err)
		return fmt.Errorf("rendering order delivered html: %w", err)
	}
	textContent := c.renderOrderDeliveredText(input)

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

func (c *Client) renderOrderDeliveredHTML(input OrderDeliveredEmailInput) (string, error) {
	t, err := template.New("order_delivered").Parse(orderDeliveredShellHTML)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, input); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (c *Client) renderOrderDeliveredText(input OrderDeliveredEmailInput) string {
	return fmt.Sprintf(`Olá %s,

Seu pedido #%s foi entregue. Obrigado por comprar em %s!

Esperamos que goste da escolha. Se algo deu errado, é só responder este
email — a gente fica feliz em ajudar.

Até a próxima 💛
%s
`, input.ToName, input.OrderShortID, input.StoreName, input.StoreName)
}

const orderDeliveredShellHTML = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Pedido entregue</title>
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
                                ✓ Pedido entregue
                            </p>
                            <h1 style="margin: 8px 0 0; color: #111827; font-size: 24px; font-weight: 600; letter-spacing: -0.01em;">
                                Pedido #{{.OrderShortID}} chegou! 🎉
                            </h1>
                        </td>
                    </tr>

                    <!-- Greeting -->
                    <tr>
                        <td style="padding: 16px 40px 8px;">
                            <p style="margin: 0; color: #4b5563; font-size: 16px; line-height: 1.55;">
                                Olá {{.ToName}}, seu pedido foi entregue. Esperamos que tenha gostado da escolha.
                            </p>
                            <p style="margin: 16px 0 0; color: #4b5563; font-size: 16px; line-height: 1.55;">
                                Se algo deu errado, é só responder este email — a gente fica feliz em ajudar.
                            </p>
                        </td>
                    </tr>

                    {{if .StoreURL}}
                    <tr>
                        <td style="padding: 28px 40px 8px;">
                            <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%">
                                <tr>
                                    <td style="text-align: center;">
                                        <a href="{{.StoreURL}}" style="display: inline-block; padding: 12px 24px; background-color: #ffffff; color: #111827; text-decoration: none; font-size: 14px; font-weight: 600; border-radius: 10px; border: 1px solid #e5e7eb;">
                                            Voltar para a loja
                                        </a>
                                    </td>
                                </tr>
                            </table>
                        </td>
                    </tr>
                    {{end}}

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
