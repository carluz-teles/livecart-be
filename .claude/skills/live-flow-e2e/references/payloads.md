# Payloads, seletores e dados de teste

Templates verificados contra os DTOs do backend (`internal/*/types.go`, `instagram_types.go`) e os componentes do FE.

## Webhook Instagram — comentário de live

`POST $API_BASE/api/webhooks/instagram` (público, sem assinatura). Campos obrigatórios para o roteamento: `entry[].changes[].field = "live_comments"`, `value.media.id` = `platform_live_id` do evento, `value.id` = comment_id **único**.

```json
{
  "object": "instagram",
  "entry": [
    {
      "id": "sim-account-1",
      "time": 1720000000,
      "changes": [
        {
          "field": "live_comments",
          "value": {
            "from": {
              "id": "sim_user_1",
              "username": "maria_teste",
              "self_ig_scoped_id": "sim_igsid_1"
            },
            "id": "sim-comment-<UNICO>",
            "text": "9432",
            "media": {
              "id": "<MEDIA_ID>",
              "media_product_type": "IG_LIVE"
            }
          }
        }
      ]
    }
  ]
}
```

Notas:
- `from.id` identifica o comprador — mesmo `from.id` no mesmo evento reutiliza o cart (`UNIQUE(event_id, platform_user_id)`); usuários diferentes = `from.id` diferentes.
- `self_ig_scoped_id` é usado só para DM (que falha na simulação — ok).
- Verificação Meta (GET): `curl "$API_BASE/api/webhooks/instagram?hub.mode=subscribe&hub.challenge=teste123&hub.verify_token=livecart_verify_token"` → responde `teste123`.

## API admin (bypass dev)

Headers: `-H "Authorization: Bearer dev" -H "X-Dev-User-ID: <clerk_id>"`.

**Criar produto** — `POST /api/v1/stores/:storeId/products`:
```json
{
  "name": "[E2E] Produto Teste",
  "externalSource": "manual",
  "keyword": "9432",
  "price": 4990,
  "stock": 10,
  "shipping": {
    "weightGrams": 300, "heightCm": 10, "widthCm": 15, "lengthCm": 20,
    "packageFormat": "box"
  }
}
```
`price` em centavos. Dimensões all-or-nothing (peso+altura+largura+comprimento juntos) — obrigatórias para cotação de frete.

**Criar evento** — `POST /api/v1/stores/:storeId/lives`:
```json
{
  "title": "[E2E] Live Teste",
  "type": "single",
  "platform": "instagram",
  "platformLiveId": "SIM-1720000000",
  "cartExpirationMinutes": 15,
  "cartMaxQuantityPerItem": 10,
  "pixDiscountPercent": 0
}
```
`cartExpirationMinutes`: 5–1440. Produto em destaque: `PATCH /lives/:id/active-product` com `{"productId":"<uuid>"}`.

## Checkout público (API)

**Editar itens**: `POST /api/public/checkout/:token/items` `{"productId":"<uuid>","quantity":1}` · `PATCH /items/:itemId` `{"quantity":2}` · `DELETE /items/:itemId`.

**Cotar/selecionar frete**: `POST /shipping-quote` `{"zipCode":"01310100"}` → opções com `id`+`provider`; `PUT /shipping-method` com a opção escolhida.

**PIX** — `POST /api/public/checkout/:token/pix`:
```json
{
  "email": "comprador+e2e@teste.com",
  "customerName": "Maria Teste da Silva",
  "customerDocument": "52998224725",
  "customerPhone": "11999990000",
  "shippingAddress": {
    "zipCode": "01310-100", "street": "Avenida Paulista", "number": "1000",
    "complement": "Ap 11", "neighborhood": "Bela Vista", "city": "São Paulo", "state": "SP"
  }
}
```
Resposta: `paymentId`, `qrCode` (base64), `qrCodeText`, `expiresAt`.

**Cartão** — `POST /api/public/checkout/:token/card`: mesmos campos + `"token"` (card token do provider, gerado no FE), `"installments": 1`. Aprovação síncrona dispara `OnCartPaid`.

**Status** — `GET /api/public/checkout/:token/status` → `{status, paymentStatus, paidAt, message}` (lê o banco).

## Dados de teste

