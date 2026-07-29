# Code Review — Fatia B1b (payment fast-paths)

## BLOCKING

### B1 — ProcessCardPayment e GeneratePixPayment sem teste (service_test.go) — ✅ RESOLVIDO

**Resolvido em `apps/api/internal/payment/service_test.go`:**
- `TestService_ProcessCardPayment` (`service_test.go:594`) — sucesso (assert do mapeamento campo-a-campo, incl. `CardToken → Token`, `PaymentMethodID`, `IssuerID`, `DeviceID` via captura `gotCardInput`), erro do provider (`wantHandle: process_card_payment`), erro do resolver (sem flag).
- `TestService_GeneratePixPayment` (`service_test.go:709`) — mesma estrutura (captura `gotPixInput`, `wantHandle: generate_pix_payment`).
- `TestService_CreateCheckout` (`service_test.go:240`) — cache-hit (idempotency `Found=true`, provider NÃO construído), caminho normal (Start→Complete = "completed"), e construção da `notifyURL` via `paymentProvider.Name()` (`t.Setenv WEBHOOK_BASE_URL`). Adicionado `Name() providers.ProviderName` ao `stubPaymentProvider` + `fakeIdempotencyRepo` (fake da `idempotency.Repository`).
- `TestService_GetPaymentStatus` (`service_test.go:344`) — cobertura extra do 4º método listado como não-coberto (mapeamento + `wantHandle: get_payment_status`).

`go build ./...` e `go test -race ./internal/payment/...` verdes.

<details><summary>Descrição original</summary>

**Localização:** `apps/api/internal/payment/service_test.go` — ausentes  
**Funções cobertas:** `TestService_GetProvider`, `TestService_RefundPayment`, `TestService_GetCheckoutConfig`  
**Funções NÃO cobertas:** `ProcessCardPayment`, `GeneratePixPayment`, `CreateCheckout`, `GetPaymentStatus`

O prompt de revisão designa `ProcessCardPayment` e `GeneratePixPayment` como os mais críticos. Ambos moveram lógica de mapeamento field-a-field de `providers.CardPaymentInput` e `providers.PixPaymentInput` — exatamente onde um campo esquecido muda comportamento silenciosamente. Nenhum dos dois tem teste.

`CreateCheckout` é ainda mais complexo: é a única fast-path com lógica de idempotência (Check → Start → Fail/Complete) e com construção dinâmica da `notifyURL` via `paymentProvider.Name()`. Também sem teste.

**Impacto concreto:**
- Um campo trocado em `ProcessCardPaymentInput → providers.CardPaymentInput` (ex: `CardToken` → `Token`) passaria despercebido — já que a lógica de mapeamento não é testada.
- O caminho de cache-hit da idempotência em `CreateCheckout` (json.Unmarshal → return cached) não tem coverage.
- `stubPaymentProvider` (declarado no arquivo) não implementa `Name()` — se um teste de `CreateCheckout` com `notifyURL == ""` fosse adicionado usando o stub, causaria panic no embedded nil interface. Isso indica que o test author já sabe que CreateCheckout não será testado nesta fatia.

**Fix:** Adicionar ao menos:
1. `TestService_ProcessCardPayment` — casos: sucesso (verificar campos mapeados), erro do provider (wantHandle: "process_card_payment"), erro do resolver.
2. `TestService_GeneratePixPayment` — mesma estrutura.
3. `TestService_CreateCheckout` — ao menos o caminho de cache-hit (idempotency.Found=true) e o caminho normal. Adicionar `Name() string` ao `stubPaymentProvider` para suportar o teste.

</details>

---

## ADVISORY (não bloqueantes)

### A1 — Delegação fiel: ✓
Todos os 6 métodos foram movidos com lógica idêntica. `s.GetPaymentProvider(...)` → `s.GetProvider(...)` e `s.handleProviderError(...)` → `s.resolver.HandleProviderError(...)` são equivalentes funcionais. Ordem de operações preservada. Nenhum evento era emitido nos métodos movidos.

### A2 — Import cycle: ✓
`payment` não importa `integration`; `integration` importa `payment` apenas via alias de tipo. `go list -deps` confirma sem ciclo.

### A3 — DRY: ✓
DTOs foram movidos com aliases (`type X = paymentdomain.X`), não copiados. `createProviderFromRow` e `handleProviderError` permanecem em `integration`.

### A4 — Condição de guarda da idempotência herdada (não regressão)
Em `CreateCheckout`: `if input.IdempotencyKey != "" || s.idempotency != nil` pode chamar `nil.Start()` se key != "" mas idempotency == nil. Bug pré-existente herdado do código original — não introduzido nesta fatia.

### A5 — `idempotency.Check` sem guarda nil
A primeira linha de `CreateCheckout` chama `s.idempotency.Check(...)` incondicionalmente. Se `s.idempotency == nil`, panic. Também herdado.
