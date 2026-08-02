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

// TestCheckoutDomainCodes_WireUpper — D1c: the checkout domain throws migrated
// from ErrUnprocessable(+WithReason lower_snake) to DomainError(422, Code, msg).
// This guards the exact wire contract those sites now emit — status 422, the
// EXACT human message, and an UPPER_SNAKE reason — including the three payment
// sites renamed from the old lower_snake reasons. Each row mirrors a real throw
// site (checkout/service.go + checkout/shipping.go) verbatim.
func TestCheckoutDomainCodes_WireUpper(t *testing.T) {
	tests := []struct {
		name       string
		code       Code
		msg        string
		wantReason string
	}{
		// payment (renamed lower→UPPER)
		{"payment not configured", CodePaymentNotConfigured, "loja não possui integração de pagamento configurada", "PAYMENT_NOT_CONFIGURED"},
		{"payment unavailable", CodePaymentUnavailable, "nenhuma integração de pagamento está respondendo agora", "PAYMENT_UNAVAILABLE"},
		{"payment link failed", CodePaymentLinkFailed, "erro ao gerar link de pagamento. Tente novamente.", "PAYMENT_LINK_FAILED"},
		// cart
		{"cart expired", CodeCartExpired, "carrinho expirado", "CART_EXPIRED"},
		{"cart already paid", CodeCartAlreadyPaid, "carrinho já foi pago", "CART_ALREADY_PAID"},
		{"cart not payable", CodeCartNotPayable, "carrinho não está disponível para checkout", "CART_NOT_PAYABLE"},
		{"cart no items payable", CodeCartNoItemsPayable, "carrinho não tem itens disponíveis para pagamento", "CART_NO_ITEMS_PAYABLE"},
		// shipping
		{"shipping cart empty", CodeShippingCartEmpty, "nenhum item no carrinho para cotar", "SHIPPING_CART_EMPTY"},
		{"shipping cep required", CodeShippingCepRequired, "CEP é obrigatório para confirmar o frete", "SHIPPING_CEP_REQUIRED"},
		{"shipping not quoted", CodeShippingNotQuoted, "primeiro cote o frete antes de selecionar", "SHIPPING_NOT_QUOTED"},
		{"shipping quote expired", CodeShippingQuoteExpired, "cotação expirou — refaça a cotação", "SHIPPING_QUOTE_EXPIRED"},
		{"shipping option missing", CodeShippingOptionMissing, "opção de frete não encontrada na cotação atual — refaça a cotação", "SHIPPING_OPTION_MISSING"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DomainError(422, tt.code, tt.msg)

			// errors.As + se.Reason: DOMAIN category, 422, exact message, UPPER reason.
			var se *ServiceError
			if !errors.As(err, &se) {
				t.Fatalf("DomainError must produce a *ServiceError")
			}
			if se.Code != 422 || se.Message != tt.msg {
				t.Fatalf("status/message wrong: %+v", se)
			}
			if se.Reason != tt.wantReason {
				t.Fatalf("reason: want %q, got %q", tt.wantReason, se.Reason)
			}
			if se.Category != CategoryDomain {
				t.Fatalf("category: want DOMAIN, got %q", se.Category)
			}

			// Wire: status 422 + exact message + UPPER reason on the Envelope.
			code, body := do(t, appReturning(err), "GET", "/x", "")
			if code != 422 || body["error"] != tt.msg || body["reason"] != tt.wantReason {
				t.Fatalf("wire: got %d %v", code, body)
			}
		})
	}
}

