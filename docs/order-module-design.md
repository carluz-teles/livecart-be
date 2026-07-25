# Módulo Order — Definição (módulo · data model · handlers · listeners)

> **Status:** DEFINIÇÃO DE DESIGN (pré-implementação). Base: `docs/cart-order-split-study.md`.
> Segue as convenções do projeto: `.claude/skills/api-domain-convention` (fluxo HTTP),
> `docs/domain-map.md` §8 (handlers/listeners) e a memória `reactor-naming-on-fact` (`On<Fato>`).
> Decisões do dono já incorporadas: totais `total_cents`/`paid_total_cents`; **migrar tudo para a
> Order** (inclui ERP); sem split em WS.

---

## 1. Módulo Order — estrutura do pacote

Evolui o `internal/order/` atual (hoje agregado **read-only** com SQL inline) para o dono das
tabelas `orders`/`order_items`, seguindo a separação **handlers (síncrono) / listeners (reação a
evento)** do domain-map §8.

```
internal/order/
├── service.go              # regra de negócio; DONO de orders/order_items/order_payments/order_logistics
├── repository.go           # via SQLC (migrar do SQL inline → db/queries/order*.sql + sqlc gen)
├── types.go                # DTOs: XRequest+Validate, ToInput, XInput, NewXResponse (mappers)
├── domain/                 # o agregado Order = raiz + 4 entidades (§2.1)
│   ├── order.go            # RAIZ: identidade, totais congelados, status coarse; monta o agregado
│   ├── order_item.go       # ① linha imutável (snapshot de cart_item no pagamento)
│   ├── payment.go          # ③ Payment: pagamento + cupom + fiscal/NFe (payment_status VO)
│   ├── logistics.go        # ④ Logistics: frete + entrega + reserva de estoque (shipment_status VO)
│   ├── order_status.go     # VO status raiz: pending_payment|paid|shipped|delivered|refunded|cancelled|expired
│   └── totals.go           # VO Money/totais (reusar lib/valueobject)
│                           # ② Cart = entidade existente (internal/cart), referenciada por cart_id
├── handlers/               # AÇÕES SÍNCRONAS (path HTTP) — nome = use case
│   └── handler.go          # 9 rotas (Register→Validate→ToInput→Usecase→Response)
└── listeners/              # REACTORS (assinam o bus) — nome = On<Fato>
    ├── on_cart_paid.go     # OnCartPaid — CONGELA a Order (draft→paid); + EnsureOrderForCheckout (síncrono)
    ├── on_cart_refunded.go # OnCartRefunded — payment_status/status → refunded
    ├── on_cart_cancelled.go# OnCartCancelled / OnCartExpired
    └── on_shipment.go      # OnShipmentPosted / OnShipmentDelivered → logistics/status
```

**Wiring (`main.newApp`):** registra cada listener no `eventsServer` para o fato que assina; o
`order/listeners/OnCartPaid` roda como listener independente de `cart.paid` (chaveado por
`cart_id`, idempotente — não depende de ordem vs billing/ERP). O que hoje o `postcheckout` faz
(tracking_token, order_events, waitlist→fulfilled, e-mail) migra para `order/listeners` (timeline
e tracking passam a ser da Order).

**O que sai do `integration.ReactCartPaid`** (dissolução, domain-map §8): a parte de
timeline/tracking → `order/listeners`; ledger → `billing/listeners` (WS5, feito); waitlist →
`inventory/listeners`; ERP → `erp/listeners` (a Order consome o resultado do ERP, ver §5).

---

## 2. Data model

### 2.1 As 4 entidades da Order (decisão do dono, 2026-07-25)

A Order é um **agregado** com raiz `orders` + **4 entidades**: `order_items`, o **Cart** (origem,
referência), `order_payments` (pagamento + cupom + fiscal/NFe) e `order_logistics` (frete +
entrega + reserva de estoque). Normaliza — em vez de JSONBs gordos, cada preocupação tem sua tabela.

```
                       carts (INTENÇÃO — entidade existente, referenciada)
                         ▲ cart_id (1:1)
                         │
   order_items  ◀──1:N── orders (RAIZ: identidade + totais congelados + status) ──1:1──▶ order_payments
                                     │                                                    (money + fiscal)
                                     └──────────────1:1──────────────▶ order_logistics
                                                                       (frete + entrega + estoque)
```

