# Estudo — Separação Cart / Order

> **Status:** ESTUDO (design, não implementação). Aterrado no código da branch `stg` em 2026-07-25.
> Complementa `docs/domain-map.md` (§7 sequência) e a decisão registrada em memória `cart-order-split`.
> **Não é um plano de execução aprovado** — é a base para decidir *se* e *como* separar.

## 0. TL;DR

O `cart` hoje é um **híbrido**: carrega a *intenção de compra* da live **e** uma *order embutida*
(dados de venda congelados no checkout/pagamento). Isso força ~14 read-models pós-venda a
**re-somar `cart_items` mutável** para descobrir "quanto essa compra valeu" — a causa raiz que
gerou as ~20 cópias da fórmula de GMV (mitigadas pelo WS5 com a função canônica
`cart_product_total_cents`, um *stopgap* que centraliza a fórmula mas ainda lê de tabela mutável).

A cura é materializar uma **Order** no `cart.paid`: um **snapshot imutável da compra**
(linhas, totais, cliente, frete, cupom, pagamento) + uma **máquina de estados de fulfillment**
(pago → enviado → entregue; estornado/cancelado). Depois disso, o GMV pós-venda vira
`SELECT total_cents FROM orders` em vez de re-somar cart.

**Boa notícia:** parte do split já existe latente — `shipments`/`shipment_tracking_events`
(fulfillment já fora do cart), `order_events` (timeline), e o **snapshot no payload de `cart.paid`**
(padrão semeado no WS4). O trabalho é *extrair o que já está implícito*, não construir do zero.

---

## 1. Causa raiz e motivação

- **Sem Order imutável, todo read-model pós-venda relê o cart vivo.** Dashboard, pedidos,
  clientes, contabilidade — todos filtram `carts.payment_status='paid'` e re-somam
  `cart_items`. Como o cart continua mutável (waitlist, upsell, reabertura de expirado), a
  "verdade da venda" nunca fica congelada num só lugar → N fórmulas que divergem.
- **WS5 tratou o sintoma, não a doença.** `cart_product_total_cents(cart_id)` unificou a
  fórmula, mas continua lendo de `cart_items`. A Order imutável elimina a leitura do cart.
- **A fronteira conceitual:** `cart` = **fonte da verdade até o pagamento** (intenção,
  stateful, efêmero); `order` = **fonte da verdade do fulfillment depois** (venda liquidada,
  ciclo próprio). O papel do cart *termina* no `cart.paid`.

---

## 2. Estado atual (aterrado no código)

### 2.1 Não existe tabela `orders`
- `detected_orders` foi removida na migration `000044`. Hoje é **"cart-as-order"**.
- `internal/order/` é um **agregado DDD read-only** (`domain/order.go`) montado em runtime a
  partir de `carts` + `cart_items` + `shipments` + colunas `erp_*` + `live_events`.
  `repository.go` usa **SQL inline** (o `db/queries/order.sql` é parcialmente legado — atenção:
  o WS5 alterou queries nele; confirmar quais estão realmente em uso).
- 9 endpoints (`/orders`, `/orders/:id`, `/orders/stats`, `/orders/:id/upsell`,
  PATCH status/shipping-address, regenerate-checkout, retry-erp, sync-invoice).

### 2.2 O cart é um híbrido (colunas de `carts`)