// TestCouponDomainCodes_WireUpper — D1d-1: the coupon domain throws migrated
// from httpx.Err{Conflict,Unprocessable,NotFound}(msg) to
// DomainError(status, Code, msg), preserving each site's EXACT status
// (409/422/404) and human message — now also carrying the typed UPPER_SNAKE
// reason (Category=DOMAIN). Unlike checkout (all 422), coupon spans three
// statuses, so each row asserts its own. Each row mirrors a real throw site in
// internal/coupon/service.go verbatim (message byte-for-byte, format strings
// filled in).
func TestCouponDomainCodes_WireUpper(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       Code
		msg        string
		wantReason string
	}{
		// 409 conflict
		{"coupon already exists", 409, CodeCouponAlreadyExists, `coupon code "SAVE10" already exists for this event`, "COUPON_ALREADY_EXISTS"},
		{"coupon redeemed (delete guard)", 409, CodeCouponRedeemed, "coupon already redeemed by a customer; disable it instead of deleting", "COUPON_REDEEMED"},
		{"cart already paid", 409, CodeCartAlreadyPaid, "cart already paid", "CART_ALREADY_PAID"},
		{"cart expired", 409, CodeCartExpired, "cart expired", "CART_EXPIRED"},
		{"cart already has a coupon", 409, CodeCouponHasCoupon, "cart already has a coupon — remove it first", "CART_HAS_COUPON"},
		{"coupon fully redeemed", 409, CodeCouponFullyRedeemed, "coupon already fully redeemed", "COUPON_FULLY_REDEEMED"},
		// 422 unprocessable
		{"coupon code required", 422, CodeCouponCodeRequired, "coupon code is required", "COUPON_CODE_REQUIRED"},
		{"cart empty", 422, CodeCartEmpty, "cart is empty", "CART_EMPTY"},
		{"coupon not active", 422, CodeCouponNotActive, "coupon is not active", "COUPON_NOT_ACTIVE"},
		{"coupon not yet valid", 422, CodeCouponNotYetValid, "coupon is not valid yet", "COUPON_NOT_YET_VALID"},
		{"coupon expired", 422, CodeCouponExpired, "coupon expired", "COUPON_EXPIRED"},
		{"coupon min purchase", 422, CodeCouponMinPurchase, "minimum purchase of 50.00 BRL not reached", "COUPON_MIN_PURCHASE"},
		{"coupon shipping required", 422, CodeCouponShippingRequired, "select shipping before applying a free-shipping coupon", "COUPON_SHIPPING_REQUIRED"},
		// 404 not found
		{"invalid coupon code", 404, CodeCouponInvalidCode, "invalid coupon code", "COUPON_INVALID_CODE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DomainError(tt.status, tt.code, tt.msg)

			// errors.As + se: DOMAIN category, exact status, exact message, UPPER reason.
			var se *ServiceError
			if !errors.As(err, &se) {
				t.Fatalf("DomainError must produce a *ServiceError")
			}
			if se.Code != tt.status || se.Message != tt.msg {
				t.Fatalf("status/message wrong: %+v", se)
			}
			if se.Reason != tt.wantReason {
				t.Fatalf("reason: want %q, got %q", tt.wantReason, se.Reason)
			}
			if se.Category != CategoryDomain {
				t.Fatalf("category: want DOMAIN, got %q", se.Category)
			}

			// Wire: exact status + exact message + UPPER reason on the Envelope.
			code, body := do(t, appReturning(err), "GET", "/x", "")
			if code != tt.status || body["error"] != tt.msg || body["reason"] != tt.wantReason {
				t.Fatalf("wire: got %d %v", code, body)
			}
		})
	}
}

// TestStoreInvitationDomainCodes_WireUpper — D1d-3: the store + invitation domain
// throws migrated from httpx.Err{Conflict,Unprocessable,Gone,Forbidden}(msg) to
// DomainError(status, Code, msg), preserving each site's EXACT status — this slice
// spans FOUR statuses (409 conflict, 422 unprocessable, 410 gone, 403 forbidden) —
// and its human message byte-for-byte, now also carrying the typed UPPER_SNAKE
// reason (Category=DOMAIN). Each row mirrors a real throw site in
// internal/store/service.go or internal/invitation/{service,repository}.go verbatim.
// The "invitation is %s" sites render a status into the message; the row uses one
// concrete rendering ("invitation is revoked") to assert the format string stays
// intact. Out of scope (unchanged): the ErrGone(err.Error()) default arms, the
// ErrInternal membership-check failure, the not-found and validation sites.
func TestStoreInvitationDomainCodes_WireUpper(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       Code
		msg        string
		wantReason string
	}{
		// 409 conflict
		{"store already exists", 409, CodeStoreAlreadyExists, "you already have a store - delete your current store first to create a new one", "STORE_ALREADY_EXISTS"},
		{"store slug in use", 409, CodeStoreSlugInUse, "slug already in use", "STORE_SLUG_IN_USE"},
		{"invitation already exists", 409, CodeInvitationExists, "invitation already exists for this email", "INVITATION_ALREADY_EXISTS"},
		{"owner of other store", 409, CodeOwnerOfOtherStore, "you are the owner of another store - delete your store first to accept this invitation", "OWNER_OF_OTHER_STORE"},
		// 422 unprocessable
		{"user not synced (store create)", 422, CodeUserNotSynced, "user not found - please sync your account first", "USER_NOT_SYNCED"},
		{"resend not pending", 422, CodeInvitationNotPending, "can only resend pending invitations", "INVITATION_NOT_PENDING"},
		// 410 gone
		{"invitation expired", 410, CodeInvitationExpired, "invitation has expired", "INVITATION_EXPIRED"},
		{"invitation not acceptable", 410, CodeInvitationNotAcceptable, "invitation is revoked", "INVITATION_NOT_ACCEPTABLE"},
		{"accept no longer pending", 410, CodeInvitationNotPending, "invitation is no longer pending", "INVITATION_NOT_PENDING"},
		// 403 forbidden
		{"invitation email mismatch", 403, CodeInvitationEmailMismatch, "invitation email does not match your account", "INVITATION_EMAIL_MISMATCH"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DomainError(tt.status, tt.code, tt.msg)

			// errors.As + se: DOMAIN category, exact status, exact message, UPPER reason.
			var se *ServiceError
			if !errors.As(err, &se) {
				t.Fatalf("DomainError must produce a *ServiceError")
			}
			if se.Code != tt.status || se.Message != tt.msg {
				t.Fatalf("status/message wrong: %+v", se)
			}
			if se.Reason != tt.wantReason {
				t.Fatalf("reason: want %q, got %q", tt.wantReason, se.Reason)
			}
			if se.Category != CategoryDomain {
				t.Fatalf("category: want DOMAIN, got %q", se.Category)
			}

			// Wire: exact status + exact message + UPPER reason on the Envelope.
			code, body := do(t, appReturning(err), "GET", "/x", "")
			if code != tt.status || body["error"] != tt.msg || body["reason"] != tt.wantReason {
				t.Fatalf("wire: got %d %v", code, body)
			}
		})
	}
}

