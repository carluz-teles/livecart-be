# Domain Map — LiveCart

> **Status:** north-star para alinhar domínios ↔ código. Complementa
> `docs/event-architecture.md` (o *fluxo* de eventos) definindo os *donos*: quem
> é dono de qual dado, quem faz o quê, e onde o código atual **viola** as
> fronteiras. Aterrado no código (`internal/*`, `db/queries/*`, wiring em
> `cmd/http-server/main.go`).

---

## 1. O problema central: pacote ≠ domínio

A estrutura de **pacotes não bate com a estrutura de domínios**. Três sintomas:

- **`integration` é um mega-pacote** (~7.700 linhas só no `service.go`) que hospeda
  **6 domínios + a camada de adapters**: Pagamento, ERP/Tiny, Estoque, Waitlist,
  ingestão de comentário do Instagram, sync de produto, WhatsApp — tudo junto com
  o anti-corruption layer dos providers.
- **`live` é um orquestrador** que injeta 7 domínios (`SetNotifier`,
  `SetPostCheckoutHook`, `SetCouponLifecycle`, `SetERPFinalizer`,
  `SetCustomerUpserter`, `SetBlockedHandleChecker`, `SetEventCloseScheduler`).
- **`postcheckout` é um middleman**: `OnCartPaid` sozinho faz Order (timeline +
  tracking) + Billing (ledger/taxa via `GMVReporter`) + Waitlist (fulfillment) +
  Notificação (recibo) + ERP-retry.

O acoplamento cross-domínio roda por **~20 interfaces injetadas** (hooks / gates /
reporters / syncers / schedulers). Isso evita ciclo de import — mas as fronteiras
ficam **implícitas** e as responsabilidades **vazam**.

---

## 2. Os domínios (modelo alvo)

Cada domínio = dono claro de tabelas + produtor/consumidor de eventos. `⚠` marca
onde o código atual não respeita a fronteira.

### A. Live Commerce (Ingestão)
- **Faz:** o evento ao vivo — ciclo de event/session, ingestão de comentário do IG,
  parsing de intenção de compra, live mode (produto em destaque).
- **Possui:** `live_events`, `live_sessions`, `live_session_platforms`,
  `live_comments`, `event_products`, `event_upsells`.
- **Produz:** `event.created/ended`, `session.created/ended`, `post.window_closed`,
  `comment.received`.
- **Hoje:** `live` + `integration` (`ProcessInstagramComment`).
- ⚠ A ingestão de comentário (parsing + criação de cart) mora em `integration`, não em `live`.

### B. Cart — Intenção de compra (stateful, efêmero)
- **Faz:** o carrinho do comprador durante a live; mutações; armar checkout;
  expiração (ETA); reopen/cancel. **Termina no pagamento** (ver split Cart/Order).
- **Possui:** `carts` (campos de intenção), `cart_items`, `cart_mutations`,
  `cart_initial_items`, `store_order_counters` (short_id), `blocked_handles`.
- **Produz:** `cart.item_added`, `cart.checkout_armed`, `cart.reopened`,
  `cart.expired`, `cart.cancelled`.
- **Hoje:** espalhado por `live` (criação na live), `checkout` (mutações),
  `integration` (expiração/estoque).
- ⚠ Um domínio, três pacotes.

### C. Payment — Processamento
- **Faz:** resolve o status no provider → o flip guardado → emite o fato canônico
  de pagamento. É o **executor L2** da coreografia.
- **Possui:** `carts.payment_status` (o flip); `payments` (⚠ vestigial — ver §5).
- **Produz:** `cart.paid`, `cart.refunded`, `payment.failed`, `cart.cancelled`.
- **Consome:** `payment.process` (command).
- **Hoje:** `integration` (`ProcessPaymentNotification`, `webhook_handler.go`) +
  `checkout` (fast-path do cartão, agora só dispara o inline; o `cart.paid` canônico
  é do webhook — ver `fix 89850db`).
- ⚠ Domínio limpo, mas enterrado no `integration`.

### D. Order — Materialização de fulfillment (pós-pagamento)
- **Faz:** o ciclo de vida do **pedido** depois do pagamento — timeline, tracking,
  estado de fulfillment. (Ver `cart-order-split`: Cart=intenção, Order=materialização.)
- **Possui:** `order_events`; `carts.tracking_token` (⚠ deveria ser da Order);
  **futuro:** tabela `orders` própria.
- **Produz:** (interno: linhas de `order_events`).
- **Consome:** `cart.paid`, `cart.refunded`, `cart.cancelled`, `shipment.*`.
- **Hoje:** `postcheckout` (`OnCartPaid/Refunded/Cancelled/OnShipmentPosted/OnDelivered`)
  + `order` (view read-only sobre carts pagos).
