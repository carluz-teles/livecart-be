package integration

import "livecart/apps/api/internal/live"

// The Instagram comment parser moved to internal/live (Bloco B4a): it is pure
// Live/parse logic with no integration dependency, and internal/live is the
// package that owns comment ingestion. These thin re-exports keep the parser's
// public surface byte-identical for the integration callers that still reference
// it (ProcessInstagramComment, resolvePostEventProduct), so no call site changed.

// PurchaseIntent is an alias of live.PurchaseIntent (moved with the parser).
type PurchaseIntent = live.PurchaseIntent

// ParsePurchaseIntent delegates to live.ParsePurchaseIntent.
func ParsePurchaseIntent(text string) *PurchaseIntent { return live.ParsePurchaseIntent(text) }

// ExtractPossibleKeywords delegates to live.ExtractPossibleKeywords.
func ExtractPossibleKeywords(text string) []string { return live.ExtractPossibleKeywords(text) }

// IsCancellation delegates to live.IsCancellation.
func IsCancellation(text string) bool { return live.IsCancellation(text) }