// TestOrderDomainCodes_WireUpper — D1d-2: the order domain throws migrated from
// httpx.Err{Conflict,Unprocessable}(msg) to DomainError(status, Code, msg),
// preserving each site's EXACT status (409/422) and human message — now also
// carrying the typed UPPER_SNAKE reason (Category=DOMAIN). Like coupon, order
// spans more than one status, so each row asserts its own. Each row mirrors a
// real throw site in internal/order/service.go verbatim (message byte-for-byte).
// The not-found sites ("order %s not found") stay on ErrNotFound and are out of
// scope for this slice.
func TestOrderDomainCodes_WireUpper(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       Code
		msg        string
		wantReason string
	}{
		// 422 unprocessable — capability unavailable
		{"cancel unavailable", 422, CodeCancelUnavailable, "cancelamento indisponível", "CANCEL_UNAVAILABLE"},
		{"nfe sync unavailable", 422, CodeNfeSyncUnavailable, "sync de NFe indisponível", "NFE_SYNC_UNAVAILABLE"},
		{"erp retry unavailable", 422, CodeErpRetryUnavailable, "retry de ERP indisponível", "ERP_RETRY_UNAVAILABLE"},
		// 409 conflict — cancel guards
		{"order already paid", 409, CodeOrderAlreadyPaid, "pedido já foi pago — não é possível cancelar", "ORDER_ALREADY_PAID"},
		{"order already cancelled", 409, CodeOrderAlreadyCancelled, "pedido já está cancelado", "ORDER_ALREADY_CANCELLED"},
		{"order expired", 409, CodeOrderExpired, "pedido já expirou", "ORDER_EXPIRED"},
		// 409 conflict — address/checkout locks
		{"address locked after payment", 409, CodeOrderAddressLocked, "cannot edit shipping address after payment", "ORDER_ADDRESS_LOCKED"},
		{"address locked after shipment", 409, CodeOrderAddressLocked, "cannot edit shipping address after shipment is created", "ORDER_ADDRESS_LOCKED"},
		{"checkout locked on paid", 409, CodeOrderCheckoutLocked, "cannot regenerate checkout for a paid order", "ORDER_CHECKOUT_LOCKED"},
		{"checkout locked after shipment", 409, CodeOrderCheckoutLocked, "cannot regenerate checkout after shipment is created", "ORDER_CHECKOUT_LOCKED"},
		{"regenerate on cancelled", 409, CodeOrderRegenCancelled, "pedido cancelado — não é possível regerar o link", "ORDER_REGEN_CANCELLED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DomainError(tt.status, tt.code, tt.msg)

			// errors.As + se: DOMAIN category, exact status, exact message, UPPER reason.
			var se *ServiceError
			if !errors.As(err, &se) {
				t.Fatalf("DomainError must produce a *ServiceError")
			}
			if se.Code != tt.status || se.Message != tt.msg {
				t.Fatalf("status/message wrong: %+v", se)
			}
			if se.Reason != tt.wantReason {
				t.Fatalf("reason: want %q, got %q", tt.wantReason, se.Reason)
			}
			if se.Category != CategoryDomain {
				t.Fatalf("category: want DOMAIN, got %q", se.Category)
			}

			// Wire: exact status + exact message + UPPER reason on the Envelope.
			code, body := do(t, appReturning(err), "GET", "/x", "")
			if code != tt.status || body["error"] != tt.msg || body["reason"] != tt.wantReason {
				t.Fatalf("wire: got %d %v", code, body)
			}
		})
	}
}