**Raiz `orders`** — identidade, totais congelados (para reporting) e status do ciclo:
```sql
CREATE TABLE orders (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  cart_id            UUID NOT NULL UNIQUE REFERENCES carts(id),  -- 1:1 origem + idempotência
  short_id           INT  NOT NULL,          -- herdado do cart (nº público; NÃO regerar)
  store_id           UUID NOT NULL,
  event_id           UUID NOT NULL,
  customer_id        UUID,
  status             TEXT NOT NULL DEFAULT 'pending_payment',  -- ciclo §2.5 (nasce draft)
  -- Totais CONGELADOS no cart.paid (nullable no draft; imutáveis após paid). Fonte do reporting.
  total_cents        BIGINT,                 -- GMV: cart_product_total_cents(cart) no pagamento
  discount_cents     BIGINT NOT NULL DEFAULT 0,
  shipping_cents     BIGINT NOT NULL DEFAULT 0,
  paid_total_cents   BIGINT,                 -- total - discount + shipping
  paid_at            TIMESTAMPTZ,            -- selado no cart.paid (invariante: paid ⇒ NOT NULL)
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_orders_store ON orders(store_id);
CREATE INDEX idx_orders_event ON orders(event_id);
CREATE INDEX idx_orders_customer ON orders(customer_id);
```

**① `order_items`** — snapshot imutável das linhas (resolve os 2 sites por-produto do WS5):
```sql
CREATE TABLE order_items (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  order_id      UUID NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  product_id    UUID NOT NULL,          -- referência (produto pode mudar/ser excluído depois)
  product_name  TEXT NOT NULL,          -- DENORMALIZADO: order histórica sobrevive à mudança do produto
  quantity      INT  NOT NULL,
  unit_price    BIGINT NOT NULL         -- centavos, congelado
);
CREATE INDEX idx_order_items_order ON order_items(order_id);
```

**② Cart** — entidade **existente** (`carts`), referenciada por `orders.cart_id`. É a intenção de
compra (pré-pagamento); a Order não a duplica, aponta. Ao final da migração o cart fica enxuto
(§2.6): identidade + status + expires_at + short_id + cart_items.

**③ `order_payments`** — pagamento + cupom + **fiscal (NFe)** ("informações fiscais"):
```sql
CREATE TABLE order_payments (
  order_id            UUID PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,  -- 1:1
  payment_status      TEXT NOT NULL DEFAULT 'pending', -- pending|paid|refunded|failed|chargeback
  payment_method      TEXT,                   -- credit_card | pix | ...
  card_snapshot       JSONB,                  -- {brand, last_four, installments, authorization_code}
  gateway_snapshot    JSONB,                  -- provider PaymentStatus (WS4 payment_snapshot)
  -- cupom (o desconto que compôs discount_cents na raiz):
  coupon_id           UUID,
  coupon_code         TEXT,
  coupon_discount_cents BIGINT NOT NULL DEFAULT 0,
  -- fiscal / ERP finalização + NFe:
  external_order_id   TEXT,                   -- bridge Tiny
  erp_finalisation_status TEXT NOT NULL DEFAULT 'pending', -- pending|done|failed
  erp_last_error      TEXT,
  erp_last_attempt_at TIMESTAMPTZ,
  erp_attempts_count  INT  NOT NULL DEFAULT 0,
  invoice_id          TEXT,                   -- NFe (erp_invoice_id)
  invoice_key         VARCHAR(44),
  invoice_status      TEXT,                   -- pending|authorized|cancelled|rejected
  invoice_emitted_at  TIMESTAMPTZ
);
```

**④ `order_logistics`** — frete + entrega + rastreamento + **reserva de estoque (Design C)**:
```sql
CREATE TABLE order_logistics (
  order_id            UUID PRIMARY KEY REFERENCES orders(id) ON DELETE CASCADE,  -- 1:1
  -- endereço + cotação de frete (congelados no pagamento):
  shipping_address    JSONB,
  shipping_service_id INT,
  shipping_service_name TEXT,
  shipping_carrier    TEXT,
  shipping_cost_cents BIGINT,                 -- cobrado do cliente (= shipping_cents da raiz)
  shipping_cost_real_cents BIGINT,            -- custo real (lojista)
  shipping_deadline_days INT,
  -- rastreamento público + envio:
  tracking_token      TEXT,                   -- gerado 1x no paid (migra do cart)
  shipment_status     TEXT,                   -- pending|in_transit|delivered|issue|... (de shipments)
  shipment_provider   TEXT,
  provider_order_id   TEXT,
  tracking_code       TEXT,
  public_tracking_url TEXT,
  delivered_at        TIMESTAMPTZ,
  -- reserva de estoque no ERP (Design C — pré-pagamento; nasce no draft):
  erp_order_state     TEXT NOT NULL DEFAULT 'none', -- none|converting|open|mutating|confirmed|cancelled
  erp_stock_launched  BOOLEAN NOT NULL DEFAULT false,
  erp_op_started_at   TIMESTAMPTZ
);
CREATE UNIQUE INDEX idx_order_logistics_tracking ON order_logistics(tracking_token) WHERE tracking_token IS NOT NULL;
```
> `shipments`/`shipment_tracking_events` (000052) já existem: a timeline granular de tracking
> continua nelas, re-apontadas a `order_id`; `order_logistics` guarda o estado corrente/resumo.

