package httpx

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// appReturning builds a one-route fiber app wired to the central ErrorHandler
// whose single handler returns err. It exercises the real wire path
// (ErrorHandler → HandleServiceError → Envelope), so assertions on status/body
// reflect exactly what a client would receive.
func appReturning(err error) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/x", func(c *fiber.Ctx) error { return err })
	return app
}

// rawBody returns the status and the raw (unparsed) response body, so tests can
// assert on the exact bytes on the wire — e.g. that a leaked substring is
// ABSENT and that an omitempty field did not render.
func rawBody(t *testing.T, app *fiber.App) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", "/x", nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestServiceError_Wire4xxUnchanged — AC 3: a reason-less 4xx serializes exactly
// {"error":...,"requestId":...} with NO reason key (omitempty). Golden wire.
func TestServiceError_Wire4xxUnchanged(t *testing.T) {
	app := appReturning(&ServiceError{Code: 422, Message: "carrinho expirado"})

	code, body := do(t, app, "GET", "/x", "")
	if code != fiber.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d (%s)", code, body["error"])
	}
	if body["error"] != "carrinho expirado" {
		t.Fatalf("want error 'carrinho expirado', got %v", body["error"])
	}
	if _, ok := body["reason"]; ok {
		t.Fatalf("reason must be omitted for a reason-less error, got %v", body["reason"])
	}

	// And the raw bytes must not carry a "reason" key at all.
	_, raw := rawBody(t, app)
	if strings.Contains(raw, "reason") {
		t.Fatalf("raw body must not contain a reason key: %s", raw)
	}
}

// TestServiceError_5xxHardened — AC 4: a 500 from RepositoryError returns a
// generic body (error:"internal server error", reason:"INTERNAL") that NEVER
// leaks the wrapped cause, while errors.Unwrap still recovers it server-side and
// the log line carries category/code/cause.
func TestServiceError_5xxHardened(t *testing.T) {
	cause := errors.New("pgx: connection refused")

	// Unwrap recovers the cause for the server (single source for the log).
	err := RepositoryError(cause, "GetCart")
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is must find the wrapped cause")
	}
	if errors.Unwrap(err) != cause {
		t.Fatalf("errors.Unwrap must return the wrapped cause")
	}

	// Wire: generic, no leak.
	code, raw := rawBody(t, appReturning(RepositoryError(cause, "GetCart")))
	if code != fiber.StatusInternalServerError {
		t.Fatalf("want 500, got %d", code)
	}
	if !strings.Contains(raw, `"error":"internal server error"`) {
		t.Fatalf("want generic error message, got %s", raw)
	}
	if !strings.Contains(raw, `"reason":"INTERNAL"`) {
		t.Fatalf("want reason INTERNAL, got %s", raw)
	}
	if strings.Contains(raw, "pgx") || strings.Contains(raw, "GetCart") {
		t.Fatalf("5xx body leaked the cause/op: %s", raw)
	}

	// Log: rich (category=REPOSITORY, code=INTERNAL, cause + op present).
	core, logs := observer.New(zap.InfoLevel)
	SetLogger(zap.New(core))
	t.Cleanup(func() { SetLogger(nil) })

	if _, raw := rawBody(t, appReturning(RepositoryError(cause, "GetCart"))); strings.Contains(raw, "pgx") {
		t.Fatalf("5xx body leaked the cause: %s", raw)
	}
	entries := logs.FilterMessage("internal error").All()
	if len(entries) != 1 {
		t.Fatalf("want 1 'internal error' log line, got %d", len(entries))
	}
	m := entries[0].ContextMap()
	if m["category"] != "REPOSITORY" {
		t.Fatalf("log category: want REPOSITORY, got %v", m["category"])
	}
	if m["code"] != "INTERNAL" {
		t.Fatalf("log code: want INTERNAL, got %v", m["code"])
	}
	if m["op"] != "GetCart" {
		t.Fatalf("log op: want GetCart, got %v", m["op"])
	}
	if c, ok := m["cause"].(string); !ok || !strings.Contains(c, "pgx: connection refused") {
		t.Fatalf("log must carry the wrapped cause, got %v", m["cause"])
	}
}

// TestDomainError_SetsReasonAndCategory — AC (DomainError): sets status, message,
// reason (from Code) and DOMAIN category, and round-trips on the wire.
func TestDomainError_SetsReasonAndCategory(t *testing.T) {
	err := DomainError(422, CodeCartExpired, "carrinho expirado")

	var se *ServiceError
	if !errors.As(err, &se) {
		t.Fatalf("DomainError must produce a *ServiceError")
	}
	if se.Code != 422 || se.Message != "carrinho expirado" {
		t.Fatalf("status/message wrong: %+v", se)
	}
	if se.Reason != "CART_EXPIRED" {
		t.Fatalf("reason: want CART_EXPIRED, got %q", se.Reason)
	}
	if se.Category != CategoryDomain {
		t.Fatalf("category: want DOMAIN, got %q", se.Category)
	}

	code, body := do(t, appReturning(err), "GET", "/x", "")
	if code != 422 || body["error"] != "carrinho expirado" || body["reason"] != "CART_EXPIRED" {
		t.Fatalf("wire: got %d %v", code, body)
	}
}

// TestWithCode_TypedSibling — AC (WithCode): attaches a typed Code as Reason on a
// *ServiceError, and is a safe no-op on a non-ServiceError.
func TestWithCode_TypedSibling(t *testing.T) {
	err := WithCode(ErrUnprocessable("carrinho expirado"), CodeCartExpired)
	var se *ServiceError
	if !errors.As(err, &se) || se.Reason != "CART_EXPIRED" {
		t.Fatalf("WithCode must set reason CART_EXPIRED, got %+v", se)
	}

	plain := errors.New("boom")
	if got := WithCode(plain, CodeCartExpired); got != plain {
		t.Fatalf("WithCode on a non-ServiceError must be a no-op returning the same err")
	}
}

// TestExistingWithReasonSitesUnaffected — AC 5: the three existing checkout
// WithReason sites emit lower_snake reasons; that path is untouched by this slice
// (the UPPER rename is D1c). Guards the wire behavior those sites depend on.
func TestExistingWithReasonSitesUnaffected(t *testing.T) {
	for _, reason := range []string{"payment_not_configured", "payment_unavailable"} {
		err := WithReason(ErrUnprocessable("pagamento indisponível"), reason)

		var se *ServiceError
		if !errors.As(err, &se) || se.Reason != reason {
			t.Fatalf("WithReason must preserve %q, got %+v", reason, se)
		}

		_, body := do(t, appReturning(err), "GET", "/x", "")
		if body["reason"] != reason {
			t.Fatalf("wire reason: want %q, got %v", reason, body["reason"])
		}
	}
}
