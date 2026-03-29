package integration

import (
	"regexp"
	"strconv"
	"strings"
)

// PurchaseIntent represents a detected purchase intent from a comment
type PurchaseIntent struct {
	Quantity int    // Quantity requested (e.g., 2 for "quero 2")
	RawText  string // Original comment text
}

// purchasePatterns are regex patterns to detect purchase intent
// Order matters: more specific patterns should come first
var purchasePatterns = []*regexp.Regexp{
	// "quero X", "quero X unidades"
	regexp.MustCompile(`(?i)\bquero\s+(\d+)\b`),
	// "reserva X", "reserva X pra mim"
	regexp.MustCompile(`(?i)\breserva\s+(\d+)\b`),
	// "manda X", "manda X unidades"
	regexp.MustCompile(`(?i)\bmanda\s+(\d+)\b`),
	// "separa X", "separa X pra mim"
	regexp.MustCompile(`(?i)\bsepara\s+(\d+)\b`),
	// "X unidades", "X unidade"
	regexp.MustCompile(`(?i)\b(\d+)\s+unidades?\b`),
	// "pega X", "pega X pra mim"
	regexp.MustCompile(`(?i)\bpega\s+(\d+)\b`),
	// "eu quero X"
	regexp.MustCompile(`(?i)\beu\s+quero\s+(\d+)\b`),
	// "me manda X"
	regexp.MustCompile(`(?i)\bme\s+manda\s+(\d+)\b`),
	// "coloca X"
	regexp.MustCompile(`(?i)\bcoloca\s+(\d+)\b`),
	// Just "quero" without number = 1 unit
	regexp.MustCompile(`(?i)\bquero\b`),
	// Just "eu quero" without number = 1 unit
	regexp.MustCompile(`(?i)\beu\s+quero\b`),
}

// negativePatterns indicate the user is NOT buying
var negativePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bn[aã]o\s+quero\b`),
	regexp.MustCompile(`(?i)\bcancela\b`),
	regexp.MustCompile(`(?i)\bdesisto\b`),
	regexp.MustCompile(`(?i)\bn[aã]o\s+preciso\b`),
}

// questionPatterns indicate the user is asking a question, not buying
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

// ParsePurchaseIntent analyzes comment text and detects purchase intent
// Returns nil if no purchase intent is detected
func ParsePurchaseIntent(text string) *PurchaseIntent {
	// Normalize text
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

	// Check for question patterns (not a purchase)
	for _, pattern := range questionPatterns {
		if pattern.MatchString(text) {
			return nil
		}
	}

	// Try to match purchase patterns
	for _, pattern := range purchasePatterns {
		matches := pattern.FindStringSubmatch(text)
		if matches != nil {
			quantity := 1 // Default quantity

			// If we captured a number, parse it
			if len(matches) > 1 && matches[1] != "" {
				if q, err := strconv.Atoi(matches[1]); err == nil && q > 0 {
					quantity = q
				}
			}

			// Sanity check: limit quantity to reasonable range
			if quantity > 100 {
				quantity = 100
			}

			return &PurchaseIntent{
				Quantity: quantity,
				RawText:  text,
			}
		}
	}

	return nil
}

// IsCancellation checks if the text indicates a cancellation request
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

// keywordPattern matches 4-character alphanumeric codes like "A9B1", "X1Y2"
// Must contain at least one letter and one digit to be a valid keyword
var keywordPattern = regexp.MustCompile(`\b([A-Za-z0-9]{4})\b`)

// ExtractPossibleKeywords extracts all 4-character alphanumeric codes from text.
// Only returns codes that contain both letters and digits (e.g., "A9B1").
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
		// Must have at least one letter AND one digit (e.g., A9B1)
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
// Valid keywords must contain at least one letter and one digit.
func isValidKeyword(s string) bool {
	hasLetter := false
	hasDigit := false
	for _, c := range s {
		if c >= 'A' && c <= 'Z' {
			hasLetter = true
		}
		if c >= '0' && c <= '9' {
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}