**Decisões travadas:**
- `product_name` **denormalizado**; `total_cents` na raiz é redundante com `SUM(order_items)` **por
  design** (campo congelado = fonte do relatório; invariante trava a igualdade na selagem).
- **Split fiscal × estoque:** NFe/finalização → `order_payments` (fiscal); reserva/estoque/envio
  → `order_logistics`. `external_order_id` (id do pedido Tiny, usado por ambos) mora no
  `order_payments` como bridge; o estado da reserva fica no `order_logistics`.
- **1:1** de payments/logistics via `order_id` como PK (sem linha órfã; criadas junto do draft).

### 2.5 Estados: raiz `orders.status` + sub-status das entidades

A Order nasce **draft** na iniciação do checkout (decisão §5) e é **selada** no `cart.paid`.
O `orders.status` é o ciclo **coarse** (para listagem/reporting); o detalhe fino mora nas
entidades: `order_payments.payment_status` (pending|paid|refunded|failed|chargeback) e
`order_logistics.shipment_status` (pending|in_transit|delivered|issue|…).

```
   EnsureOrderForCheckout
          │
          ▼
   [pending_payment] ──(cart expira sem pagar)──▶ [expired]   (terminal; draft abandonado)
          │
       OnCartPaid  (congela snapshot; draft → paid)
          ▼
       [paid] ──OnShipmentPosted──▶ [shipped] ──OnShipmentDelivered──▶ [delivered]
          │
   OnCartRefunded │ OnCartCancelled
          ▼
   [refunded] / [cancelled] / [chargeback]
```
Só `orders.status='paid'` em diante (ou `paid_at IS NOT NULL`) conta como venda no reporting.
`shipped`/`delivered` da raiz derivam do `order_logistics.shipment_status`; `refunded`/`chargeback`
do `order_payments.payment_status` — a raiz consolida, as entidades detalham.
`order_events` (hoje `UNIQUE(cart_id, event_type)`) passa a ser `UNIQUE(order_id, event_type)` —
timeline 1:1 com a Order. event_types já cobrem payment_confirmed/refunded/cancelled/shipped/delivered.

### 2.6 Relação cart ↔ order e o que MUDA no `carts`
- **Compartilhado:** `short_id` nasce no cart (000062) e é **herdado** pela order — chave pública
  única; nunca regerado.
- **`shipments`/`shipment_tracking_events`** passam a apontar `order_id → orders(id)` (hoje →
  carts.id). Migração de FK na Fase E.
- **Colunas order-like do `carts`** (customer_*, shipping_*, coupon_*, card_*, payment_status,
  tracking_token, notify_*, erp_*, external_order_id) são **depreciadas e dropadas** ao final
  (Fase F do estudo), depois do cutover de leitura. O cart fica: identidade + `status` +
  `expires_at` + `short_id` + `cart_items` + FK order.

---

## 3. Handlers (ações síncronas — path HTTP)

Todos seguem `Request → Validate (ozzo) → ToInput (VO) → svc.UseCase → NewXResponse`, passam
`c.UserContext()`, e só `return err` (ErrorHandler central). Molde: `internal/member`.
Nome do handler = **use case** (não `On<Fato>` — isso é só listener).

| Rota | Handler (use case) | Request DTO | Retorno |
|---|---|---|---|
| `GET /orders` | `List` | query filters (status[], payment[], q, page, sort) | `NewListOrdersResponse` |
| `GET /orders/stats` | `GetStats` | — | `NewOrderStatsResponse` |
| `GET /orders/:id` | `GetDetail` | — | `NewOrderDetailResponse` |
| `GET /orders/:id/upsell` | `GetUpsell` | — | `NewOrderUpsellResponse` |
| `PATCH /orders/:id` | `UpdateFulfillment` | `UpdateOrderStatusRequest` (status/payment_status) | `NewOrderResponse` |
| `PATCH /orders/:id/shipping-address` | `UpdateShippingAddress` | `UpdateShippingAddressRequest` | 204 |
| `POST /orders/:id/regenerate-checkout` | `RegenerateCheckout` | — | `NewRegenerateCheckoutResponse` |
| `POST /orders/:id/retry-erp` | `RetryERPFinalisation` | — | `NewOrderDetailResponse` |
| `POST /orders/:id/sync-invoice` | `SyncInvoice` | — | `NewOrderDetailResponse` |