| Grupo | Colunas | Pertence a | Migration |
|---|---|---|---|
| **Intenção (fica no cart)** | `id`, `event_id`, `session_id`, `platform_user_id`, `platform_handle`, `token`, `status`, `expires_at`, `cancelled_reason`, `created_at`, `customer_id` | **CART** | 000001, 000032, 000042 |
| **Nº do pedido** | `short_id` | **Compartilhado** (nasce no cart, herdado pela order) | 000062 |
| **Cliente (checkout)** | `customer_email/name/document/phone`, `whatsapp_consent*` | **ORDER** | 000024, 000041, 000081 |
| **Frete** | `shipping_address`, `shipping_service_*`, `shipping_cost_*`, `shipping_deadline_days`, `shipping_quoted_at` | **ORDER** | 000041, 000048 |
| **Cupom** | `coupon_id`, `coupon_code`, `coupon_discount_cents` | **ORDER** | 000063 |
| **Pagamento** | `payment_status`, `paid_at`, `payment_method`, `payment_integration_id`, `card_*` | **ORDER** | 000001, 000039, 000056/57 |
| **Checkout transiente** | `checkout_id`, `checkout_url`, `checkout_expires_at` | **Cache transiente** (nem cart nem order — do provider) | 000024 |
| **Rastreamento** | `tracking_token` | **ORDER** (gerado no paid) | 000066 |
| **Notificação** | `notify_status`, `notify_error`, `notified_at` | **ORDER** | 000001 |
| **ERP finalização** | `erp_finalisation_status`, `erp_last_error/attempt_at`, `erp_attempts_count`, `erp_payment_snapshot`, `external_order_id` | **ORDER (fulfillment)** | 000069, 000001 |
| **ERP Design C** | `erp_order_state`, `erp_stock_launched`, `erp_op_started_at` | **ORDER (fulfillment)** — máquina de estados própria | 000085 |
| **ERP NFe** | `erp_invoice_id/key/status/emitted_at` | **ORDER (fulfillment)** | 000072 |
| **Snapshot upsell** | `initial_snapshot_taken_at`, `initial_subtotal_cents` | Audit (indeciso) | 000059 |

### 2.3 Partes da order que JÁ estão fora do cart
- **`shipments` + `shipment_tracking_events`** (000052): fulfillment físico, chaveado por
  `order_id → carts.id`. Máquina de status própria (pending/in_transit/delivered/issue/...).
- **`order_events`** (000067): timeline `UNIQUE(cart_id, event_type)`, append-only, **interna**
  (não no bus). event_types já incluem `payment_confirmed/cancelled/refunded/shipped/delivered`.
- **Snapshot no payload de `cart.paid`** (WS4): o ERP reactor recebe `payment_snapshot` no
  evento em vez de reler o cart → **é exatamente o padrão que a Order vai generalizar**.

### 2.4 Coreografia de `cart.paid` hoje
Emissor `ProcessPaymentNotification` (`integration/service.go:~3705`) emite o fato com payload:
```json
{ "cart_id", "store_id", "payment_id", "payment_method",
  "gmv_cents": <WS5>, "payment_snapshot": <WS4> }
```
Reactors (fan-out L3): **coupon** (confirma redemption), **postcheckout** (gera `tracking_token`
set-once, insere `order_events` `payment_confirmed`, waitlist→fulfilled, e-mail de recibo),
**billing ledger** (WS5, com retry/DLQ), **ERP** (separado, usa o snapshot; máquina Design C).

---

## 3. Modelo-alvo: a Order

### 3.1 O que "imutável" realmente significa
A Order tem **duas camadas**:
1. **Snapshot da compra (IMUTÁVEL)** — congelado no `cart.paid` e nunca mais alterado:
   linhas (`order_items`), totais (`product_total_cents`, `shipping_cents`, `discount_cents`,
   `grand_total_cents`), cliente, endereço de frete, cupom aplicado, dados do cartão, snapshot
   do pagamento. É a "nota do que foi vendido".
2. **Estado de fulfillment (MUTÁVEL, ciclo próprio)** — evolui muito depois: status de
   fulfillment (paid→shipped→delivered; refunded/cancelled/chargeback), NFe, envio, tentativas
   de ERP. Referencia o snapshot mas não o altera.

> Ou seja: não é a *linha inteira* que é imutável — é o **fato da compra**. O fulfillment
> precisa mudar de estado por design.

