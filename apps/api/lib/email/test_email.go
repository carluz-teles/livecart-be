package email

import (
	"context"
	"fmt"
	"html/template"

	"go.uber.org/zap"

	"livecart/apps/api/internal/notification"
)

// SendTestEmail wraps the merchant's draft body in the override shell and
// ships it via Resend. Used by the dashboard's "Testar envio" button so the
// lojista can preview a full render in their inbox before saving.
//
// This signature satisfies notification.EmailSender — the notification
// package defines an interface to avoid importing this package directly.
func (c *Client) SendTestEmail(ctx context.Context, input notification.EmailTestSendInput) error {
	subject := prefixTestSubject(input.Subject)
	audit := AuditEntry{
		StoreID: input.StoreID,
		Kind:    "test_email",
		ToEmail: input.ToEmail,
		Subject: subject,
	}

	if !c.IsConfigured() {
		c.logger.Warn("Resend not configured, skipping test email",
			zap.String("to", input.ToEmail),
		)
		c.auditSkipped(ctx, audit)
		return fmt.Errorf("email service is not configured")
	}

	// Body fallback when the merchant only customized the subject: a short
	// placeholder paragraph so the test email isn't visually empty.
	body := input.BodyHTML
	if body == "" {
		body = `<p>Esta é uma prévia de teste. O corpo do email aparece aqui quando você customiza o template.</p>`
	}

	htmlContent, err := RenderOverrideShell(ShellInput{
		StoreName:    input.StoreName,
		StoreLogoURL: input.StoreLogoURL,
		BodyHTML:     template.HTML(body),
	})
	if err != nil {
		c.auditFailed(ctx, audit, err)
		return fmt.Errorf("rendering test email html: %w", err)
	}

	return c.send(ctx, SendEmailInput{
		ToEmail:     input.ToEmail,
		Subject:     subject,
		HTMLContent: htmlContent,
		TextContent: stripHTML(body),
		Audit:       audit,
	})
}

// prefixTestSubject tags every test send so the inbox makes the "this is a
// preview, not a real receipt" affordance obvious.
func prefixTestSubject(s string) string {
	if s == "" {
		return "[Teste] Prévia de email"
	}
	return "[Teste] " + s
}

// stripHTML is a primitive plain-text fallback for clients that prefer text.
// Email clients render HTML by default, so this is rarely seen — keeping it
// minimal avoids a fully-featured HTML→text dependency.
func stripHTML(html string) string {
	out := make([]byte, 0, len(html))
	inTag := false
	for i := 0; i < len(html); i++ {
		ch := html[i]
		switch ch {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				out = append(out, ch)
			}
		}
	}
	return string(out)
}