- ⚠ `OnCartPaid` também faz Ledger + Waitlist + Notificação — não é trabalho da Order.

### E. Billing / Finance — **DOIS sub-domínios** (ver decisão §6.1)
- **E1. Ledger / Receita interna** — quanto o LiveCart **fatura** das vendas do
  lojista (taxa de sucesso) + extrato financeiro. **É a nossa contabilidade.**
  - **Possui:** `billing_ledger_entries`. **Produz:** `gmv.recorded`, `gmv.refunded`.
    **Consome:** `cart.paid`, `cart.refunded`.
  - ⚠ Escrito **inline dentro de `postcheckout.OnCartPaid`** via `GMVReporter`
    (fire-and-forget, sem retry) — assimétrico com o lado de estorno que já é reactor
    (`billingGate.OnCartRefunded`). Ver **WS5** proposto.
- **E2. Subscription** — o lojista **paga** mensalidade ao LiveCart (Stripe).
  - **Possui:** `subscriptions`. **Produz:** `subscription.*`, `trial.ending_soon`.
    **Consome:** `subscription.process` (command).
  - **Hoje:** `billing` (invertido no template L1→L2, `1467aa6`).
- Ambos vivem no pacote `billing`. Contextos distintos: "quanto o lojista fatura"
  vs "quanto o lojista nos paga".

### F. ERP / Sync de fulfillment (Tiny)
- **Faz:** espelha a venda no ERP do lojista — máquina de estados do pedido
  (create/finalize/cancel), reversão em refund/expiry, sync/import de produto,
  resolução de contato.
- **Possui:** `carts.erp_*` (`erp_order_state`, `external_order_id`,
  `erp_payment_snapshot`), `erp_contacts`.
- **Produz:** `erp.order_created/finalized/cancelled/finalization_failed`.
- **Consome:** `cart.paid` (snapshot no payload), `cart.refunded`, `cart.expired`.
- **Hoje:** `integration` (`service_erp_order.go` + trechos grandes do `service.go`).
- **É um CONSUMER** de fatos de Payment/Order, não o Order em si → **domínio próprio**.
  (Já invertido: `ReactCartPaidERP/RefundConvertedCartOrder/ReactCartExpiredERP`.)

### G. Inventory — Estoque & Waitlist
- **Faz:** verdade do estoque local + reservas + fila de espera (queue/promote/fulfill).
- **Possui:** `products.stock` (a contagem — ⚠ tabela compartilhada com Catalog),
  `stock_reservations`, `waitlist_items`.
- **Produz:** `stock.released`, `waitlist.queued/notified/fulfilled/expired`.
- **Consome:** `cart.paid` (fulfill waitlist), `cart.expired`/`stock.released`.
- **Hoje:** `integration` (componente `StockReservations` + métodos de waitlist).
- ⚠ Fulfillment de waitlist roda dentro de `postcheckout.OnCartPaid`.

### H. Catalog — Produto
- **Possui:** `products`, `product_groups`, `product_options`, `product_variant_options`,
  `product_images`, `product_group_images`.
- **Hoje:** `product`, `productgroup`.
- ⚠ A **contagem** de estoque (`products.stock`) é da Inventory, mas mora na tabela do Catalog.

### I. Notification — Comunicação de saída
- **Faz:** mensagens de saída por canal (Instagram DM / WhatsApp / e-mail).
- **Possui:** `notification_logs`, notification settings (em `stores`).
- **Produz:** `notification.requested/sent/failed/skipped` (canal no payload — WS2).
- **Deveria consumir:** `cart.*`, `order.*`, `trial.ending_soon`, `waitlist.notified`.
- **Hoje:** `notification` (`Service.Send` central) + senders em `integration`
  (DM/WhatsApp) + `lib/email`.
- ⚠ Chamado **inline** de vários lugares (postcheckout, integration, recovery),
  ainda não é consumer de fatos. (Componente central já existe → inversão = rewiring.)

### J. Coupon
- **Possui:** `coupons`, `coupon_redemptions`.
- **Produz:** `coupon.applied/confirmed/refunded`. **Consome:** `cart.paid`, `cart.refunded`.
- **Hoje:** `coupon` (já reactor via `couponSyncer` nos ReactCart*).

### K. Shipping — Logística
- **Faz:** cotação de frete, etiquetas, tracking do carrier.
- **Produz:** `shipment.created/tracking_generated/status_updated/delivered`.
- **Hoje:** providers de shipping em `integration` + cotação no `checkout`.

