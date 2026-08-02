package httpx

import (
	"testing"

	// Aliased: package httpx has a package-level var named `logger` (response.go).
	loggerpkg "livecart/apps/api/lib/logger"
)

// TestCategoryConstsMatchLogger — AC 7: guards the deliberate duplication. The
// logger package copies its Cat* strings instead of importing httpx.Category so
// it stays a leaf (importing httpx would cycle). This guard lives HERE — httpx
// MAY import logger, never the reverse — and fails the instant the two drift.
func TestCategoryConstsMatchLogger(t *testing.T) {
	cases := []struct {
		name   string
		copied string   // logger's duplicated string
		source Category // httpx's source of truth
	}{
		{"domain", loggerpkg.CatDomain, CategoryDomain},
		{"validation", loggerpkg.CatValidation, CategoryValidation},
		{"repository", loggerpkg.CatRepository, CategoryRepository},
		{"infrastructure", loggerpkg.CatInfrastructure, CategoryInfrastructure},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if tt.copied != string(tt.source) {
				t.Fatalf("logger copy %q must equal httpx source %q", tt.copied, string(tt.source))
			}
		})
	}
}
