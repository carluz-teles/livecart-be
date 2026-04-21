package notification

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// RenderTemplate replaces template variables with actual values.
// Variables are in the format {variable_name}.
func RenderTemplate(template string, vars TemplateVariables) string {
	replacements := map[string]string{
		"{handle}":      vars.Handle,
		"{produto}":     vars.Produto,
		"{keyword}":     vars.Keyword,
		"{quantidade}":  fmt.Sprintf("%d", vars.Quantidade),
		"{total_itens}": fmt.Sprintf("%d", vars.TotalItens),
		"{total}":       vars.Total,
		"{link}":        vars.Link,
		"{loja}":        vars.Loja,
		"{expira_em}":   vars.ExpiraEm,
		"{live_titulo}": vars.LiveTitulo,
	}

	result := template
	for placeholder, value := range replacements {
		result = strings.ReplaceAll(result, placeholder, value)
	}

	return result
}

// FormatCurrency formats cents as Brazilian Real currency string.
func FormatCurrency(cents int64) string {
	reais := float64(cents) / 100
	return fmt.Sprintf("R$ %.2f", reais)
}

// FormatExpiry formats expiry hours as a human-readable string.
func FormatExpiry(hours int) string {
	if hours == 1 {
		return "1 hora"
	}
	if hours < 24 {
		return fmt.Sprintf("%d horas", hours)
	}
	days := hours / 24
	if days == 1 {
		return "1 dia"
	}
	return fmt.Sprintf("%d dias", days)
}

// FormatExpiryMinutes formats expiry minutes as a human-readable string.
func FormatExpiryMinutes(minutes int) string {
	if minutes < 60 {
		if minutes == 1 {
			return "1 minuto"
		}
		return fmt.Sprintf("%d minutos", minutes)
	}
	hours := minutes / 60
	if hours == 1 {
		return "1 hora"
	}
	if hours < 24 {
		return fmt.Sprintf("%d horas", hours)
	}
	days := hours / 24
	if days == 1 {
		return "1 dia"
	}
	return fmt.Sprintf("%d dias", days)
}

// ValidateTemplate checks if a template is valid and within limits.
// Returns the rendered length and any validation errors.
func ValidateTemplate(template string, sampleVars TemplateVariables) (int, error) {
	rendered := RenderTemplate(template, sampleVars)
	byteLen := len(rendered)

	if byteLen > MaxMessageBytes {
		return byteLen, fmt.Errorf("mensagem muito longa: %d bytes (máximo: %d)", byteLen, MaxMessageBytes)
	}

	if byteLen == 0 {
		return 0, fmt.Errorf("template não pode estar vazio")
	}

	return byteLen, nil
}

// TruncateMessage truncates a message to fit within the byte limit.
// It ensures we don't cut in the middle of a UTF-8 character.
func TruncateMessage(message string, maxBytes int) string {
	if len(message) <= maxBytes {
		return message
	}

	// Find a safe truncation point
	truncated := message[:maxBytes]

	// Ensure we don't cut in the middle of a UTF-8 character
	for !utf8.ValidString(truncated) && len(truncated) > 0 {
		truncated = truncated[:len(truncated)-1]
	}

	// Add ellipsis if we truncated
	if len(truncated) < len(message) {
		// Make room for "..."
		for len(truncated) > maxBytes-3 {
			_, size := utf8.DecodeLastRuneInString(truncated)
			truncated = truncated[:len(truncated)-size]
		}
		truncated += "..."
	}

	return truncated
}

// GetAvailableVariables returns a list of all available template variables.
func GetAvailableVariables() []string {
	return []string{
		"{handle}",
		"{produto}",
		"{keyword}",
		"{quantidade}",
		"{total_itens}",
		"{total}",
		"{link}",
		"{loja}",
		"{expira_em}",
		"{live_titulo}",
	}
}

// SampleVariables returns sample data for template preview.
func SampleVariables() TemplateVariables {
	return TemplateVariables{
		Handle:       "@cliente_exemplo",
		Produto:      "Camiseta Preta M",
		Keyword:      "ABCD",
		Quantidade:   2,
		TotalItens:   3,
		Total:        "R$ 199,90",
		TotalCents:   19990,
		Link:         "https://sualoja.com/cart/abc123",
		Loja:         "Minha Loja",
		ExpiraEm:     "48 horas",
		LiveTitulo:   "Black Friday 2024",
	}
}