**Notas de convenção:**
- `PATCH /orders/:id` deixa de ser "mutar o cart" e vira **transição de fulfillment** — a
  invariante (transições válidas do §2.3) mora no domínio (`domain.Order.Transition`), não no ozzo.
- **3 camadas de validação:** sintática no `Request.Validate()` (enum de status, page/sort);
  semântica no `ToInput` (VO de status/endereço → 422 `ErrUnprocessable`); invariante no
  service/domínio (transição ilegal, order já estornada → `httpx.Err*`).
- **Gotcha ozzo:** qualquer `int` de valor com `Min(n≥1)` (ex.: `page`) pareado com `Required`.
- Service retorna `*domain.Order` (nunca DTO); handler mapeia com `NewOrderResponse`.
- **Contrato dos 9 endpoints preservado** durante a migração — a Order alimenta os mesmos DTOs
  que o FE já consome (o read-model muda de fonte, não de forma).

---

## 4. Listeners (reactors — assinam o bus; nome `On<Fato>`)

Todos retornam `error` (sobe pra task asynq → retry+DLQ) e são **idempotentes**.

### 4.0 `EnsureOrderForCheckout` — cria a Order draft (SÍNCRONO, não é listener)
Chamado de dentro do `PrepareCartForPayment` (iniciação do checkout). Cria/reusa a Order em
`pending_payment` (idempotente por `orders.cart_id UNIQUE`) e delega a reserva de estoque ERP
(Design C). Não congela snapshot — o cart ainda pode mudar. É um **handler** (ação síncrona).

### 4.1 `OnCartPaid` — CONGELA a Order (o coração)
Assinatura (convenção On<Fato>, alinhada ao billing WS5):
```go
func (l *Listener) OnCartPaid(ctx context.Context, cartID, storeID string, gmvCents int64, paymentSnapshot []byte) error
```
Passos (idempotente: se a Order já está `paid` → no-op):
1. Carrega a Order draft do `cart_id` (criada em §4.0; se não existir — rollout/loja sem reserva —
   cria on-the-fly em `pending_payment`).
2. Lê o snapshot do cart (linhas + customer/shipping/coupon/card + totais) **no instante do pagamento**.
   `total_cents` = `gmv_cents` do payload (fallback `cart_product_total_cents`, mesma query — proibido
   re-somar no Go, ver `centralize-shared-logic`); `paid_total_cents` = total − discount + shipping.
3. **Sela** numa transação: raiz `orders` (totais + `status → paid`), `order_items` (imutável;
   `product_name` denorm), `order_payments` (payment_status=paid, cupom, card/gateway snapshot),
   `order_logistics` (endereço/frete congelados).
4. `order_logistics.tracking_token` (set-once) + `order_events` `payment_confirmed` (UNIQUE order_id+type).
5. Marca notify e dispara e-mail de recibo (best-effort — falha não derruba a task; segue billing).

> **Ordenação:** roda como listener **independente** de `cart.paid` (chaveado por cart). Não
> precisa preceder billing/ERP porque cada um materializa o seu por `cart_id`. Se algum reactor
> vier a **depender** da Order existir, aí sim o `order_id` entra no payload e a ordem é imposta
> no `main` (decisão a confirmar na implementação).

### 4.2 `OnCartRefunded`
```go
func (l *Listener) OnCartRefunded(ctx context.Context, cartID, storeID string, refundSnapshot []byte) error
```
`order_payments.payment_status → refunded` + raiz `orders.status → refunded`; grava `order_events`
`payment_refunded` com snapshot do que foi estornado (registra o estorno, não apaga o snapshot da
compra). Simetria com billing.

### 4.3 `OnCartCancelled`
`order_payments.payment_status → cancelled` + `orders.status → cancelled`; `order_events`
`payment_cancelled`. (Cancelamento de venda já paga; cart nunca-checkout não gera Order — ver §5.)

### 4.4 `OnShipmentPosted` / `OnShipmentDelivered`
Hoje inline no `postcheckout`; viram listeners da Order: atualizam `order_logistics.shipment_status`
(→ in_transit/delivered) e derivam a raiz `orders.status` (→ shipped/delivered) + `order_events`.
Consomem `shipment.*` (fatos de fulfillment já previstos em `events/types.go`).