### L. Customer — `customers`. Pacote `customer`.
### M. Store / Account / Platform — `stores`, `memberships`, `store_invitations`, `users`. Pacotes `store`, `member`, `invitation`, `user`.

### N. Integrations — Anti-Corruption Layer (**não é domínio de negócio**)
- **Faz:** adapters técnicos de provider (instagram, pagarme, mercadopago, stripe,
  tiny, melhor-envio/smartenvios).
- **Possui:** `integrations`, `integration_logs`, `oauth_states`, `webhook_events`,
  `idempotency_keys`.
- **Hoje:** `integration/providers`.
- ⚠ **O problema-raiz:** o pacote `integration` mistura este ACL (infra) com os
  domínios de negócio (Payment, ERP, Inventory, senders de Notification, ingestão
  Live) que *usam* o ACL.

---

## 3. Mapa de acoplamento (as ~20 interfaces)

Quem **define** → quem **implementa** → o que dispara (domínio A chama domínio B):

| Interface | Define | Implementa | Chamada cross-domínio |
|---|---|---|---|
| `PostCheckoutHook` | checkout, integration | postcheckout | Payment/Cart → Order (OnCartPaid…) |
| `GMVReporter` | postcheckout | billing | Order → Billing-Ledger (ReportPaidGMV) |
| `BillingGate` | integration | billing | Payment → Billing (paywall + refund credit) |
| `CouponSyncer` / `CouponLifecycle` | integration / checkout | coupon | Payment/Cart → Coupon |
| `CartInvoiceSyncer` | order | integration(ERP) | Order → ERP |
| `ERPFinalizer` / `ERPFinalisationRetrier` | live / postcheckout | integration(ERP) | Live/Order → ERP |
| `ProductSyncer` / `ProductGroupSyncer` | integration | product / productgroup | ERP → Catalog |
| `NotificationService` / `Notifier` / `DMSender` | integration / live / notification | integration / notification | vários → Notification |
| `EmailSender` / `WhatsAppSender` | notification | lib/email / integration | Notification → provider |
| `CustomerUpserter` | live | integration/customer | Live → Customer |
| `BlockedHandleChecker` | live | integration | Live → Cart(blocked) |
| `CartExpiryScheduler` / `EventCloseScheduler` / `TrialReminderScheduler` | integration / live / billing | main (events client) | domínio → scheduler ETA |
| `CartCanceler` | billing(?) | integration | Billing → Cart |

**Leitura:** `integration` aparece dos dois lados (define E implementa) porque
hospeda múltiplos domínios. `live` e `postcheckout` são hubs de orquestração.

---

## 4. As fronteiras mais violadas (prioridade de correção)

1. **`postcheckout.OnCartPaid` = 4 domínios numa função:** Order (timeline/tracking)
   + Billing-Ledger (GMV) + Inventory (waitlist fulfill) + Notification (recibo).
   → Cada um deveria ser reactor independente de `cart.paid`. (Ledger = WS5;
   waitlist e notification = próximos.)
2. **`integration` = 6 domínios + ACL:** Payment, ERP, Inventory, Waitlist,
   Live-ingestão, Product-sync, WhatsApp. → Extrair em pacotes por domínio; deixar
   `integration/providers` só como ACL.
3. **Ledger escrito fire-and-forget** (sem retry) enquanto o lado de refund já é
   reactor → assimetria (WS5).
4. **Cart em 3 pacotes** (live/checkout/integration) → consolidar a entidade Cart.
5. **`tracking_token` no cart** mas é atributo da Order → migra no split Cart/Order.

---

## 5. Modelo alvo (camadas)

```
Providers (ACL / infra)        instagram · pagarme · mp · stripe · tiny · frete
        │  adapters normalizam
        ▼
Domínios de negócio            Live · Cart · Payment · Order · ERP · Inventory ·
  (dono de tabelas +           Catalog · Notification · Coupon · Shipping ·
   produz/consome fatos)       Customer · Billing[Ledger|Subscription] · Store
        │  fatos (cart.paid, cart.refunded, cart.expired, …)
        ▼
Application layer (fino)       reactors que traduzem fato → chamada de domínio
  (a coreografia)              idempotentes, retry+DLQ (ReactCart* hoje)
        │  todos os fatos
        ▼
Analytics exporter (L4)        New Relic (Fase 11)
```

Regra: **cada domínio é dono das suas tabelas e reage a fatos**; ninguém chama o
side-effect de outro domínio inline — emite o fato, quem reage assina. O ACL de
providers é infra, separado dos domínios que o usam.