### 3.2 Esboço de schema (a discutir, não final)
```
orders
  id                UUID PK
  cart_id           UUID UNIQUE FK carts(id)   -- 1:1 com o cart que a originou
  short_id          INT                        -- herdado do cart (nº público do pedido)
  store_id          UUID
  -- SNAPSHOT IMUTÁVEL DA COMPRA (congelado no cart.paid):
  total_cents          BIGINT   -- GMV: = cart_product_total_cents(cart) no pagamento (exclui frete/cupom)
  shipping_cents       BIGINT
  discount_cents       BIGINT   -- cupom
  paid_total_cents     BIGINT   -- o que o cliente pagou = total - discount + shipping
  customer_snapshot    JSONB    -- name/document/phone/email congelados
  shipping_snapshot    JSONB    -- endereço + serviço + prazo congelados
  coupon_snapshot      JSONB    -- code/discount
  card_snapshot        JSONB    -- brand/last4/installments/auth_code
  payment_snapshot     JSONB    -- do gateway (o mesmo do WS4)
  paid_at              TIMESTAMPTZ
  -- ESTADO DE FULFILLMENT (mutável):
  fulfillment_status   TEXT     -- paid|shipped|delivered|refunded|cancelled|chargeback
  external_order_id    TEXT     -- ERP
  erp_*                ...      -- (migração tardia — ver §5 fase E)
  created_at           TIMESTAMPTZ

order_items                      -- SNAPSHOT IMUTÁVEL das linhas (resolve os 2 sites por-produto)
  id            UUID PK
  order_id      UUID FK orders(id)
  product_id    UUID
  product_name  TEXT             -- DENORMALIZADO: sobrevive a edição/exclusão do produto
  quantity      INT
  unit_price    BIGINT
```
`order_events` e `tracking_token` passam a ser chaveados por `order_id` (hoje `cart_id`).

### 3.3 Materialização (Order Service reactor)
- Novo **`order/listeners/OnCartPaid`** (convenção On<Fato>, ver `reactor-naming-on-fact`)
  torna-se o **primeiro reactor** de `cart.paid` (antes de billing/ERP).
- Lê o snapshot do cart no instante do pagamento → insere `orders` + `order_items` com totais
  congelados. **Idempotente** por `UNIQUE(cart_id)` (mesmo padrão do ledger WS5).
- Falha → propaga erro → retry/DLQ (uma venda nunca "some").
- Os demais reactors (billing, ERP, postcheckout) passam gradualmente a **ler da Order**
  (ou receber `order_id` no payload) em vez do cart.

---

## 4. Impacto nos call sites (o que migra de "re-somar cart" → "ler Order")

Fonte: inventário do explorador de call sites. `total_cents` congelado torna 12 read-models triviais.

**Tier 1 — vira `SELECT ... FROM orders` (12):** `GetDashboardStats`, `GetMonthlyRevenue`,
`GetEventsWithRevenue`, `GetAggregatedFunnel`, `ListOrders`, `GetOrderStats`, `ListCustomers`
(total_spent), `GetCustomerStats`, `SearchCustomers`, `GetWhatsAppRecoveryStats`,
`GetEventStats` (confirmed_revenue), `GetSessionStats` (confirmed).

**Tier 2 — condicional cart↔order (2):** `GetCartTotals`, `ListCartsByCustomer` (o cart no
resultado pode estar pago → lê order; ou aberto → calcula do cart). `GetCartGMVCents` idem.

**Tier 3 — Order escalar NÃO resolve (3):**
- `GetTopProducts`, `ListProductsByEvent` — são **por-produto** (`GROUP BY product`).
  **Resolvidos por `order_items`** (§3.2), não pelo `total_cents`. São os 2 `TODO(gmv-canonical)`
  deixados no WS5.
- `ListCartsForWhatsAppRecovery` — opera sobre carts que **nunca pagaram** (abertos/expirados);
  continua no cart. A função `cart_product_total_cents` **sobrevive** justamente para o
  cart vivo (pré-pagamento).

**Precedente já existente:** `GetCustomerCheckoutSnapshot` já congela dados de checkout — prova
de que "congelar no pagamento" é padrão aceito na base.

