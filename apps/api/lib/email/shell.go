package email

import (
	"bytes"
	"html/template"
)

// ShellInput is the data fed to the merchant-override shell. When the merchant
// has not overridden a template, the type-specific renderers (order_paid.go,
// order_shipped.go, order_delivered.go) handle the full layout themselves and
// don't go through this shell.
type ShellInput struct {
	StoreName    string
	StoreLogoURL string
	BodyHTML     template.HTML // post-substitution body, trusted markup
}

// RenderOverrideShell wraps the merchant's body in a neutral shell (logo +
// name in the header, store credit + LiveCart in the footer). The body is
// already-substituted HTML — do NOT call this with raw `{variable}` markers.
func RenderOverrideShell(input ShellInput) (string, error) {
	t, err := template.New("override_shell").Parse(overrideShellHTML)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, input); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// overrideShellHTML is intentionally lighter than the typed shells: just a
// header + body slot + footer, with conservative inline styles so what the
// merchant typed dominates the visual hierarchy.
const overrideShellHTML = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
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

                    <!-- Merchant body -->
                    <tr>
                        <td style="padding: 32px 40px;">
                            <div style="color: #1f2937; font-size: 16px; line-height: 1.6;">
                                {{.BodyHTML}}
                            </div>
                        </td>
                    </tr>

                    <!-- Footer -->
                    <tr>
                        <td style="padding: 24px 40px; border-top: 1px solid #f0f0f0;">
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
