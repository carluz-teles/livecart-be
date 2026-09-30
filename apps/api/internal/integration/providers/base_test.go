package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestProviderFailureCarriesPathAndStoreWithoutQuerySecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	core, logs := observer.New(zap.DebugLevel)
	var operation IntegrationLog
	provider := NewBaseProvider(BaseProviderConfig{IntegrationID: "integration-1", StoreID: "store-1", Logger: zap.New(core),
		LogFunc: func(_ context.Context, entry IntegrationLog) error { operation = entry; return nil },
	})
	if _, _, err := provider.DoRequest(t.Context(), http.MethodGet, server.URL+"/pedidos/123?access_token=secret", nil, nil); err != nil {
		t.Fatal(err)
	}
	if operation.Path != "/pedidos/123" || operation.StoreID != "store-1" || operation.HTTPStatus != 404 {
		t.Fatalf("missing operation context: %+v", operation)
	}
	for _, entry := range logs.All() {
		for _, field := range entry.Context {
			if strings.Contains(field.String, "secret") {
				t.Fatal("query credential leaked")
			}
		}
	}
}
