package email

import (
	"context"
	"fmt"
	"html/template"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// OrderCancelledEmailInput carries the data for the "Pedido cancelado" email.
type OrderCancelledEmailInput struct {
	StoreName    string
	StoreLogoURL string

	ToEmail        string
	ToName         string
	OrderShortID   string
	TotalFormatted string // "R$ 247,80" — valor do pedido cancelado

	// WasCharged distingue a copy: pedido cancelado ANTES do pagamento
	// (nada foi cobrado) vs. cobrança cancelada pelo gateway.
	WasCharged bool

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

// SendOrderCancelled notifies the customer that the order was cancelled.
// Best-effort like the other order emails.
func (c *Client) SendOrderCancelled(ctx context.Context, input OrderCancelledEmailInput) error {
	subject := input.OverrideSubject
	if subject == "" {
		subject = fmt.Sprintf("Pedido #%s cancelado · %s", input.OrderShortID, input.StoreName)
	}
	audit := AuditEntry{
		StoreID: input.StoreID,
		CartID:  input.CartID,
		EventID: input.EventID,
		Kind:    "order_cancelled",
		ToEmail: input.ToEmail,
		Subject: subject,
	}

	if !c.IsConfigured() {
		logger.From(ctx, c.logger).Warn("Resend not configured, skipping order cancelled email",
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
		htmlContent, err = c.renderOrderCancelledHTML(input)
	}
	if err != nil {
		c.auditFailed(ctx, audit, err)
		return fmt.Errorf("rendering order cancelled html: %w", err)
	}

	return c.send(ctx, SendEmailInput{
		ToEmail:     input.ToEmail,
		ToName:      input.ToName,
		Subject:     subject,
		HTMLContent: htmlContent,
		TextContent: c.renderOrderCancelledText(input),
		ReplyTo:     input.ReplyTo,
		FromName:    input.StoreName,
		Audit:       audit,
	})
}

func (c *Client) renderOrderCancelledHTML(input OrderCancelledEmailInput) (string, error) {
	chargedLine := "Nenhum valor foi cobrado."
	if input.WasCharged {
		chargedLine = "Se houve cobrança, o estorno será processado e você receberá a confirmação por e-mail."
	}

	body := fmt.Sprintf(`
		<h2 style="margin:0 0 16px;font-size:20px;color:#111827;">Pedido #%s cancelado</h2>
		<p style="margin:0 0 12px;font-size:15px;line-height:1.6;color:#374151;">
			Olá %s, seu pedido de <strong>%s</strong> na %s foi cancelado.
		</p>
		<p style="margin:0 0 12px;font-size:15px;line-height:1.6;color:#374151;">%s</p>
		<p style="margin:24px 0 0;font-size:13px;line-height:1.6;color:#6b7280;">
			Foi um engano ou ficou com alguma dúvida? É só responder este e-mail que a %s te ajuda.
		</p>`,
		template.HTMLEscapeString(input.OrderShortID),
		template.HTMLEscapeString(displayName(input.ToName)),
		template.HTMLEscapeString(input.TotalFormatted),
		template.HTMLEscapeString(input.StoreName),
		chargedLine,
		template.HTMLEscapeString(input.StoreName),
	)

	return RenderOverrideShell(ShellInput{
		StoreName:    input.StoreName,
		StoreLogoURL: input.StoreLogoURL,
		BodyHTML:     template.HTML(body),
	})
}

func (c *Client) renderOrderCancelledText(input OrderCancelledEmailInput) string {
	chargedLine := "Nenhum valor foi cobrado."
	if input.WasCharged {
		chargedLine = "Se houve cobrança, o estorno será processado em seguida."
	}
	return fmt.Sprintf(
		"Pedido #%s cancelado\n\nOlá %s, seu pedido de %s na %s foi cancelado.\n%s\n\nDúvidas? Responda este e-mail.",
		input.OrderShortID, displayName(input.ToName), input.TotalFormatted, input.StoreName, chargedLine,
	)
}

func displayName(name string) string {
	if name == "" || name == "cliente" {
		return "tudo bem"
	}
	return name
}
