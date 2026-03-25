package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"
)

// Client handles email sending via SendGrid
type Client struct {
	apiKey    string
	fromEmail string
	fromName  string
	logger    *zap.Logger
	client    *http.Client
}

// NewClient creates a new SendGrid email client
func NewClient(logger *zap.Logger) *Client {
	return &Client{
		apiKey:    os.Getenv("SENDGRID_API_KEY"),
		fromEmail: getEnvOrDefault("SENDGRID_FROM_EMAIL", "noreply@livecart.com"),
		fromName:  getEnvOrDefault("SENDGRID_FROM_NAME", "LiveCart"),
		logger:    logger.Named("email"),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// IsConfigured checks if SendGrid is properly configured
func (c *Client) IsConfigured() bool {
	return c.apiKey != ""
}

// InvitationEmailInput contains data for invitation email
type InvitationEmailInput struct {
	ToEmail     string
	ToName      string
	StoreName   string
	InviterName string
	Role        string
	AcceptURL   string
	ExpiresAt   time.Time
}

// SendInvitation sends an invitation email
func (c *Client) SendInvitation(ctx context.Context, input InvitationEmailInput) error {
	if !c.IsConfigured() {
		c.logger.Warn("SendGrid not configured, skipping email",
			zap.String("to", input.ToEmail),
			zap.String("store", input.StoreName),
		)
		return nil
	}

	subject := fmt.Sprintf("Você foi convidado para %s no LiveCart", input.StoreName)
	htmlContent, err := c.renderInvitationHTML(input)
	if err != nil {
		return fmt.Errorf("rendering email template: %w", err)
	}

	textContent := c.renderInvitationText(input)

	return c.send(ctx, SendEmailInput{
		ToEmail:     input.ToEmail,
		ToName:      input.ToName,
		Subject:     subject,
		HTMLContent: htmlContent,
		TextContent: textContent,
	})
}

// SendEmailInput contains data for a generic email
type SendEmailInput struct {
	ToEmail     string
	ToName      string
	Subject     string
	HTMLContent string
	TextContent string
}

// send sends an email via SendGrid API
func (c *Client) send(ctx context.Context, input SendEmailInput) error {
	payload := map[string]interface{}{
		"personalizations": []map[string]interface{}{
			{
				"to": []map[string]string{
					{"email": input.ToEmail, "name": input.ToName},
				},
			},
		},
		"from": map[string]string{
			"email": c.fromEmail,
			"name":  c.fromName,
		},
		"subject": input.Subject,
		"content": []map[string]string{
			{"type": "text/plain", "value": input.TextContent},
			{"type": "text/html", "value": input.HTMLContent},
		},
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling email payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.sendgrid.com/v3/mail/send", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending email: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var errorBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errorBody)
		c.logger.Error("SendGrid API error",
			zap.Int("status", resp.StatusCode),
			zap.Any("error", errorBody),
		)
		return fmt.Errorf("sendgrid returned status %d", resp.StatusCode)
	}

	c.logger.Info("email sent successfully",
		zap.String("to", input.ToEmail),
		zap.String("subject", input.Subject),
	)

	return nil
}

func (c *Client) renderInvitationHTML(input InvitationEmailInput) (string, error) {
	tmpl := `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Convite LiveCart</title>
</head>
<body style="margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif; background-color: #f5f5f5;">
    <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="background-color: #f5f5f5;">
        <tr>
            <td style="padding: 40px 20px;">
                <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%" style="max-width: 600px; margin: 0 auto; background-color: #ffffff; border-radius: 8px; box-shadow: 0 2px 4px rgba(0,0,0,0.1);">
                    <!-- Header -->
                    <tr>
                        <td style="padding: 40px 40px 20px; text-align: center;">
                            <h1 style="margin: 0; color: #1a1a1a; font-size: 24px; font-weight: 600;">
                                Você foi convidado!
                            </h1>
                        </td>
                    </tr>
                    <!-- Content -->
                    <tr>
                        <td style="padding: 20px 40px;">
                            <p style="margin: 0 0 20px; color: #4a4a4a; font-size: 16px; line-height: 1.5;">
                                Olá,
                            </p>
                            <p style="margin: 0 0 20px; color: #4a4a4a; font-size: 16px; line-height: 1.5;">
                                <strong>{{.InviterName}}</strong> convidou você para fazer parte da equipe
                                <strong>{{.StoreName}}</strong> no LiveCart como <strong>{{.RoleDisplay}}</strong>.
                            </p>
                            <p style="margin: 0 0 30px; color: #4a4a4a; font-size: 16px; line-height: 1.5;">
                                Clique no botão abaixo para aceitar o convite e começar a colaborar.
                            </p>
                            <!-- CTA Button -->
                            <table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%">
                                <tr>
                                    <td style="text-align: center;">
                                        <a href="{{.AcceptURL}}" style="display: inline-block; padding: 14px 32px; background-color: #f59e0b; color: #ffffff; text-decoration: none; font-size: 16px; font-weight: 600; border-radius: 6px;">
                                            Aceitar Convite
                                        </a>
                                    </td>
                                </tr>
                            </table>
                        </td>
                    </tr>
                    <!-- Footer -->
                    <tr>
                        <td style="padding: 30px 40px 40px;">
                            <p style="margin: 0 0 10px; color: #9a9a9a; font-size: 14px; line-height: 1.5;">
                                Este convite expira em <strong>{{.ExpiresIn}}</strong>.
                            </p>
                            <p style="margin: 0; color: #9a9a9a; font-size: 14px; line-height: 1.5;">
                                Se você não esperava este email, pode ignorá-lo com segurança.
                            </p>
                        </td>
                    </tr>
                    <!-- Brand Footer -->
                    <tr>
                        <td style="padding: 20px 40px; border-top: 1px solid #eaeaea; text-align: center;">
                            <p style="margin: 0; color: #9a9a9a; font-size: 12px;">
                                LiveCart - Venda mais nas suas lives
                            </p>
                        </td>
                    </tr>
                </table>
            </td>
        </tr>
    </table>
</body>
</html>`

	t, err := template.New("invitation").Parse(tmpl)
	if err != nil {
		return "", err
	}

	roleDisplay := "Membro"
	if input.Role == "admin" {
		roleDisplay = "Administrador"
	}

	expiresIn := "7 dias"
	if !input.ExpiresAt.IsZero() {
		hoursUntilExpiry := time.Until(input.ExpiresAt).Hours()
		if hoursUntilExpiry < 24 {
			expiresIn = fmt.Sprintf("%.0f horas", hoursUntilExpiry)
		} else {
			expiresIn = fmt.Sprintf("%.0f dias", hoursUntilExpiry/24)
		}
	}

	data := struct {
		InviterName string
		StoreName   string
		RoleDisplay string
		AcceptURL   string
		ExpiresIn   string
	}{
		InviterName: input.InviterName,
		StoreName:   input.StoreName,
		RoleDisplay: roleDisplay,
		AcceptURL:   input.AcceptURL,
		ExpiresIn:   expiresIn,
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func (c *Client) renderInvitationText(input InvitationEmailInput) string {
	roleDisplay := "Membro"
	if input.Role == "admin" {
		roleDisplay = "Administrador"
	}

	return fmt.Sprintf(`Olá,

%s convidou você para fazer parte da equipe %s no LiveCart como %s.

Clique no link abaixo para aceitar o convite:
%s

Este convite expira em 7 dias.

Se você não esperava este email, pode ignorá-lo com segurança.

---
LiveCart - Venda mais nas suas lives
`, input.InviterName, input.StoreName, roleDisplay, input.AcceptURL)
}
