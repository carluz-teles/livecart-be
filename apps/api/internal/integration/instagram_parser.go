package integration

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
	regexp.MustCompile(`(?i)\bcancela\b`),
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

// quantityPatterns extract quantity from text. Order matters: specific first.
var quantityPatterns = []*regexp.Regexp{
	// "2x", "3X" (multiplier notation)
	regexp.MustCompile(`(?i)\b(\d+)\s*x\b`),
	// "x2", "X3"
	regexp.MustCompile(`(?i)\bx\s*(\d+)\b`),
	// "quero N", "eu quero N"
	regexp.MustCompile(`(?i)\bquero\s+(\d+)\b`),
	// "reserva N"
	regexp.MustCompile(`(?i)\breserva\s+(\d+)\b`),
	// "manda N"
	regexp.MustCompile(`(?i)\bmanda\s+(\d+)\b`),
	// "separa N"
	regexp.MustCompile(`(?i)\bsepara\s+(\d+)\b`),
	// "pega N"
	regexp.MustCompile(`(?i)\bpega\s+(\d+)\b`),
	// "coloca N"
	regexp.MustCompile(`(?i)\bcoloca\s+(\d+)\b`),
	// "me manda N"
	regexp.MustCompile(`(?i)\bme\s+manda\s+(\d+)\b`),
	// "N unidade(s)"
	regexp.MustCompile(`(?i)\b(\d+)\s+unidades?\b`),
}

// ParsePurchaseIntent analyzes comment text and detects purchase intent.
// Uses keyword-first approach: if a 4-char keyword is present AND the comment
// isn't a question/negation, it's a purchase. Quantity defaults to 1.
func ParsePurchaseIntent(text string) *PurchaseIntent {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// Check for negative patterns first
	for _, pattern := range negativePatterns {
		if pattern.MatchString(text) {
			return nil
		}
	}

	// Check for question patterns
	for _, pattern := range questionPatterns {
		if pattern.MatchString(text) {
			return nil
		}
	}

	// Check if text contains a keyword (4-char alphanumeric code).
	// If it does, this IS a purchase intent — extract quantity.
	keywords := ExtractPossibleKeywords(text)
	if len(keywords) > 0 {
		quantity := extractQuantity(text)
		return &PurchaseIntent{
			Quantity: quantity,
			RawText:  text,
		}
	}

	// No keyword found. Fall back to explicit purchase verb patterns
	// like "quero", "manda", etc. (handles cases without a keyword in the text,
	// the keyword matching will happen later in findProductByKeyword).
	for _, pattern := range quantityPatterns {
		matches := pattern.FindStringSubmatch(text)
		if matches != nil {
			quantity := 1
			if len(matches) > 1 && matches[1] != "" {
				if q, err := strconv.Atoi(matches[1]); err == nil && q > 0 {
					quantity = q
				}
			}
			if quantity > 100 {
				quantity = 100
			}
			return &PurchaseIntent{
				Quantity: quantity,
				RawText:  text,
			}
		}
	}

	// "quero" / "eu quero" without number
	if regexp.MustCompile(`(?i)\b(eu\s+)?quero\b`).MatchString(text) {
		return &PurchaseIntent{
			Quantity: 1,
			RawText:  text,
		}
	}

	return nil
}

// extractQuantity finds a quantity number in the text using known patterns.
// Returns 1 if no quantity is found (default to 1 unit).
func extractQuantity(text string) int {
	for _, pattern := range quantityPatterns {
		matches := pattern.FindStringSubmatch(text)
		if matches != nil && len(matches) > 1 && matches[1] != "" {
			if q, err := strconv.Atoi(matches[1]); err == nil && q > 0 {
				if q > 100 {
					return 100
				}
				return q
			}
		}
	}

	// Look for a standalone small number (1-99) that isn't part of the keyword.
	// e.g., "1001 2" → quantity=2, "3 1001" → quantity=3
	standaloneNum := regexp.MustCompile(`\b(\d{1,2})\b`)
	for _, match := range standaloneNum.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(match[1]); err == nil && n > 0 && n <= 99 {
			// Make sure this isn't part of a keyword (4-char code)
			// by checking the matched string length
			if len(match[1]) <= 2 {
				return n
			}
		}
	}

	return 1 // Default
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

// isValidKeyword checks if a 4-char string is a valid product keyword.
// Accepts any 4-char alphanumeric code: "1001", "A9B1", "BONE", etc.
func isValidKeyword(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
