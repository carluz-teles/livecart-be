package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestRedactingCoreProtectsBoundAndNestedFields(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&output), zap.DebugLevel)
	base := zap.New(&redactingCore{Core: core})
	metadata := map[string]any{
		"cart_token": "nested-token",
		"cart_id":    "cart-1",
		"items": []map[string]any{{
			"checkoutToken": "array-token", "product_id": "product-1",
		}},
	}
	bound := base.With(zap.String("cart_token", "bound-token"), zap.Any("metadata", metadata))
	bound.Error("request /api/public/checkout/message-token/card failed",
		zap.String("path", "/api/public/checkout/path-token/shipping-quote"),
		zap.String("url", "https://shop.test/checkout/url-token?cart_token=query-token"),
		zap.ByteString("redirect", []byte("/checkout/byte-token")),
		zap.Error(fmt.Errorf("payment: %w", errors.New("GET /checkout/error-token?checkout_token=error-query: timeout"))),
		zap.Any("request_payload", map[string]any{"customer": "Buyer Name", "card_number": "4111111111111111"}),
		zap.String("response_body", `{"email":"buyer@example.test","document":"12345678900"}`),
		zap.String("cart_id", "cart-1"),
		zap.String("payment_id", "ch_1"),
		zap.Int64("amount_cents", 29025),
	)
	for _, secret := range []string{
		"nested-token", "array-token", "bound-token", "message-token", "path-token", "url-token",
		"query-token", "byte-token", "error-token", "error-query", "Buyer Name", "4111111111111111",
		"buyer@example.test", "12345678900",
	} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("log contains secret %q", secret)
		}
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["cart_id"] != "cart-1" || entry["payment_id"] != "ch_1" || entry["amount_cents"] != float64(29025) {
		t.Fatalf("safe payment context lost: %v", entry)
	}
	if entry["path"] != "/api/public/checkout/[redacted]/shipping-quote" {
		t.Errorf("path lost route context: %v", entry["path"])
	}
	if metadata["cart_token"] != "nested-token" {
		t.Fatal("redaction mutated the caller's metadata")
	}
	base.Info("parent remains usable", zap.String("cart_id", "cart-2"))
	if !strings.Contains(output.String(), "cart-2") {
		t.Fatal("With modified the parent logger")
	}
}

func TestRedactingCoreObjectsNamespacesAndProviderErrors(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&output), zap.InfoLevel)
	log := zap.New(&redactingCore{Core: core})
	log.Info("inline amount", zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddInt64("amount_cents", 29025)
		enc.AddString("cart_token", "inline-token")
		return nil
	})))
	log.With(zap.Object("operation", zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
		enc.AddString("cart_token", "object-token")
		enc.AddString("cart_id", "cart-object")
		return nil
	}))).Error("provider failed", zap.Error(fmt.Errorf("wrapped: %w", safeTestError{})))
	log.With(zap.String("cart_id", "cart-namespace"), zap.Namespace("customer")).Info("customer processing",
		zap.String("name", "Hidden Buyer"), zap.String("email", "secret@example.test"))
	log.Error("raw error", zap.Error(errors.New(`HTTP 422: {"name":"Echo Buyer","card_token":"raw-token"}`)))
	for _, secret := range []string{
		"object-token", "inline-token", "Hidden Buyer", "secret@example.test", "Echo Buyer", "raw-token", "provider buyer",
	} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("log contains sensitive value %q", secret)
		}
	}
	if !strings.Contains(output.String(), `"amount_cents":29025`) {
		t.Fatal("redaction changed an inline amount to a string")
	}
	for _, useful := range []string{"cart-object", "cart-namespace", "HTTP 422", "pagarme creating customer"} {
		if !strings.Contains(output.String(), useful) {
			t.Errorf("lost diagnostic context %q", useful)
		}
	}
}

type safeTestError struct{}

func (safeTestError) Error() string      { return "validation failed for provider buyer" }
func (safeTestError) LogMessage() string { return "pagarme creating customer: HTTP 422" }

func TestRedactTextTokenFormats(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"/api/public/checkout/secret/cart",
		"https://shop.test/cart/secret?status=success",
		"/checkout/secret%2Fmore?cart_token=secret",
		"cart_token=secret", "checkoutToken: secret", `card_token="secret"`,
		"token='secret'", `failed: {"customer":"secret"}`,
	} {
		t.Run(input, func(t *testing.T) {
			if got := redactText(input); strings.Contains(got, "secret") {
				t.Errorf("sensitive text survived: %s", got)
			}
		})
	}
}

func TestNewInstallsRedactionInEveryEnvironment(t *testing.T) {
	for _, env := range []string{"production", "staging", "development"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("APP_ENV", env)
			// New opens stderr as its sink; redirect only within this serial test.
			sink, err := os.CreateTemp(t.TempDir(), "log")
			if err != nil {
				t.Fatal(err)
			}
			previous := os.Stderr
			os.Stderr = sink
			defer func() { os.Stderr = previous; _ = sink.Close() }()
			log, err := New()
			if err != nil {
				t.Fatal(err)
			}
			log.With(zap.String("checkout_token", "production-secret")).Info("test",
				zap.String("path", "/checkout/path-secret/card"), zap.String("cart_id", "cart-visible"))
			if err := log.Sync(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(sink.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "secret") || !strings.Contains(string(data), "cart-visible") {
				t.Fatalf("New did not install redaction for %s: %s", env, data)
			}
		})
	}
}
