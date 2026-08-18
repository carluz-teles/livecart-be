package live

import (
	"regexp"
	"strconv"
	"strings"
)

// PurchaseIntent represents a detected purchase intent from a comment.
type PurchaseIntent struct {
	Quantity int    // Quantity requested (default 1)
	RawText  string // Original comment text
}

// =============================================================================
// KEYWORD-FIRST APPROACH
//
// The parser uses a two-stage strategy:
// 1. Extract a 4-char keyword (e.g., "1001") — if found, it's a purchase.
// 2. Extract quantity from surrounding context (e.g., "2x", "quero 3", "x2").
//
// This handles real-world Instagram live comment patterns:
//   "1001"           → 1x product 1001
//   "1001 2x"        → 2x product 1001
//   "2x 1001"        → 2x product 1001
//   "quero 2 1001"   → 2x product 1001
//   "manda 3 1001"   → 3x product 1001
//   "1001 quero 2"   → 2x product 1001
//   "quero 1001"     → 1x product 1001
// =============================================================================

// negativePatterns indicate the user is NOT buying.
var negativePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bn[aã]o\s+quero\b`),
	// `cancela` sozinho não pegava "cancelar", que é como as pessoas escrevem.
	// Na live de 17/08: "Gi, quero cancelar 1124 e colocar essa que vc mostrou
	// 1229" era lido como pedido DOS DOIS — inclusive o que ela estava
	// cancelando.
	regexp.MustCompile(`(?i)\bcancel\w*`),
	regexp.MustCompile(`(?i)\bdesisto\b`),
	regexp.MustCompile(`(?i)\bn[aã]o\s+preciso\b`),
}

// questionPatterns indicate the user is asking a question, not buying.
var questionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bquanto\s+custa\b`),
	regexp.MustCompile(`(?i)\bqual\s+o\s+pre[cç]o\b`),
	regexp.MustCompile(`(?i)\btem\s+desconto\b`),
	regexp.MustCompile(`(?i)\bainda\s+tem\b`),
	regexp.MustCompile(`(?i)\btem\s+em\s+estoque\b`),
	regexp.MustCompile(`(?i)\bquanto\s+[eé]\b`),
	regexp.MustCompile(`(?i)\bentrega\s+pra\s+onde\b`),
	regexp.MustCompile(`(?i)\baceita\s+pix\b`),
	regexp.MustCompile(`(?i)\bqual\s+o\s+tamanho\b`),
	regexp.MustCompile(`(?i)\btem\s+outras\s+cores\b`),
}

// ParsePurchaseIntent responde "é compra, e de quantas unidades no total?".
//
// Delega para ParsePurchaseItems. Antes tinha leitura PRÓPRIA do comentário — e
// era ela que rodava em produção, com os defeitos da live de 16/08: "1024x3"
// não casava com padrão nenhum, "1208 × 4" usava um sinal que ela não conhecia,
// e "valor 1000" virava pedido. Manter duas leituras do mesmo texto garantia
// que uma delas fosse a errada; quem chamasse esta continuaria com os defeitos
// depois de corrigidos na outra.
//
// A resposta é achatada em UMA quantidade porque é o que o chamador antigo
// sabe receber. Quem precisa saber QUAIS produtos usa ParsePurchaseItems.
func ParsePurchaseIntent(text string) *PurchaseIntent {
	return intentDoComentario(ParsePurchaseItems(text), text)
}

// IsCancellation checks if the text indicates a cancellation request.
func IsCancellation(text string) bool {
	cancellationPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bcancela\b`),
		regexp.MustCompile(`(?i)\bdesisto\b`),
		regexp.MustCompile(`(?i)\bn[aã]o\s+quero\s+mais\b`),
		regexp.MustCompile(`(?i)\btira\s+o\s+meu\b`),
		regexp.MustCompile(`(?i)\bremove\b`),
	}

	for _, pattern := range cancellationPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}

	return false
}

// keywordPattern matches 4-character alphanumeric codes.
var keywordPattern = regexp.MustCompile(`\b([A-Za-z0-9]{4})\b`)

// ExtractPossibleKeywords extracts all 4-character alphanumeric codes from text.
// Returns uppercase keywords for case-insensitive matching.
func ExtractPossibleKeywords(text string) []string {
	matches := keywordPattern.FindAllStringSubmatch(text, -1)
	if matches == nil {
		return nil
	}

	keywords := make([]string, 0, len(matches))
	seen := make(map[string]bool)

	for _, match := range matches {
		keyword := strings.ToUpper(match[1])
		if !isValidKeyword(keyword) {
			continue
		}
		if !seen[keyword] {
			seen[keyword] = true
			keywords = append(keywords, keyword)
		}
	}

	return keywords
}

// isValidKeyword diz se o trecho PODE ser o código de um produto.
//
// Espelha o value object do domínio, que é a autoridade:
//
//	// Keywords are 4-digit numeric strings between 1000-9999.
//	func NewKeyword(value string) (Keyword, error)  → product/domain/keyword.go
//
// Antes, aqui aceitava-se qualquer alfanumérico de 4 caracteres ("BONE",
// "A9B1"). Nenhum produto pode ter esses códigos — o domínio recusa na
// criação —, mas o parser os tratava como pedido, e toda palavra portuguesa de
// quatro letras virava código: "esse", "essa", "isso", "aqui", "amei", "acho".
//
// Sozinho isso seria inofensivo (código que não existe não acha produto). Junto
// com o fallback de produto em destaque, virava venda: "Esse cogumelo tem
// maior?" não achava produto ESSE, caía no destaque e criava o pedido que
// ninguém fez. Foi um dos casos da live de 16/08.
func isValidKeyword(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	// Fora de 1000-9999 nenhum produto existe: "0999" e "0000" são ruído.
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1000 && n <= 9999
}