---

## 5. Caminho de migração (sem big-bang)

Cada fase é isolável, verificável e reversível. **Regra de ouro:** nada muda no fluxo
pré-pagamento do cart; a paridade de números é travada por *golden test* (mesmo método do WS5).

- **Fase A — Materializar (dual-write + backfill).** Criar `orders`/`order_items` (migration
  000094+). `order/listeners/OnCartPaid` grava a Order no `cart.paid`, **em paralelo** às colunas
  do cart (que continuam sendo escritas). Backfill das carts pagas históricas. Nenhum read-model
  muda ainda. ✅ baixo risco.
- **Fase B — Cutover de leitura (Tier 1).** Migrar os 12 read-models para ler de `orders`.
  Golden test travando: número novo == número antigo (cart-based) para os mesmos dados.
- **Fase C — Timeline & rastreamento.** `order_events` e `tracking_token` passam a ser da Order
  (`order_id`). Página pública `/track/{short_id}/{token}` lê da Order.
- **Fase D — Por-produto.** Migrar `GetTopProducts`/`ListProductsByEvent` para `order_items`
  (remove os 2 `TODO(gmv-canonical)`).
- **Fase E — ERP/fulfillment (mais arriscada, por último).** Mover a máquina Design C
  (`erp_order_state`, invoice, `external_order_id`, `shipments.order_id`) para referenciar a
  Order. Alto risco (single-flight CAS, retry, NFe) — só depois de A–D estáveis.
- **Fase F — Deprecar.** Parar de escrever as colunas order-like no cart; depois **dropar**.
  O cart fica enxuto (só intenção + `short_id` + FK order).

---

## 6. Riscos

1. **Backfill incorreto** falseia relatórios históricos. Mitigar com golden/paridade por loja.
2. **Divergência dual-write** (Fase A) entre cart e order. Mitigar: order é derivada 1:1 do cart
   no evento; teste de igualdade cart↔order enquanto ambos coexistem.
3. **Máquina ERP Design C** é intrincada (CAS `converting` nunca volta a `none`, sweep de stuck
   ops, mutating, NFe). Mover cedo = risco de duplicar pedido/estoque no Tiny. → **Fase E, por último.**
4. **`short_id` é público e nasce no cart.** Não regerar; é a chave compartilhada cart↔order.
5. **9 endpoints "cart-as-order" em produção** + o FE que os consome. A migração precisa manter
   o contrato desses endpoints estável (a Order alimenta os mesmos DTOs).
6. **Imutabilidade × edições atuais:** o PATCH `/orders/:id` hoje muda `status`/`payment_status`
   e `shipping-address`. Pós-split, essas viram **transições de fulfillment** (ou edições
   pré-pagamento no cart), não mutação do snapshot. Reconciliar antes da Fase F.

---

## 7. Decisões do dono (2026-07-25 — RESOLVIDAS)

1. **Nomes dos totais:** `total_cents` (GMV — produto, exclui frete/cupom) + `paid_total_cents`
   (o que o cliente pagou = total − desconto + frete). ✅
2. **Migrar TUDO para a Order** — inclusive ERP Design C. Sem dividir em WS separados; feito
   nesta sessão/iniciativa. ✅
3. Antes de implementar: **definir módulo Order, data model, Handlers e Listeners** →
   `docs/order-module-design.md`. ✅
4. Identidade da Order, denormalização de `order_items`, backfill: ver decisões no design doc.

> Detalhamento do módulo/data-model/handlers/listeners: **`docs/order-module-design.md`**.

---

## 8. Recomendação

Fazer o split **incremental por fases A→F**, começando por **A (materializar + backfill,
dual-write)** e **B (cutover Tier-1 com golden test)** — que já entregam a cura da causa raiz
(GMV lido, não recalculado) com risco baixo, aproveitando o padrão de snapshot do WS4 e a
infra de reactors do projeto event-driven. Fulfillment/ERP (Fase E) fica para depois, isolado.