| Dado | Valor | Nota |
|---|---|---|
| CPF válido | `529.982.247-25` | Passa validação de dígitos; uso corrente em exemplos BR |
| CEP real | `01310-100` | Av. Paulista, São Paulo/SP — ViaCEP autopreenche |
| E-mail | `qualquer+e2e@teste.com` | Para Clerk dev: `*+clerk_test@...` com código `424242` |

### Cartões sandbox

- **Mercado Pago** (estável/documentado): Mastercard `5031 4332 1540 6351`, Visa `4235 6477 2802 5682`, CVV `123`, validade `11/30`. O **nome do titular controla o resultado**: `APRO` = aprovado, `OTHE` = recusado, `CONT` = pendente. CPF de teste: `12345678909`.
- **Pagar.me** (test mode): consultar os cartões vigentes na doc oficial antes de usar (buscar via context7/web — os números de simulação mudam). Em test mode nenhuma cobrança real ocorre.
- Antes de pagar, confirmar que a integração usa credenciais de teste (`integrations.credentials` é cifrado — conferir com o usuário ou pelo prefixo da public key retornada em `GET /:token/config`, ex. `TEST-` no MP).

## Seletores da UI (Playwright)

### § Login (Clerk em `$FE_BASE/login`)

Componente `<SignIn/>` oficial: campo e-mail (`input[name="identifier"]` / placeholder com "email") → botão **"Continuar"** → campo senha (`input[name="password"]`) → submeter. Aguardar URL `**/dashboard**`. Se aparecer captcha/bot detection, usar o harness `@clerk/testing` do FE (`e2e/auth.setup.ts`).

### § Produto (sheet "Novo Produto" em `/dashboard/products`)

Botão **"Novo Produto"** → passo origem: **Manual** → formulário: labels `Nome`, `Preço`, `Estoque`, `Origem`, keyword, e no bloco de frete `Peso (g)`, `Altura (cm)`, `Largura (cm)`, `Comprimento (cm)`. Salvar → toast **"Produto criado com sucesso"**. (Componentes Radix: preferir seleção por role/label no snapshot.)

### § Página do evento (`/dashboard/events/<id>`)

- Feed **Comentários**: procurar pelo `@handle` e texto do comentário.
- Tabela **Pedidos**: colunas Cliente/Status/Itens/Valor/Criado; procurar linha com `@handle` e valor esperado.
- **Checkouts Ativos**, **KPIs** (comentários, carrinhos, receita) e **Funil** na lateral.
- **Live Mode**: seletor "Produto em Destaque" + toggle de pausa (só com evento ativo).
- Encerrar: botão **"Finalizar evento"** → dialog → **"Finalizar"**.
- Atualização via polling do `/pulse`: aguardar 5–10s (`browser_wait_for` pelo texto esperado) ou recarregar a página.

### § Checkout UI (`$FE_BASE/cart/<token>`)

Seção 1 — inputs por `name`:
```
customerName        (placeholder "Como está no documento")
customerDocument    (placeholder "000.000.000-00")
customerPhone       (placeholder "(11) 99999-9999", opcional)
email               (placeholder "seu@email.com")
```

Seção 2 — inputs por `name` (prefixo `shippingAddress.`): `zipCode` (placeholder "00000-000"), `street`, `number` (placeholder "123"), `complement`, `neighborhood`, `city`, `state`. Após CEP válido (debounce ~400ms) o ViaCEP autopreenche e **cidade/UF ficam disabled** — preencher só o `number` (e rua/bairro se não vierem).

Seção 3 — opções de frete são **buttons** (transportadora + prazo + valor, badges "Mais barato"/"Mais rápido"); clicar numa opção.

Seção 4 — dois buttons de método: **"Pix"** e **"Cartão de crédito"**.
- PIX: botão **"Gerar PIX"** → QR (`img alt="QR Code Pix"`) + input readonly copia-e-cola + botão **"Copiar"** + contador; polling de status a cada 5s; expirado → **"Gerar novo PIX"**.
- Cartão Mercado Pago: número/validade/CVV são **Secure Fields em iframes** (interagir via snapshot/click+type dentro do frame); nome no cartão `#mp-cardholder-name`; parcelas em select Radix.
- Cartão Pagar.me: inputs nativos `#pm-card-number`, `#pm-expiry`, `#pm-cvv`, `#pm-name`.
- Submit: botão **"Pagar R$ X,XX"** (valor dinâmico) — aguardar tela de sucesso ou erro inline.

Telas terminais: pago → `CheckoutPaidScreen` (resumo do pedido); expirado → `CheckoutExpiredScreen`.