---

## 6. Decisões (RESOLVIDAS — 2026-07-23)

1. **Billing = 2 sub-domínios.** ✅ Ledger/Finance (nossa receita das vendas) e
   Subscription (mensalidade do lojista) ficam no pacote `billing` com **fronteiras
   internas claras** (não split de pacote agora).
2. **ERP:** domínio próprio consumindo fatos de Cart/Payment — ✅ confirmado (já invertido).
3. **Inventory:** ✅ **extrair de `integration`** para pacote próprio (`inventory`):
   estoque + reservas + waitlist. `products.stock` segue na tabela do Catalog, mas a
   **contagem é da Inventory** (Catalog não escreve stock; Inventory é dona da mutação).
4. **Reactors moram em CADA pacote** ✅ via o padrão **handlers / listeners** (ver §8):
   `handlers/` = ações **síncronas** (use cases do path HTTP/command); `listeners/` =
   **assinantes de eventos** (os reactors que reagem a `cart.paid` etc.). Cada domínio
   é dono dos seus listeners; o `main` compõe o registro.
5. **Extrair Payment e Live-ingestão do `integration`** ✅ para pacotes próprios
   (`payment` — hoje vestigial, vira o real; e a ingestão de comentário volta pra `live`).
6. **Notification:** completar a inversão para consumer de fatos (já tem componente central).

---

## 8. Convenção: handlers vs listeners (por pacote)

Cada domínio expõe dois pontos de entrada, separando **ação síncrona** de
**reação a evento**:

```
internal/<dominio>/
├── service.go        # regra de negócio + acesso a dado (dono das tabelas)
├── handlers/         # AÇÕES SÍNCRONAS — HTTP handlers / commands do request path
│   └── ...           #   ex.: checkout.handlers → ProcessCardPayment (síncrono)
└── listeners/        # ASSINANTES DE EVENTO — reagem a fatos do bus (os reactors)
    └── ...           #   ex.: billing.listeners → OnCartPaid (ledger) reage a cart.paid
```

- **handler** = síncrono, dispara no fluxo da request (ou de um command executor).
  É onde o domínio *age* (ex.: Payment resolve o status e emite o fato).
- **listener** = idempotente, assíncrono, retry+DLQ. É onde o domínio *reage* a um
  fato de outro domínio (ex.: `billing/listeners` escuta `cart.paid` → grava o ledger;
  `inventory/listeners` escuta `cart.paid` → fulfill waitlist; `notification/listeners`
  escuta `cart.*` → envia).
- **CONVENÇÃO DE NOME (regra do dono):** todo listener/reactor é nomeado **`On<Fato>`**
  — `OnCartPaid`, `OnCartRefunded`, `OnCartExpired`, `OnShipmentDelivered`… Um método
  `On<Fato>` = "este domínio reage ao fato `<fato>`". Retorna `error` (o erro sobe pra
  task asynq → retry+DLQ); é idempotente. Nada de nomes ad-hoc tipo `RecordCartSale`
  ou `ReportPaidGMV` para reações a evento — sempre `On<Fato>`. (Handlers síncronos
  seguem o nome do use case, ex. `ProcessCardPayment`; a regra `On<Fato>` é só para
  listeners.)
- O `main.newApp` compõe: registra cada `listener` no `eventsServer` para o fato que
  ele assina. Um fato pode ter N listeners (um por domínio) — hoje eles estão
  amontoados em `ReactCartPaid`; a meta é cada domínio ter o seu.
- **Migração:** os `ReactCart*` atuais (em `integration`) se dissolvem em listeners
  por domínio: `ReactCartPaid` → `order/listeners` (timeline/tracking) +
  `billing/listeners` (ledger) + `inventory/listeners` (waitlist) +
  `coupon/listeners` + `erp/listeners`.

---

## 7. Sequência sugerida (do menor risco ao maior)

1. **WS5 — Ledger como reactor** de `cart.paid` (tira `ReportPaidGMV` do OnCartPaid;
   retry guiado pelo insert do ledger, meter best-effort). Fecha a assimetria.
2. **Waitlist fulfill** e **Notification** como reactors de `cart.paid` (esvazia o OnCartPaid).
3. **Split Cart/Order** (`cart-order-split`): materializar `orders`, mover
   tracking_token/order_events pra Order.
4. **Extrair do `integration`** (por domínio, um de cada vez): Payment → `payment`,
   ERP → `erp`, Inventory → `inventory`, Live-ingestão → `live`.
5. **Fase 11** fecha o loop dando exporter aos fatos que sobraram.
