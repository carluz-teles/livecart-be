# Error System Plan — Categories, Machine Codes (BE→FE), Structured Logging

Status: DESIGN / PLAN (not implemented). Branch context: `stg`.
Scope: `apps/api` (Go / Fiber / zap). FE work (code→message map) tracked separately in `livecart-fe`.

This is an **evolution** of what already ships, not a greenfield build. The BE→FE
machine-code pipe already exists end to end (`ServiceError.Reason` →
`Envelope.reason`), and validation already has its own dedicated channel
(`ValidationEnvelope` → `422 {error, fields}`). The initiative adds three thin,
additive layers on top: a **category** taxonomy, a **typed code catalog** that
turns the underused `Reason` string into a stable enum, and a **logging
convention** (`::func::` marker + metadata bag + category tag).

---

## 1. Gap analysis (what exists vs. what the initiative needs)

### 1.1 The machine-code pipe ALREADY EXISTS

- `apps/api/lib/httpx/errors.go:9-16` — `ServiceError{Code int, Message string, Reason string}`.
  The doc comment already calls `Reason` "an optional stable machine code
  (e.g. `payment_not_configured`) the frontend can branch on instead of matching
  the human message."
- `apps/api/lib/httpx/errors.go:22-28` — `WithReason(err, reason)` attaches a
  reason to any `*ServiceError` (no-op otherwise).
- `apps/api/lib/httpx/response.go:20-27` — `Envelope` carries `Reason string`
  with json tag `reason,omitempty`.
- `apps/api/lib/httpx/response.go:107` — `HandleServiceError` already serializes
  `se.Reason` into the response:
  `c.Status(se.Code).JSON(Envelope{Error: se.Message, RequestID: reqID, Reason: se.Reason})`.

**Conclusion:** the wire contract for domain codes is done. What's missing is
**adoption** — only three call sites set a reason today (all in checkout, see §3):
`apps/api/internal/checkout/service.go:646-649`, `:686-689`, `:709-712`
(`payment_not_configured`, `payment_unavailable`). Everything else returns a bare
human `Message` and no `Reason`.

Two consistency gaps in the existing three reason strings:
- They are **lower_snake** (`payment_not_configured`), but the owner wants
  **UPPER_SNAKE** (`CART_EXPIRED`). The catalog must normalize these.
- They are free-form strings, not a typed enum — nothing prevents a typo or a
  drift between BE and FE.

### 1.2 The VALIDATION channel ALREADY EXISTS (separate from ServiceError)

- `apps/api/lib/httpx/errorhandler.go:17-25` — the single Fiber `ErrorHandler`
  routes **ozzo** `validation.Errors` to `422 {error:"validation failed", fields:{...}}`
  BEFORE it ever reaches `HandleServiceError`.
- `apps/api/lib/httpx/errorhandler.go:31-53` — `validationFields` flattens nested
  ozzo errors into `field→message`, keyed by json tag (matches the request body).
- `apps/api/lib/httpx/response.go:29-33,72-87` — `ValidationEnvelope{Error, Fields}`
  and the `validator/v10` variant.

**Conclusion:** VALIDATION is already a first-class, separate category on the wire.
The plan does **not** move it onto `ServiceError`; it only needs a category **tag**
for logging/analytics so all four categories are observable uniformly (see §2.4).

### 1.3 Logging already carries most standard fields

- `apps/api/lib/logger/logger.go:15-29,46` — zap production/dev config; every log
  carries `env`; `ts` is ISO8601 (`EncoderConfig.TimeKey = "ts"`), and zap adds
  `level`, `caller`, `msg`.
- `apps/api/lib/logger/context.go:51-69` — `logger.From(ctx, base)` enriches with
  `store_id`, `store_slug`, `trace_id` (OTEL, set by telemetry middleware).
- `apps/api/lib/logger/context.go:31-46` — `WithStore(ctx, id, slug)` for
  workers/webhooks that resolve the store outside HTTP middleware.
- `logger.From(ctx, ...)` is already the dominant pattern — **578** call sites
  (`grep -rc` across `apps/api/internal`).

