package email

import (
	"context"
	"fmt"
	"html/template"

	"go.uber.org/zap"

	"livecart/apps/api/lib/logger"
)

// OrderRefundedEmailInput carries the data for the "Pedido estornado" email.
type OrderRefundedEmailInput struct {
	StoreName    string
	StoreLogoURL string

	ToEmail        string
	ToName         string
	OrderShortID   string
	TotalFormatted string // valor estornado

	// PaymentMethod ajusta a expectativa do prazo: "pix" volta rápido,
	// cartão pode levar até 2 faturas.
	PaymentMethod string

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

// SendOrderRefunded notifies the customer that the payment was refunded.
func (c *Client) SendOrderRefunded(ctx context.Context, input OrderRefundedEmailInput) error {
	subject := input.OverrideSubject
	if subject == "" {
		subject = fmt.Sprintf("Reembolso do pedido #%s · %s", input.OrderShortID, input.StoreName)
	}
	audit := AuditEntry{
		StoreID: input.StoreID,
		CartID:  input.CartID,
		EventID: input.EventID,
		Kind:    "order_refunded",
		ToEmail: input.ToEmail,
		Subject: subject,
	}

	if !c.IsConfigured() {
		logger.From(ctx, c.logger).Warn("Resend not configured, skipping order refunded email",
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
		htmlContent, err = c.renderOrderRefundedHTML(input)
	}
	if err != nil {
		c.auditFailed(ctx, audit, err)
		return fmt.Errorf("rendering order refunded html: %w", err)
	}

	return c.send(ctx, SendEmailInput{
		ToEmail:     input.ToEmail,
		ToName:      input.ToName,
		Subject:     subject,
		HTMLContent: htmlContent,
		TextContent: c.renderOrderRefundedText(input),
		ReplyTo:     input.ReplyTo,
		FromName:    input.StoreName,
		Audit:       audit,
	})
}

func refundTimeline(paymentMethod string) string {
	if paymentMethod == "pix" {
		return "Como o pagamento foi via PIX, o valor costuma voltar pra sua conta em instantes — no máximo em alguns dias úteis."
	}
	return "Como o pagamento foi no cartão, o estorno aparece na fatura em até 2 ciclos, dependendo do banco emissor."
}

func (c *Client) renderOrderRefundedHTML(input OrderRefundedEmailInput) (string, error) {
	body := fmt.Sprintf(`
		<h2 style="margin:0 0 16px;font-size:20px;color:#111827;">Reembolso confirmado ✔</h2>
		<p style="margin:0 0 12px;font-size:15px;line-height:1.6;color:#374151;">
			Olá %s, o valor de <strong>%s</strong> do pedido #%s na %s foi estornado.
		</p>
		<p style="margin:0 0 12px;font-size:15px;line-height:1.6;color:#374151;">%s</p>
		<p style="margin:24px 0 0;font-size:13px;line-height:1.6;color:#6b7280;">
			Qualquer dúvida sobre o reembolso, é só responder este e-mail que a %s te ajuda.
		</p>`,
		template.HTMLEscapeString(displayName(input.ToName)),
		template.HTMLEscapeString(input.TotalFormatted),
		template.HTMLEscapeString(input.OrderShortID),
		template.HTMLEscapeString(input.StoreName),
		template.HTMLEscapeString(refundTimeline(input.PaymentMethod)),
		template.HTMLEscapeString(input.StoreName),
	)

	return RenderOverrideShell(ShellInput{
		StoreName:    input.StoreName,
		StoreLogoURL: input.StoreLogoURL,
		BodyHTML:     template.HTML(body),
	})
}

func (c *Client) renderOrderRefundedText(input OrderRefundedEmailInput) string {
	return fmt.Sprintf(
		"Reembolso confirmado\n\nOlá %s, o valor de %s do pedido #%s na %s foi estornado.\n%s\n\nDúvidas? Responda este e-mail.",
		displayName(input.ToName), input.TotalFormatted, input.OrderShortID, input.StoreName,
		refundTimeline(input.PaymentMethod),
	)
}
