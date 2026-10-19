package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"livecart/apps/api/internal/integration/providers"
	"livecart/apps/api/lib/logger"
)

func TestIntegrationProbeLogsWithoutHealingTransientID(t *testing.T) {
	for _, tc := range []struct {
		name          string
		integrationID string
		status        string
		wantHealWarn  int
		operationLog  string
	}{
		{"successful probe", "probe", "success", 0, "integration operation completed"},
		{"rejected probe remains visible", "probe", "error", 0, "integration operation failed"},
		{"no persisted integration", "", "success", 0, "integration operation completed"},
		{"unexpected malformed ID remains visible", "invalid-id", "success", 1, "integration operation completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, entries := observer.New(zap.DebugLevel)
			// No database: transient IDs have no row to heal. The malformed-ID
			// control still reaches repository validation and reports the defect.
			svc := &Service{repo: &Repository{}, logger: zap.New(core)}
			err := svc.LogIntegrationOperation(t.Context(), providers.IntegrationLog{
				IntegrationID: tc.integrationID,
				Status:        tc.status,
				Direction:     "outbound",
				Method:        http.MethodGet,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := entries.FilterMessage("failed to heal integration status after success").Len(); got != tc.wantHealWarn {
				t.Fatalf("healing warnings = %d, want %d", got, tc.wantHealWarn)
			}
			if got := entries.FilterMessage(tc.operationLog).Len(); got != 1 {
				t.Fatalf("operation log count = %d, want 1", got)
			}
		})
	}
}

func TestIntegrationOperationLogsMetadataWithoutPayloads(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		message    string
	}{
		{"success", http.StatusOK, "integration operation completed"},
		{"provider rejection", http.StatusUnprocessableEntity, "integration operation failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const payload = `{"customer":"private-customer","access_token":"secret-token"}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()

			core, entries := observer.New(zap.DebugLevel)
			// No repository: recording an operation must not persist HTTP logs.
			// An empty integration ID skips status healing, tested separately by
			// rate_limit_resilience_test.go against the migrated database.
			svc := &Service{logger: zap.New(core)}
			provider := providers.NewBaseProvider(providers.BaseProviderConfig{
				Logger:  svc.logger,
				LogFunc: svc.LogIntegrationOperation,
			})
			ctx := logger.WithStore(context.Background(), "store-test", "")
			resp, body, err := provider.DoRequest(
				ctx,
				http.MethodPost,
				server.URL,
				map[string]string{"document": "private-document"},
				nil,
			)
			if err != nil || resp.StatusCode != tc.statusCode || string(body) != payload {
				t.Fatalf("provider response changed: response=%v body=%q err=%v", resp, body, err)
			}
			operation := entries.FilterMessage(tc.message).All()
			if len(operation) != 1 {
				t.Fatalf("expected one operation log, got %d", len(operation))
			}
			fields := operation[0].ContextMap()
			if fields["http_status"] != int64(tc.statusCode) || fields["method"] != http.MethodPost {
				t.Fatalf("missing HTTP metadata: %v", fields)
			}
			if fields["store_id"] != "store-test" {
				t.Fatalf("missing store correlation: %v", fields)
			}
			if tc.statusCode >= 400 && operation[0].Level != zap.WarnLevel {
				t.Fatal("provider rejection must remain visible at warn level")
			}
			for _, entry := range entries.All() {
				for key, value := range entry.ContextMap() {
					if key == "request_payload" || key == "response_payload" || key == "error_message" {
						t.Fatalf("raw operation field emitted: %s", key)
					}
					if text, ok := value.(string); ok && strings.Contains(text, "private-") {
						t.Fatalf("customer data emitted in %s", key)
					}
					if text, ok := value.(string); ok && strings.Contains(text, "secret-token") {
						t.Fatalf("credential emitted in %s", key)
					}
				}
			}
		})
	}
}