// TestLiveErpDomainCodes_WireUpper — D1e-1: the live + erp domain throws migrated
// from httpx.Err{BadRequest,Unprocessable}(msg) to DomainError(status, Code, msg),
// preserving each site's EXACT status (400 bad request / 422 unprocessable) and
// human message byte-for-byte — now also carrying the typed UPPER_SNAKE reason
// (Category=DOMAIN). This slice spans two statuses, so each row asserts its own.
// Each row mirrors a real throw site verbatim: internal/live/service.go,
// internal/erp/{finalisation,stock_service,invoice}.go. The "estado inválido para
// retry: <status>" site renders row.Status into the message; the row uses one
// concrete rendering ("estado inválido para retry: pending") to assert the
// concatenation stays intact. LIVE_EVENT_NOT_ACTIVE is one code reused by two
// sites (set-active-product / change-processing) with distinct messages, so it
// appears in two rows. Out of scope (unchanged): the ErrNotFound sites and the
// repository/infrastructure DB errors (D1e-2).
func TestLiveErpDomainCodes_WireUpper(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       Code
		msg        string
		wantReason string
	}{
		// 400 bad request — live (internal/live/service.go)
		{"live media required", 400, CodeLiveMediaRequired, "mediaId is required", "LIVE_MEDIA_REQUIRED"},
		{"live product required", 400, CodeLiveProductRequired, "select at least one product for the promotion", "LIVE_PRODUCT_REQUIRED"},
		{"live event not active (set active product)", 400, CodeLiveEventNotActive, "can only set active product on active events", "LIVE_EVENT_NOT_ACTIVE"},
		{"live event not active (change processing)", 400, CodeLiveEventNotActive, "can only change processing state on active events", "LIVE_EVENT_NOT_ACTIVE"},
		// 422 unprocessable — instagram messaging (internal/live/service.go)
		{"ig notify not configured", 422, CodeIgNotifyNotConfigured, "instagram notifications are not configured", "IG_NOTIFY_NOT_CONFIGURED"},
		{"cart no ig recipient", 422, CodeCartNoIgRecipient, "cart has no Instagram recipient", "CART_NO_IG_RECIPIENT"},
		{"cart no items to send", 422, CodeCartNoItemsToSend, "cart has no items to send", "CART_NO_ITEMS_TO_SEND"},
		{"ig message window closed", 422, CodeIgMessageWindowClosed, "O Instagram só permite enviar a mensagem se o comprador comentou recentemente ou mandou uma DM para a loja. Peça para o comprador comentar de novo na live (ou enviar uma DM) e clique em reenviar em seguida.", "IG_MESSAGE_WINDOW_CLOSED"},
		{"ig message failed", 422, CodeIgMessageFailed, "failed to send Instagram message", "IG_MESSAGE_FAILED"},
		// 422 unprocessable — erp (internal/erp/{finalisation,stock_service,invoice}.go)
		{"erp finalisation in progress", 422, CodeErpFinalisationInProgress, "aguarde a finalização inicial concluir antes de tentar de novo", "ERP_FINALISATION_IN_PROGRESS"},
		{"erp retry invalid state", 422, CodeErpRetryInvalidState, "estado inválido para retry: pending", "ERP_RETRY_INVALID_STATE"},
		{"erp retry no snapshot", 422, CodeErpRetryNoSnapshot, "snapshot de pagamento ausente — retry não disponível", "ERP_RETRY_NO_SNAPSHOT"},
		{"stock insufficient", 422, CodeStockInsufficient, "estoque insuficiente para esse aumento", "STOCK_INSUFFICIENT"},
		{"erp not active", 422, CodeErpNotActive, "ERP integration not active for store", "ERP_NOT_ACTIVE"},
		{"erp no invoice support", 422, CodeErpNoInvoiceSupport, "ERP provider does not expose invoice operations", "ERP_NO_INVOICE_SUPPORT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DomainError(tt.status, tt.code, tt.msg)

			// errors.As + se: DOMAIN category, exact status, exact message, UPPER reason.
			var se *ServiceError
			if !errors.As(err, &se) {
				t.Fatalf("DomainError must produce a *ServiceError")
			}
			if se.Code != tt.status || se.Message != tt.msg {
				t.Fatalf("status/message wrong: %+v", se)
			}
			if se.Reason != tt.wantReason {
				t.Fatalf("reason: want %q, got %q", tt.wantReason, se.Reason)
			}
			if se.Category != CategoryDomain {
				t.Fatalf("category: want DOMAIN, got %q", se.Category)
			}

			// Wire: exact status + exact message + UPPER reason on the Envelope.
			code, body := do(t, appReturning(err), "GET", "/x", "")
			if code != tt.status || body["error"] != tt.msg || body["reason"] != tt.wantReason {
				t.Fatalf("wire: got %d %v", code, body)
			}
		})
	}
}