**Missing (the initiative's logging asks):**
1. A `::func::` marker convention — no helper exists; today messages are ad hoc
   free text (e.g. `apps/api/internal/checkout/service.go:657` `"payment integration failed checkout config, trying next"`).
2. A structured `metadata` bag helper — today callers hand-append `zap.Field`s
   inline per call.
3. An error-**category** tag on logs — nothing classifies a log/error into the
   four categories.

### 1.4 Error surface today: REPOSITORY / INFRASTRUCTURE collapse to 500

- `Err*` constructors (`apps/api/lib/httpx/errors.go:30-36`):
  `ErrBadRequest(400)`, `ErrNotFound(404)`, `ErrForbidden(403)`,
  `ErrConflict(409)`, `ErrGone(410)`, `ErrUnprocessable(422)`, `ErrInternal(500)`.
- DB/persistence errors are almost never wrapped — services do
  `if err != nil { return nil, err }` (e.g. `apps/api/internal/checkout/service.go:340-342, 358-360, 367-369`).
  A raw pgx error bubbles up, misses both `*ServiceError` and `*fiber.Error`
  branches, and lands in `logUnhandledError` → `500 "internal server error"`
  (`apps/api/lib/httpx/response.go:130-131`). So **REPOSITORY** and
  **INFRASTRUCTURE** are today an undifferentiated 500 with no category, no code,
  and only whatever the raw error string says.

### 1.5 Enforcement precedent already exists

- `apps/api/internal/conventions/convention_test.go` — a **convention ratchet**:
  `TestEveryRequestDTOHasValidate` freezes a `baselineWithoutValidate` set and
  fails only when the set of violations **grows** (and complains if the baseline
  rots). This is the exact pattern to reuse for "domain errors without a code"
  (see §7d).

### Gap summary table

| Need | Exists today | Gap |
|---|---|---|
| BE→FE machine code on wire | `ServiceError.Reason` → `Envelope.reason` (`errors.go:15`, `response.go:107`) | Underused (3 sites); untyped string; lower_snake not UPPER_SNAKE |
| VALIDATION channel | `ValidationEnvelope` 422+fields (`errorhandler.go:17-25`) | No category tag for logs/analytics |
| DOMAIN errors | `Err*` + `WithReason` | Most set no code; no category |
| REPOSITORY errors | collapse to generic 500 | No category, no code, no rich server log |
| INFRASTRUCTURE errors | collapse to generic 500 | Same as above |
| Log standard fields | `ts`, `trace_id`, `store_*`, `env`, `level`, `caller`, `msg` | No `::func::` marker, no `metadata` bag helper, no category tag |
| Enforcement | `convention_test.go` ratchet (validation) | No ratchet for error codes |

---

## 2. Proposed error model (the 4 categories)

### 2.1 Category type

New file `apps/api/lib/httpx/category.go`:

```go
package httpx

// Category classifies every backend error into one of four buckets. It drives
// server-side logging/analytics and, for DOMAIN, gates whether a stable Code is
// expected. It is NOT sent to the client as-is (the client sees Code + HTTP
// status); it is a server-side observability tag.
type Category string

const (
    CategoryDomain         Category = "DOMAIN"         // business-rule violation (cart expired, coupon not active)
    CategoryValidation     Category = "VALIDATION"     // data-quality (missing/invalid field) — lives on the ozzo/fields channel
    CategoryRepository     Category = "REPOSITORY"     // DB / persistence failure
    CategoryInfrastructure Category = "INFRASTRUCTURE" // network / external service / infra
)
```

### 2.2 Decision — ADD a `Category` field to `ServiceError` (don't derive it)

**Chosen: add a field.** `ServiceError` becomes:

```go
type ServiceError struct {
    Code     int      // HTTP status (unchanged name/semantics)
    Message  string   // human message (unchanged)
    Reason   string   // stable machine code (unchanged; typed via Code, see §3)
    Category Category // NEW — DOMAIN | REPOSITORY | INFRASTRUCTURE (never VALIDATION here)
}
```

**Why a field, not derivation:**
- HTTP status is a lossy proxy for category. `422` is used today for both
  DOMAIN business rules (`"carrinho expirado"`, `checkout/service.go:347`) and
  for semantic input problems (`"invalid email format"`, `invitation/types.go:40`).
  You cannot derive DOMAIN vs VALIDATION from `422`. A `500` could be REPOSITORY
  or INFRASTRUCTURE — also underivable.
- A field lets constructors set it explicitly at the throw site, where the author
  knows the true category, and lets logging read it for free.
- Backward compatible: the field is optional. Existing `Err*` helpers keep
  working; a zero `Category` means "unclassified" and is treated as DOMAIN for
  4xx / INTERNAL for 5xx (see §2.3 default rule), so nothing breaks on day one.

VALIDATION is deliberately **never** stored on `ServiceError.Category` because
validation never becomes a `ServiceError` — it stays on the ozzo/fields channel
(`errorhandler.go:17-25`). Its category tag is applied at the log site in the
error handler (§2.4), not on the error object.

### 2.3 REPOSITORY / INFRASTRUCTURE: rich server-side, generic to the client

The rule: **categorize + code + log richly on the server, but return a generic
`INTERNAL` code and generic message to the client.** Never leak a pgx string, a
provider URL, or a driver error to the FE.

New constructors (in `errors.go`, see §4) produce a `ServiceError` with
`Code: 500`, `Category: REPOSITORY|INFRASTRUCTURE`, a generic client
`Message: "internal server error"`, and `Reason: INTERNAL` — while carrying the
**original** error wrapped for the server log:

```go
// RepositoryError wraps a persistence failure. Client sees a generic 500 +
// INTERNAL; the wrapped cause + category are logged server-side only.
func RepositoryError(cause error, op string) error
// InfrastructureError wraps an external-service/network failure. Same contract.
func InfrastructureError(cause error, op string) error
```

`HandleServiceError` change (`response.go:89-108`): when `se.Category` is
`REPOSITORY`/`INFRASTRUCTURE` (or any `se.Code >= 500`), keep the current
behavior — call `logUnhandledError` (which already logs `zap.Error(err)` with the
full wrap chain, `response.go:141-152`) — but **additionally** log the category
and code, and **overwrite** the client-facing `Message`/`Reason` to the generic
`"internal server error"` / `INTERNAL` regardless of what the throw site put in
`Message`. This guarantees no internal detail leaks even if a future caller is
careless. Concretely:

```go
if se.Code >= 500 {
    logUnhandledError(c, err, reqID)                 // full cause chain, existing
    return c.Status(se.Code).JSON(Envelope{
        Error:     "internal server error",           // never se.Message for 5xx
        RequestID: reqID,
        Reason:    string(CodeInternal),               // "INTERNAL"
    })
}
```

`logUnhandledError` gains two fields: `zap.String("category", string(se.Category))`
and `zap.String("code", se.Reason)` when `se` is a `*ServiceError` (fall back to
`INFRASTRUCTURE`/`INTERNAL` for non-ServiceError unexpected errors, which are by
definition uncategorized infra/bug failures).

### 2.4 VALIDATION stays on its channel, gains a category tag

`ErrorHandler` (`errorhandler.go:17-25`) is unchanged in routing. When it emits a
`ValidationEnvelope`, add one log line (today validation 422s are silent) tagged
`category=VALIDATION`, `code=VALIDATION_FAILED`, with the offending `fields`
count in the metadata bag. Wire shape to the FE is unchanged (`{error, fields}`).

### 2.5 Category → HTTP status → client code (mapping table)

| Category | Typical throw | HTTP | Client `reason` (Code) | Client `message` | Client sees `fields`? |
|---|---|---|---|---|---|
| VALIDATION | ozzo `Validate()` / value-object parse | 422 | `VALIDATION_FAILED` (implicit) | `"validation failed"` | yes |
| DOMAIN | business rule in service/domain | 400/403/404/409/410/422 | specific code, e.g. `CART_EXPIRED` | human, localizable server msg | no |
| REPOSITORY | DB/persistence | 500 | `INTERNAL` (generic) | `"internal server error"` | no |
| INFRASTRUCTURE | external svc / network | 500 (or 502/503) | `INTERNAL` (generic) | `"internal server error"` | no |

---

## 3. Error-code catalog

### 3.1 Typed `Code` and where it lives

New file `apps/api/lib/httpx/codes.go`. Use a typed string so the compiler and the
ratchet (§7d) can reason about it:

```go
package httpx

// Code is a stable, UPPER_SNAKE_CASE machine code the FE maps to a rendered
// message. It travels on the wire as ServiceError.Reason → Envelope.reason.
// Adding a code here is the ONLY sanctioned way to introduce a new FE-facing
// reason — this file is the single source of truth mirrored by livecart-fe.
type Code string

func (c Code) String() string { return string(c) }
```

Rationale for `httpx` (not a new package): the code is a property of the HTTP
error contract that `httpx` already owns (`Envelope.Reason`), and every domain
already imports `httpx`. A separate `errcodes` package would create an import
churn with no benefit and risk an import cycle with `httpx`.

### 3.2 Naming convention

- `UPPER_SNAKE_CASE`, ASCII, no domain prefix punctuation beyond `_`.
- Group by domain concept, not by HTTP status: `CART_*`, `PAYMENT_*`, `COUPON_*`,
  `SHIPPING_*`, `INVITATION_*`, `STORE_*`, `ORDER_*`.
- One generic catch-all per non-domain category: `INTERNAL` (repo+infra to client),
  `VALIDATION_FAILED` (implicit on the validation channel).
- Codes are **stable forever** once shipped (the FE keys off them). Renames are a
  breaking change requiring a coordinated FE change.

### 3.3 Starter catalog (derived from REAL existing throw sites)

Grounded in `grep` of `ErrConflict/ErrGone/ErrUnprocessable/WithReason` across
`apps/api/internal`. Each row cites the source throw site.

```go
const (
    // --- generic (non-domain, client-safe) ---
    CodeInternal        Code = "INTERNAL"          // repo/infra → client
    CodeValidationFailed Code = "VALIDATION_FAILED" // ozzo/fields channel

    // --- CART / CHECKOUT ---
    CodeCartExpired      Code = "CART_EXPIRED"       // checkout/service.go:347,102,108,561,728; coupon/service.go:240
    CodeCartAlreadyPaid  Code = "CART_ALREADY_PAID"  // checkout/service.go:350,564,731; coupon/service.go:237,336; shipping.go:598
    CodeCartNotPayable   Code = "CART_NOT_PAYABLE"   // checkout/service.go:354,568,735 ("não está disponível para checkout")
    CodeCartEmpty        Code = "CART_EMPTY"         // coupon/service.go:246 ("cart is empty")
    CodeCartNoItemsPayable Code = "CART_NO_ITEMS_PAYABLE" // checkout/service.go:392,587 ("não tem itens disponíveis")

    // --- PAYMENT ---
    CodePaymentNotConfigured Code = "PAYMENT_NOT_CONFIGURED" // checkout/service.go:646-649,709-712 (was "payment_not_configured")
    CodePaymentUnavailable   Code = "PAYMENT_UNAVAILABLE"    // checkout/service.go:686-689 (was "payment_unavailable")
    CodePaymentLinkFailed    Code = "PAYMENT_LINK_FAILED"    // checkout/service.go:448 ("erro ao gerar link de pagamento")

    // --- SHIPPING ---
    CodeShippingQuoteExpired  Code = "SHIPPING_QUOTE_EXPIRED"  // shipping.go:612 ("cotação expirou")
    CodeShippingNotQuoted     Code = "SHIPPING_NOT_QUOTED"     // shipping.go:609 ("primeiro cote o frete")
    CodeShippingCepRequired   Code = "SHIPPING_CEP_REQUIRED"   // shipping.go:601 ("CEP é obrigatório")
    CodeShippingOptionMissing Code = "SHIPPING_OPTION_MISSING" // shipping.go:628 ("opção de frete não encontrada")
    CodeShippingCartEmpty     Code = "SHIPPING_CART_EMPTY"     // shipping.go:401 ("nenhum item no carrinho para cotar")

    // --- COUPON ---
    CodeCouponAlreadyExists   Code = "COUPON_ALREADY_EXISTS"   // coupon/service.go:88
    CodeCouponHasCoupon       Code = "CART_HAS_COUPON"         // coupon/service.go:243 ("cart already has a coupon")
    CodeCouponNotActive       Code = "COUPON_NOT_ACTIVE"       // coupon/service.go:257
    CodeCouponNotYetValid     Code = "COUPON_NOT_YET_VALID"    // coupon/service.go:261
    CodeCouponExpired         Code = "COUPON_EXPIRED"          // coupon/service.go:264
    CodeCouponFullyRedeemed   Code = "COUPON_FULLY_REDEEMED"   // coupon/service.go:267
    CodeCouponRedeemed        Code = "COUPON_REDEEMED"         // coupon/service.go:159 (delete guard)
    CodeCouponCodeRequired    Code = "COUPON_CODE_REQUIRED"    // coupon/service.go:220

    // --- STORE ---
    CodeStoreAlreadyExists Code = "STORE_ALREADY_EXISTS" // store/service.go:66
    CodeStoreSlugInUse     Code = "STORE_SLUG_IN_USE"    // store/service.go:75
    CodeUserNotSynced      Code = "USER_NOT_SYNCED"      // store/service.go:56; invitation/service.go:169

    // --- INVITATION ---
    CodeInvitationExists     Code = "INVITATION_ALREADY_EXISTS" // invitation/service.go:60
    CodeInvitationExpired    Code = "INVITATION_EXPIRED"        // invitation/service.go:127,155
    CodeInvitationNotPending Code = "INVITATION_NOT_PENDING"    // invitation/repository.go:65; service.go:240
    CodeOwnerOfOtherStore    Code = "OWNER_OF_OTHER_STORE"      // invitation/service.go:188

    // --- ORDER ---
    CodeOrderAddressLocked   Code = "ORDER_ADDRESS_LOCKED"   // order/service.go:465,471 (after payment/shipment)
    CodeOrderCheckoutLocked  Code = "ORDER_CHECKOUT_LOCKED"  // order/service.go:493,499 (regenerate on paid/shipped)
    CodeNfeSyncUnavailable   Code = "NFE_SYNC_UNAVAILABLE"   // order/service.go:83
    CodeErpRetryUnavailable  Code = "ERP_RETRY_UNAVAILABLE"  // order/service.go:97
)
```

> The owner's headline examples — `CART_EXPIRED` and `PAYMENT_NOT_CONFIGURED` —
> are both in the starter set and both back real throw sites.

Note: `PAYMENT_UNAVAILABLE` at `checkout/service.go:686` is arguably
INFRASTRUCTURE ("nenhuma integração de pagamento está respondendo") rather than
DOMAIN — but because it is a **known, recoverable, buyer-actionable** state
surfaced as a 422 today, keep it a DOMAIN-coded 422 (client can retry / pick
another method) rather than a generic 500 INTERNAL. This is a judgement call worth
confirming (see §8).

---

## 4. Constructor ergonomics

Make "set a code + category" the easy path, without breaking the existing `Err*`
helpers or `WithReason`.

### 4.1 New primary constructors (additive)

In `apps/api/lib/httpx/errors.go`:

```go
// DomainError is the primary way to raise a business-rule error with a stable
// FE code. status is the HTTP status (use the ErrKind* consts below for intent).
func DomainError(status int, code Code, msg string) error {
    return &ServiceError{Code: status, Message: msg, Reason: string(code), Category: CategoryDomain}
}

// RepositoryError / InfrastructureError: see §2.3 — generic to client, rich to log.
func RepositoryError(cause error, op string) error {
    return &ServiceError{Code: 500, Message: "internal server error",
        Reason: string(CodeInternal), Category: CategoryRepository, cause: cause, op: op}
}
func InfrastructureError(cause error, op string) error {
    return &ServiceError{Code: 500, Message: "internal server error",
        Reason: string(CodeInternal), Category: CategoryInfrastructure, cause: cause, op: op}
}
```

`ServiceError` gains unexported `cause error` and `op string` for the server log,
plus `func (e *ServiceError) Unwrap() error { return e.cause }` so `errors.Is/As`
and `zap.Error` see the real chain in `logUnhandledError`.

### 4.2 Keep `Err*` + `WithReason` (migration-friendly, non-breaking)

- The seven `Err*` helpers (`errors.go:30-36`) stay. They produce a
  `Category`-less DOMAIN-ish error → default rule (§2.3) keeps current behavior.
- `WithReason(err, reason)` stays. To ease the transition it gains a typed sibling:

```go
func WithCode(err error, code Code) error { return WithReason(err, string(code)) }
```

So a one-line migration of an existing throw is:

```go
// before
return nil, httpx.ErrUnprocessable("carrinho expirado")
// after (minimal, non-breaking)
return nil, httpx.WithCode(httpx.ErrUnprocessable("carrinho expirado"), httpx.CodeCartExpired)
// or (preferred, sets category too)
return nil, httpx.DomainError(422, httpx.CodeCartExpired, "carrinho expirado")
```

Both compile against the existing pipe; the FE immediately receives
`reason:"CART_EXPIRED"`. This lets §7 migrate flows incrementally with zero
big-bang.

### 4.3 Recategorizing the three existing `WithReason` sites

`checkout/service.go:646-649,686-689,709-712` currently pass lower_snake strings.
Replace with `DomainError(422, CodePaymentNotConfigured, ...)` etc. The wire
`reason` changes from `payment_not_configured` → `PAYMENT_NOT_CONFIGURED`; this is
a **coordinated FE change** (the FE map must switch keys simultaneously) — call it
out in the rollout (§7c) and to the owner (§8).

---

## 5. Logging convention (`::func::` marker + metadata bag + category)

Additive helpers over `logger.From` — no change to the 578 existing call sites is
forced; they keep working and can be migrated opportunistically.

### 5.1 Design

New file `apps/api/lib/logger/event.go`:

```go
package logger

import (
    "context"
    "go.uber.org/zap"
)

// Event is a structured log builder that stamps the owner's ::func:: marker,
// a category tag, and a metadata bag onto a line enriched by From(ctx).
//   logger.Event(ctx, base, "createCoupon").
//       Cat(logger.CatDomain).
//       Meta("cart_id", cart.ID).Meta("code", code).
//       Warn("coupon already redeemed")
type Event struct {
    log    *zap.Logger
    fn     string
    cat    string
    fields []zap.Field
}

func Event(ctx context.Context, base *zap.Logger, fn string) *Event {
    return &Event{log: From(ctx, base), fn: "::" + fn + "::"}
}
func (e *Event) Cat(c string) *Event { e.cat = c; return e }
func (e *Event) Meta(k string, v any) *Event {
    e.fields = append(e.fields, zap.Any(k, v)); return e
}
func (e *Event) fieldset() []zap.Field {
    out := make([]zap.Field, 0, len(e.fields)+2)
    out = append(out, zap.String("fn", e.fn))
    if e.cat != "" { out = append(out, zap.String("category", e.cat)) }
    return append(out, e.fields...)
}
func (e *Event) Info(msg string)  { e.log.Info(msg, e.fieldset()...) }
func (e *Event) Warn(msg string)  { e.log.Warn(msg, e.fieldset()...) }
func (e *Event) Error(msg string) { e.log.Error(msg, e.fieldset()...) }
```

Category string constants mirror `httpx.Category` values (`"DOMAIN"`,
`"VALIDATION"`, `"REPOSITORY"`, `"INFRASTRUCTURE"`) exported from `logger` as
`CatDomain` etc. to avoid a `logger`→`httpx` import (keeps `logger` a leaf).

- `fn` field holds the `::func::` marker (owner's requested format). It is
  distinct from zap's auto `caller` (file:line) — `caller` is the physical site,
  `fn` is a stable, greppable logical name the author chose.
- `Meta(k, v)` is the metadata bag. Repeated calls accumulate; `zap.Any` keeps it
  ergonomic. (A stricter typed variant can be added later if `zap.Any` allocation
  shows up in profiles — not a launch concern.)
- Standard fields (`ts`, `trace_id`, `store_*`, `env`, `level`, `msg`) come for
  free via `From(ctx, base)` and the base zap config — unchanged.

### 5.2 Before / after (JSON)

Before (`checkout/service.go:657`, today):

```json
{"level":"warn","ts":"2026-07-22T14:03:01.221Z","caller":"checkout/service.go:657",
 "msg":"payment integration failed checkout config, trying next","env":"staging",
 "store_id":"st_123","store_slug":"acme","trace_id":"abc","cart_id":"c_9","integration_id":"int_2","provider":"pagarme","error":"..."}
```

After (same site, migrated):

```json
{"level":"warn","ts":"2026-07-22T14:03:01.221Z","caller":"checkout/service.go:657",
 "msg":"payment integration failed checkout config, trying next","env":"staging",
 "store_id":"st_123","store_slug":"acme","trace_id":"abc",
 "fn":"::resolveCheckoutIntegration::","category":"INFRASTRUCTURE",
 "cart_id":"c_9","integration_id":"int_2","provider":"pagarme","error":"..."}
```

Everything is additive: existing fields stay, `fn` + `category` appear, `Meta`
values land as siblings (same shape as today's inline `zap.String`).

### 5.3 Error-handler log gains category/code

Per §2.3/§2.4, `logUnhandledError` (`response.go:137-153`) and the 4xx `Warn`
branch (`response.go:99-105`) add `zap.String("category", ...)` and
`zap.String("code", se.Reason)`. This makes every error line queryable by
category in the aggregator without touching call sites.

---

## 6. FE contract (exact JSON the FE receives)

The wire shapes are **unchanged** except that `reason` becomes populated and
UPPER_SNAKE. FE work (a `code → message` map) is a separate task in
`livecart-fe`; it MUST layer per its Service→Hook→UI convention
(`/home/carluz_teles/livecart-fe/.claude/CLAUDE.md`) — the code→message map is a
Service/config concern; components only render the resolved message.

### 6.1 DOMAIN error

```jsonc
// HTTP 422 (or 400/403/404/409/410 per throw)
{
  "error": "carrinho expirado",   // human fallback (server-localized, do NOT branch on it)
  "reason": "CART_EXPIRED",         // <-- FE branches on this
  "requestId": "req_abc123"
}
```

FE maps `reason` → localized message; falls back to `error` if the code is unknown
(forward-compatible with codes the FE hasn't shipped a message for yet).

### 6.2 VALIDATION error (unchanged channel)

```jsonc
// HTTP 422
{
  "error": "validation failed",
  "fields": { "email": "must be a valid email address", "role": "cannot be blank" }
}
```

No `reason` here; the FE renders field-level errors from `fields` (keyed by json
tag). Category `VALIDATION` is server-log-only.

### 6.3 REPOSITORY / INFRASTRUCTURE error (generic to client)

```jsonc
// HTTP 500
{
  "error": "internal server error",
  "reason": "INTERNAL",
  "requestId": "req_abc123"
}
```

No internal detail. The FE shows a generic "something went wrong" and surfaces
`requestId` for support correlation. The real cause + category live only in the
server log.

### 6.4 Code registry mirror

Ship a machine-readable mirror of the catalog (e.g. generate `error-codes.json`
from `codes.go` in CI, or hand-maintain a `livecart-fe` constants file) so the FE
and BE never drift. Decision on generate-vs-manual left to §8.

---

## 7. Rollout / migration plan (phased, non-breaking)

### Phase A — Land the model + helpers (no behavior change)
1. Add `apps/api/lib/httpx/category.go` (Category type/consts).
2. Add `apps/api/lib/httpx/codes.go` (`Code` type + catalog from §3.3).
3. Extend `ServiceError` with `Category`, unexported `cause`/`op`, `Unwrap()`
   (`errors.go`). Add `DomainError`, `RepositoryError`, `InfrastructureError`,
   `WithCode` constructors.
4. Update `HandleServiceError`/`logUnhandledError` (`response.go`) to (a) force
   generic message+`INTERNAL` for any `>=500`, (b) log `category`/`code`.
5. Add `apps/api/lib/logger/event.go` (`Event` builder + `Cat*` consts).
6. No call site changes yet → existing tests stay green; wire behavior for
   already-coded sites unchanged except the 5xx message hardening.

### Phase B — Catalog + adopt on the highest-value flows first
7. Migrate the **three existing `WithReason` sites** in checkout to
   `DomainError(..., CodePaymentNotConfigured/…)` (coordinate FE key rename
   lower→UPPER — see §8).
8. Migrate the **cart/payment/checkout** DOMAIN throws
   (`checkout/service.go`, `checkout/shipping.go`) to `DomainError` with the
   `CART_*` / `PAYMENT_*` / `SHIPPING_*` codes. These are the buyer-facing,
   revenue-path errors the owner cares about most.
9. Wrap DB access in these flows with `RepositoryError(err, "op")` and external
   provider calls with `InfrastructureError(err, "op")` — starting with the
   `if err != nil { return nil, err }` sites in `checkout/service.go` (e.g.
   `:340-342, 358-360, 367-369`).

### Phase C — Broaden
10. Migrate `coupon`, `order`, `store`, `invitation` DOMAIN throws using the
    codes already reserved in §3.3.
11. Opportunistically migrate hot log lines to `logger.Event(...)` with `::func::`
    markers and category tags (start with the error/warn lines in checkout).

### Phase D — Enforce with a convention ratchet (precedent: `convention_test.go`)
12. Add a ratchet test in `apps/api/internal/conventions/` that AST-walks
    `internal/**` for `httpx.ErrConflict/ErrGone/ErrUnprocessable/ErrBadRequest/
    ErrNotFound/ErrForbidden` call sites **not** wrapped in `WithCode`/`WithReason`
    and **not** produced by `DomainError`, and freezes them in a
    `baselineWithoutCode` map — failing only when the set **grows** (identical
    shape to `TestEveryRequestDTOHasValidate`, `convention_test.go:76-105`). This
    lets the codebase ratchet toward "every DOMAIN error has a stable code"
    without a big-bang migration and without letting new uncoded errors sneak in.
13. Optional second ratchet: assert every `Code` in `codes.go` is UPPER_SNAKE and
    unique (cheap reflection/string test).

Each phase ships independently; nothing in A/B/C is breaking except the
coordinated lower→UPPER `reason` rename in step 7, which is why it is called out
explicitly and gated on a matching FE change.

---

## 8. Open questions / decisions for the owner

1. **lower→UPPER `reason` rename.** Today the FE (if it branches at all) sees
   `payment_not_configured`/`payment_unavailable`. Adopting UPPER_SNAKE renames
   these on the wire. Confirm we do the coordinated FE rename now (step 7) vs.
   emit both temporarily. Recommendation: coordinated rename — only 3 sites, and
   the FE map is small.

2. **`PAYMENT_UNAVAILABLE` category.** "Nenhuma integração respondendo"
   (`checkout/service.go:686`) is technically INFRASTRUCTURE but is surfaced as a
   buyer-actionable 422 DOMAIN code today. Keep it a DOMAIN 422 (recommended,
   preserves retry UX) or make it a generic 500 INTERNAL? Recommendation: keep
   DOMAIN 422.

3. **Message localization.** DOMAIN `Message` today is human PT-BR text. With
   codes, is the server message still the source of truth (FE falls back to it) or
   does the FE own all copy and the server message becomes debug-only? Affects
   whether we invest in server-side i18n. Recommendation: FE owns copy; server
   message is a fallback/debug string.

4. **Catalog mirror to FE.** Generate `error-codes.json` from `codes.go` in CI, or
   hand-maintain a constants file in `livecart-fe`? Recommendation: generate in CI
   to prevent drift.

5. **`op` taxonomy for repo/infra.** Should `RepositoryError(cause, op)` use a
   free-form `op` string or a small enum of operations for cleaner aggregation?
   Recommendation: free-form to start, revisit if dashboards need it.

6. **Ratchet scope.** Should the Phase D ratchet also require a `::func::` marker
   on error/warn logs, or only require codes on DOMAIN errors? Recommendation:
   codes-only first (higher value, less churn); logging markers stay a soft
   convention.

---

## Appendix — files this initiative touches

New:
- `apps/api/lib/httpx/category.go`
- `apps/api/lib/httpx/codes.go`
- `apps/api/lib/logger/event.go`
- `apps/api/internal/conventions/*_test.go` (new ratchet)

Modified:
- `apps/api/lib/httpx/errors.go` (ServiceError fields, new constructors, Unwrap, WithCode)
- `apps/api/lib/httpx/response.go` (HandleServiceError 5xx hardening + category/code log fields)
- Domain services, starting with `apps/api/internal/checkout/{service.go,shipping.go}`, then `coupon`, `order`, `store`, `invitation` (incremental, per §7).

Unchanged contracts:
- `Envelope` / `ValidationEnvelope` wire shapes (`response.go:20-33`).
- `ErrorHandler` validation routing (`errorhandler.go:17-25`).
- `logger.From` / `WithStore` behavior (`context.go`).

---

## Appendix B — Metadata catalog (empirical, derived from what flows today)

Feeds §5 (the metadata bag). Not a wishlist: the tiers below are ranked by how
often each key is ALREADY logged ad-hoc across `apps/api/internal` (frequency in
parentheses = `zap.String/Int` call sites). That frequency is the proof the data
is present in the flow and worth capturing — the initiative's job is to attach it
CONSISTENTLY (uniform keys, no drift) via the metadata bag instead of hand-rolling
it per call.

### Tier 0 — Base context (already emitted; keep, add only the `::func::` marker)
Emitted by `logger.From` + `identityFields` today: `trace_id`, `span_id`,
`store_id`, `store_slug`, `env`, `ts`, `level`, `caller`; on error also `user_id`,
`request_id`, `path`, `method`, `http_status`. No new work beyond §5.1.

### Tier 1 — Correlation IDs (the join keys — attach whenever in scope)
These are the graph that reconstructs any entity's timeline across functions:
`store_id → event_id → session_id → cart_id → {product_id, payment_id, order_id, shipment_id, waitlist_item_id}`.

| Key | Freq | Notes |
|---|---|---|
| **`cart_id`** | **208** | The spine of commerce. Spans comment→cart→checkout→payment→order→shipment. **Single highest-value field — every cart-touching log must carry it.** |
| `event_id` | 56 | live/post/story event |
| `product_id` / `external_product_id` | 49 / 14 | local + ERP (Tiny) id |
| `integration_id` | 42 | pairs with `provider` |
| `comment_id` | 35 | idempotency key of the inverted comment flow |
| `media_id` | 18 | IG post/story/live media |
| `waitlist_item_id` | 18 | |
| `payment_id` | 14 | gateway charge/order id |
| `external_order_id` | 14 | ERP order id |
| `platform_user_id` | 13 | buyer (IG scoped id) |
| `order_id` | 11 | |
| `session_id` / `account_id` / `erp_movement_id` | 8 / 8 / 8 | |
| `shipment_id` / `recipient_id` / `sender_id` / `contact_id` | 6 / 6 / 5 / 6 | shipping + messaging |
| `customer_id`, `coupon_id`, `tracking_token`, `checkout_id` | present in flow, **under-logged** | good metadata gaps to add |

### Tier 2 — Domain attributes (the "what happened" context)
`status` / `state` (42), `provider` (15) [pagarme/mercadopago/stripe/tiny],
`type` (12), `payment_method` / `method` (14) [card/pix], `username` / `handle`
(18) [buyer], `quantity` / `delta` (14) [stock moves], `installments` (6),
`status_detail` (5), `keyword`.

### Tier 3 — Money (structured NUMERIC fields — never interpolate into the message)
`amount_cents` (8), `net_amount_cents` (7), `fee_amount_cents` (7). Keep as
`zap.Int64` so aggregators can sum/alert. A payment log carrying
`{cart_id, payment_id, provider, amount_cents, status}` is fully queryable.

### Tier 4 — Error-specific (the error system)
`category` (DOMAIN/VALIDATION/REPOSITORY/INFRASTRUCTURE), `code` (`CART_EXPIRED`…),
and the wrapped `cause` (server-side only, via `Unwrap()` — never sent to the FE).

### Recommendations
1. **Bind cart_id first.** It's the correlation key of the whole commerce funnel;
   the metadata bag should propagate it through every function in a cart-touching
   request/worker so an entity's timeline is one query.
2. **Standardize the keys above** (already snake_case and consistent) into the
   metadata helper so they stop being re-typed per call site (drift risk).
3. **Money is always numeric metadata**, never text in the message.
4. **Fill the under-logged gaps** (`customer_id`, `coupon_id`, `tracking_token`,
   `checkout_id`) — they're in scope in the payment/order flows but rarely logged,
   and they close correlation holes (e.g. `tracking_token` links order→public page).
5. Provider-originated flows: always carry `provider` + `integration_id` so a
   pagarme vs mercadopago vs stripe issue is filterable.

---

## Appendix C — Request/response payload logging in the interceptor

Valuable for debugging (especially failures — you see the exact body that broke),
but it CANNOT be a naive "dump the body". The checkout/payment/onboarding flows
carry PII and secrets, so raw payload logging would ship an LGPD/PCI incident
straight into the log aggregator. Design it with guardrails from day one.

### Current state
`httpx.RequestLogger` (`middleware.go:14`, mounted at `main.go:322`) already emits
one access-log line per request — `method, path, status, duration_ms, request_id,
ip, user_id, store_id, store_slug` — but NO bodies. This is the natural home to
extend (it already straddles req→`c.Next()`→resp).

### What flows through these bodies (must be handled)
- **PII (LGPD):** `customerDocument` (CPF/CNPJ), `customerName`, `customerPhone`,
  `email`, `shippingAddress` (street/number/zip), buyer `username`/`handle`.
- **Payment secrets (PCI):** card `token`, `cardNumber`, `cvv`, `expiry`,
  `cardholderName` (pagarme transparent checkout posts these).
- **Auth secrets (headers, not body):** `Authorization` (Bearer/Clerk), the dev
  bypass `X-Dev-User-ID`, webhook signatures.
- **Provider webhooks:** pagarme / mercadopago / Stripe / Clerk / Instagram post
  full customer + payment blobs to our webhook endpoints.

### Design (recommended)
1. **Redact, don't drop.** Walk the JSON and replace a denylist of sensitive keys
   with `"[REDACTED]"` (case-insensitive): `document|cpf|cnpj|card*|cardnumber|
   number|cvv|cvc|securitycode|expiry|password|token|authorization|secret|
   apikey|access_token`. Card PAN/CVV are dropped entirely (never even partial).
2. **PII posture:** for `email|phone|customerdocument|zip*|address*`, prefer
   masking (e.g. `ma***@x.com`, last-2 of CPF) over full value — or redact fully
   until a retention/masking policy is set. Decide with the owner (open question).
3. **Never log auth headers.** Log a fixed allowlist of headers only (`content-type`,
   `x-request-id`, `user-agent`); never `Authorization` / `Cookie` / signatures.
4. **Cap size.** Truncate each body to e.g. 8–16 KB with a `"…(truncated N bytes)"`
   marker — product lists / image arrays / webhook blobs get large.
5. **Content-type gate.** Only capture `application/json`. Skip `multipart/*`
   (image uploads) and binary.
6. **Volume/cost control.** Full bodies at **debug level or on-error (status ≥ 400)**
   — not always-on `info`, which would 10× log spend and noise. Redacted request
   body can ride the existing access line; response body only on error/debug.
7. **Body access in Fiber.** Request: `c.Body()` (already buffered). Response:
   `c.Response().Body()` after `c.Next()`. Redact a COPY — never mutate the live
   request/response buffers.

### Where it plugs into the error system
On a 4xx/5xx, the interceptor attaches the (redacted) request body as a
`request_body` metadata field on the same log line that already carries
`category` + `code` + the Tier-1 correlation IDs — so a failed request is one log
entry with the code, the correlating `cart_id`/`payment_id`, AND the offending
(sanitized) payload. That is the debugging payoff, made safe.

### Open questions
- PII: full-redact vs. mask vs. field-level allowlist (needs LGPD sign-off).
- Response-body logging always-on-error vs. debug-only (cost).
- Log retention window for entries carrying (masked) PII.
