---
name: api-domain-convention
description: Convenção obrigatória para escrever/alterar um domínio HTTP na API (apps/api/internal/*). Define o fluxo em camadas Request→Validate→ToInput→Usecase→Response, o contrato de erro do httpx (400/401/403/404/422 via ErrorHandler central) e as TRÊS camadas de validação (sintática ozzo, semântica via value object no ToInput, invariante no domínio/service). Usar sempre que for criar um novo domínio, adicionar/alterar um endpoint ou handler, criar um Request DTO, mexer em validação de entrada, ou revisar um PR que toca em handler/types de um domínio. Molde vivo de referência: internal/member.
---

# Convenção de domínio HTTP — API LiveCart

Todo domínio em `apps/api/internal/<dominio>/` segue o mesmo fluxo. Antes de escrever,
**abra `internal/member/` como molde vivo** (`handler.go` + `types.go`) — é o padrão-ouro.
Esta skill descreve a regra; o member é o exemplo canônico a copiar.

## O fluxo em camadas (handler)

Um handler que recebe body faz exatamente 4 passos e nada de lógica de negócio:

```go
func (h *Handler) UpdateRole(c *fiber.Ctx) error {
	var req UpdateMemberRoleRequest
	if err := httpx.BindAndValidate(c, &req); err != nil { // 1. parse + validação sintática
		return err
	}
	input, err := req.ToInput(httpx.GetStoreID(c), c.Params("memberId"), httpx.GetStoreUserID(c)) // 2. DTO→Input (+ VO)
	if err != nil {
		return err
	}
	member, err := h.svc.UpdateRole(c.UserContext(), input) // 3. usecase retorna ENTIDADE de domínio
	if err != nil {
		return err
	}
	return httpx.OK(c, NewMemberResponse(member)) // 4. mapeia entidade→Response
}
```

Peças obrigatórias, em `types.go`:

1. **`XRequest` + `Validate() error`** — DTO de entrada com tags `json`. `Validate` é ozzo puro.
2. **`ToInput(...) (XInput, error)`** — traduz o Request no input do usecase; é aqui que se
   constroem os **value objects** (validação semântica). Retorna `error` pra um VO inválido virar 422.
3. **Service retorna a `*domain.Entity`** — nunca um DTO. A apresentação conhece o domínio; o
   domínio nunca conhece o Response.
4. **`NewXResponse(entity)`** — mapper de saída (entidade → DTO de resposta).

Sempre passar **`c.UserContext()`** ao usecase (mantém o span OTEL e o `logger.From(ctx)`).

## As TRÊS camadas de validação (não confundir)

Cada regra mora na camada certa. Errar a camada é a causa nº 1 de bug de validação aqui.

| Camada | Onde | O que valida | Como falha |
|---|---|---|---|
| **Sintática** | `Request.Validate()` (ozzo) | forma, formato, faixa, obrigatoriedade, enum | `validation.Errors` → **422 `{error, fields}`** |
| **Semântica** | `ToInput()` via value object (`vo.NewMoney`, `vo.NewRole`) | valor bem-formado mas com regra de tipo | `httpx.ErrUnprocessable(...)` → 422 |
| **Invariante** | entidade de domínio / service | regra de negócio (coerência, cross-field, estado) | `httpx.Err*` do service/domínio |

Regras de negócio (ex.: "cupom percent exige percentBps > 0") **não** vão no ozzo — vão no
service (`validateBusinessRules`) ou no domínio (`ErrInvalidPrice`). O ozzo é só o portão sintático.

## ⚠️ Gotcha do ozzo: `Min`/`Max` sem `Required` NÃO pega o zero

O ozzo **pula toda regra (exceto `Required`) quando o valor é o zero-value**. Consequência:

- `Min(0)` num int → sem problema (0 satisfaz mesmo; negativo é pego).
- **`Min(n≥1)` sozinho → floor MOLE**: um campo omitido/zerado passa batido.
  - campo **valor** (`int`): body sem o campo = `0` → **bypassa o piso silenciosamente**.
  - campo **ponteiro** (`*int`): `nil` pula de propósito (ok); só vaza se mandarem `0` explícito.

**Regra:** se um `int` de valor tem piso de negócio (`Min(n≥1)`), pareie com `Required` —
`Required` trata o `0` como vazio e rejeita, então o piso realmente segura:

```go
// ERRADO — PUT que omite o campo grava 0 e fura o mínimo:
validation.Field(&r.ExpirationMinutes, validation.Min(5), validation.Max(1440)),
// CERTO:
validation.Field(&r.ExpirationMinutes, validation.Required, validation.Min(5), validation.Max(1440)),
// Piso condicional (só exige quando o toggle liga a feature):
validation.Field(&r.ExpirationReminderMinutes,
	validation.When(r.SendExpirationReminder, validation.Required, validation.Min(1), validation.Max(60))),
```

## Contrato de erro

Handler **sempre `return err`** — o `httpx.ErrorHandler` central renderiza a resposta pro FE.
Nunca montar JSON de erro na mão. Helpers:

- `httpx.ErrBadRequest` → 400 · `ErrForbidden` → 403 · `ErrNotFound` → 404 · `ErrUnprocessable` → 422
- `validation.Errors` (do ozzo) → 422 `{error:"validation failed", fields:{<jsonTag>: msg}}` automático.
- Sucesso: `httpx.OK` (200), `httpx.Created` (201), `httpx.NoContent` (204).

## Checklist de PR (novo/alterado domínio HTTP)

- [ ] `Request` tem `Validate()` ozzo cobrindo forma/faixa/enum de todo campo.
- [ ] Todo `int` de valor com `Min(n≥1)` está pareado com `Required` (ou `When(...)`).
- [ ] Value objects construídos no `ToInput`, não no handler; VO inválido → `ErrUnprocessable`.
- [ ] Regra de negócio no service/domínio, **não** no ozzo.
- [ ] Service retorna `*domain.Entity`; handler mapeia com `NewXResponse`.
- [ ] Handler passa `c.UserContext()` ao usecase.
- [ ] Handler só faz `return err` (zero JSON de erro na mão).
- [ ] Teste table-driven do `Validate()`: 1 caso válido + 1 inválido por regra, asserindo a json key
      no `validation.Errors`. Para floor com `Required`, incluir caso `0 rejeitado`.
- [ ] `go build ./apps/api/...` e `go test ./apps/api/internal/<dominio>/` verdes.