### 4.5 `OnCartExpired` e carts sem checkout
- Cart cuja Order draft (`pending_payment`) expira sem pagar → `OnCartExpired` transiciona a Order
  para `expired` (terminal) e libera a reserva ERP. **Reporting ignora** (`!= 'paid'`).
- Cart que **nunca iniciou checkout** → **sem Order** (draft só nasce no `PrepareCartForPayment`).
  `ListCartsForWhatsAppRecovery` opera nesses carts abertos — continua no cart (Tier-3 do estudo).

---

## 5. ERP e ciclo de vida — DECISÃO: (B) Order desde a iniciação do checkout ✅

Decisão do dono: **100% do ERP vai para a Order**. Consequência: a Order **nasce na iniciação do
checkout** (`PrepareCartForPayment`), não no `cart.paid`. Modelo resolvido:

- **Materialização em 2 tempos:**
  1. **Iniciação do checkout** (`PrepareCartForPayment`, síncrono) → cria a Order em
     **`pending_payment`** (draft). É aqui que a **reserva de estoque no ERP** (Design C:
     `erp_order_state` converting→open, `erp_stock_launched`, `erp_op_started_at`) passa a viver
     **na Order** desde o início. O snapshot da compra ainda é **provisório/mutável** (o cart
     ainda pode mudar via mutating/upsell).
  2. **`cart.paid`** (listener `OnCartPaid`) → **CONGELA** o snapshot (linhas, totais, customer,
     shipping, coupon, card, payment) e transiciona `pending_payment → paid`. A partir daqui o
     snapshot é **imutável**; o fulfillment (incl. ERP finalisation/NFe) evolui.
- **Toda a máquina ERP é da Order:** reserva (Design C), finalização (`erp_finalisation_*`),
  NFe (`erp_invoice_*`), bridge (`external_order_id`). O `erp/listeners`/`service_erp_order`
  opera sobre a Order, não sobre o cart.
- **Draft não pago:** Order em `pending_payment` cujo cart expira sem pagamento → `expired`
  (terminal). Reporting (Tier-1) conta só vendas reais: `WHERE fulfillment_status = 'paid'`
  (ou `paid_at IS NOT NULL`) — espelha o `payment_status='paid'` de hoje, movido pra Order.
- **Idempotência:** Order keyed por `orders.cart_id UNIQUE` — criada 1x na iniciação, congelada
  1x no paid. Reiniciar checkout (`RegenerateCheckout`) reusa a mesma Order draft.

> **Trade-off aceito:** a Order não "nasce imutável no pagamento" — nasce draft e **fica**
> imutável no pagamento. Em troca, o ERP fica 100% coeso na Order e o modelo cobre o
> "pedido-como-reserva" do Design C sem seam entre cart e order.

### 5.1 Ajuste no gatilho de materialização (reflete nas §2.3 e §4)
- **Handler síncrono** (não listener) no fluxo de iniciação: `order.EnsureOrderForCheckout(ctx,
  cartID, storeID)` — cria/reusa a Order draft + delega a reserva ERP. Chamado de dentro do
  `PrepareCartForPayment`. (Handler = ação síncrona do request path, domain-map §8.)
- **Listener** `OnCartPaid` deixa de *criar* e passa a **congelar** a Order (draft → paid) +
  tracking_token + order_events + recibo. Continua idempotente por `cart_id`.
- Campos de snapshot (`total_cents`, `paid_total_cents`, `*_snapshot`, `paid_at`) são
  **nullable até o `cart.paid`** (preenchidos e "selados" na transição para `paid`); um
  invariante de domínio garante que `paid` exige todos preenchidos.

---

## 6. Próximos passos (após aprovar esta definição)
1. Migration `000094_orders.up.sql` — as 4 tabelas (`orders`, `order_items`, `order_payments`,
   `order_logistics`), índices e defaults `pending_payment`/`none` — e queries SQLC.
2. `domain/` — raiz `order.go` + entidades `order_item.go`/`payment.go`/`logistics.go` + VOs de status.
3. `order.EnsureOrderForCheckout` (síncrono: cria draft + reserva ERP) chamado no
   `PrepareCartForPayment`.
4. `order/listeners/OnCartPaid` (congela snapshot draft→paid) com teste TDD idempotente + golden
   de paridade (número da Order == número cart-based) — mesmo método do WS5.
5. Handlers via `internal/member` como molde; cutover de leitura por Tier (estudo §5, fases A→F).
