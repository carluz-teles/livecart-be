package httpx

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// sampleReq is a stand-in for a domain *Request: it validates itself via ozzo.
type sampleReq struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (r sampleReq) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Name, validation.Required),
		validation.Field(&r.Email, validation.Required),
	)
}

func newTestApp() *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Post("/bind", func(c *fiber.Ctx) error {
		var req sampleReq
		if err := BindAndValidate(c, &req); err != nil {
			return err
		}
		return OK(c, req)
	})
	app.Get("/notfound", func(c *fiber.Ctx) error {
		return ErrNotFound("thing not found")
	})
	// Directly returning an ozzo validation.Errors (as a Request.Validate would).
	app.Get("/nested", func(c *fiber.Ctx) error {
		return validation.Errors{
			"name":    validation.NewError("x", "cannot be blank"),
			"address": validation.Errors{"zip": validation.NewError("y", "must be a valid format")},
		}
	})
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestErrorHandler_ValidationErrorsRenderAs422WithJSONKeys(t *testing.T) {
	app := newTestApp()

	code, body := do(t, app, "POST", "/bind", `{}`)
	if code != fiber.StatusUnprocessableEntity {
		t.Fatalf("empty body: want 422, got %d (%v)", code, body)
	}
	if body["error"] != "validation failed" {
		t.Fatalf("want error 'validation failed', got %v", body["error"])
	}
	fields, ok := body["fields"].(map[string]any)
	if !ok {
		t.Fatalf("want fields map, got %T (%v)", body["fields"], body["fields"])
	}
	// Keys must be the json tags the FE sent, not the Go field names.
	if _, ok := fields["name"]; !ok {
		t.Fatalf("want field 'name' (json tag) in %v", fields)
	}
	if _, ok := fields["email"]; !ok {
		t.Fatalf("want field 'email' (json tag) in %v", fields)
	}
}

// TestErrorHandler_ValidationEmitsCategoryTaggedLog — AC 8: a body failing ozzo
// (a) still returns the byte-identical 422 {error, fields} wire, and (b) emits
// exactly one log line tagged category=VALIDATION with field_count==len(fields)
// and status 422, carrying NO field content (PII-safe).
func TestErrorHandler_ValidationEmitsCategoryTaggedLog(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	SetLogger(zap.New(core))
	t.Cleanup(func() { SetLogger(nil) })

	app := newTestApp()

	// {} fails both Required fields → 2 field errors.
	code, body := do(t, app, "POST", "/bind", `{}`)

	// (a) wire unchanged.
	if code != fiber.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", code)
	}
	if body["error"] != "validation failed" {
		t.Fatalf("want error 'validation failed', got %v", body["error"])
	}
	fields, ok := body["fields"].(map[string]any)
	if !ok || len(fields) != 2 {
		t.Fatalf("want 2 validation fields, got %v", body["fields"])
	}

	// (b) exactly one VALIDATION-tagged log line, PII-safe.
	entries := logs.FilterMessage("validation error").All()
	if len(entries) != 1 {
		t.Fatalf("want 1 'validation error' log line, got %d", len(entries))
	}
	m := entries[0].ContextMap()
	if m["category"] != "VALIDATION" {
		t.Fatalf("category: want VALIDATION, got %v", m["category"])
	}
	if m["field_count"] != int64(2) {
		t.Fatalf("field_count: want 2, got %v", m["field_count"])
	}
	if m["status"] != int64(422) {
		t.Fatalf("status: want 422, got %v", m["status"])
	}
	// The offending field messages (which can carry user input) must never reach
	// the log line.
	if _, leaked := m["fields"]; leaked {
		t.Fatalf("log must not carry the fields bag (PII), got %v", m["fields"])
	}
}

func TestErrorHandler_NestedValidationFlattened(t *testing.T) {
	app := newTestApp()
	code, body := do(t, app, "GET", "/nested", "")
	if code != fiber.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", code)
	}
	fields := body["fields"].(map[string]any)
	if _, ok := fields["address.zip"]; !ok {
		t.Fatalf("want flattened key 'address.zip' in %v", fields)
	}
}

func TestBindAndValidate_MalformedBodyIs400(t *testing.T) {
	app := newTestApp()
	code, body := do(t, app, "POST", "/bind", `{not json`)
	if code != fiber.StatusBadRequest {
		t.Fatalf("malformed body: want 400, got %d (%v)", code, body)
	}
}

func TestBindAndValidate_ValidBodyPasses(t *testing.T) {
	app := newTestApp()
	code, body := do(t, app, "POST", "/bind", `{"name":"Ana","email":"ana@x.com"}`)
	if code != fiber.StatusOK {
		t.Fatalf("valid body: want 200, got %d (%v)", code, body)
	}
	if _, ok := body["data"]; !ok {
		t.Fatalf("want data envelope, got %v", body)
	}
}

func TestErrorHandler_ServiceErrorKeepsStatusAndMessage(t *testing.T) {
	app := newTestApp()
	code, body := do(t, app, "GET", "/notfound", "")
	if code != fiber.StatusNotFound {
		t.Fatalf("want 404, got %d", code)
	}
	if body["error"] != "thing not found" {
		t.Fatalf("want error 'thing not found', got %v", body["error"])
	}
}
