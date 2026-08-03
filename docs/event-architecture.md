# Event Architecture — Choreography (command → fact → N consumers)

> **Status:** north-star design. Substitui a mentalidade "emitir fatos para logEvent"
> pela coreografia real. A auditoria (`event-audit.md`) catalogou o *vocabulário*;
> este doc define o *fluxo* — e do fluxo cai naturalmente (a) command vs fact por
> papel, (b) quais consumers construir, (c) **quais eventos depreciar**.

---

## 1. O modelo em 4 camadas

```
L1  Dispatcher (fino)      webhook / handler HTTP. Valida + emite um COMMAND. Zero lógica de domínio.
        │ command
        ▼
L2  Command executor       1 consumer por command. Faz a transição GUARDADA. Emite 1 FACT (por desfecho).
        │ fact
        ▼
L3  Fact reactors (N)      0..N services reagem ao fact, cada um idempotente. Podem emitir novos facts.
        │ facts
        ▼
L4  Telemetry exporter     1 consumer assina TODOS os facts → New Relic (o stream de analytics, Fase 11).
```

**Regras:**
- **Command** = imperativo (`payment.process`, `cart.expire`). Exatamente **1 executor**. Retriável. Se falha, a ação não aconteceu.
- **Fact** = passado (`cart.paid`, `payment.failed`). **0..N reactors**. O estado já mudou; o reactor faz side-effect.
- Um produtor **nunca** chama o side-effect de outro domínio inline. Emite o fact; quem reage assina.
- **Coreografia, não orquestração.** Cada reactor é independente — não há "GMV antes de Order". A "história" vive no grafo (L4/New Relic dá a visão).
- **Consumer = in-process** (como `comment.received`→ProcessInstagramComment já é hoje). Extraível pra microserviço depois sem mudar o contrato de evento.

---

## 2. Topologia de serviços-consumidores (o alvo)

| Service (consumer) | Assina (facts) | Faz | Re-emite |
|---|---|---|---|
| **PaymentProcessor** (L2) | `payment.process` (cmd) | GetPaymentStatus + UpdateCartPaymentStatus guardado | `cart.paid` / `payment.failed` / `cart.refunded` / `cart.cancelled` |
| **GMV/Billing** | `cart.paid`, `cart.refunded` | ledger + Stripe meter / crédito de taxa | `gmv.recorded` / `gmv.refunded` |
| **Order** | `cart.paid`, `cart.refunded`, `cart.cancelled`, `shipment.posted`, `delivery.confirmed` | order_events (timeline) + e-mail transacional | — (order_events é interno) |
| **ERP/Tiny** | `cart.paid`, `cart.refunded`, `cart.expired` | finaliza/cancela pedido no Tiny (máquina de estados) | `erp.order_created/finalized/cancelled/failed` |
| **Waitlist** | `cart.paid`, `stock.released` | fulfill / promove próximo | `waitlist.fulfilled/notified` |
| **Coupon** | `cart.paid`, `cart.refunded` | confirma / estorna redemption | `coupon.confirmed/refunded` |
| **Notification** | `cart.item_added`, `cart.checkout_armed`, `cart.paid`, `order.*`, `trial.ending_soon`, `waitlist.notified` | DM / WhatsApp / e-mail idempotente | `notification.sent/failed` |
| **Analytics exporter** (L4) | **todos os facts** | export → New Relic | — |

**Insight:** hoje esses "services" são os métodos inline `OnCartPaid`, `finalizeCartERPOrder`, `couponSyncer`, `billingGate`, `notify`. A inversão = **cada um vira um consumer** de `cart.paid` (e amigos), no lugar de serem chamados em cascata pelo webhook.

---

## 3. Fluxo de PAGAMENTO (template detalhado — o exemplo do dono)

### Hoje (acoplado, inline)
`pagarme webhook → ProcessPaymentNotification` faz **tudo em cascata inline**: UpdateCartPaymentStatus + couponSyncer.OnCartPaid/Refunded + billingGate.OnCartRefunded + postCheckoutHook.OnCartPaid/Cancelled/Refunded + RefundConvertedCartOrder + finalizeCartERPOrder. Emite `payment.succeeded`/`order.payment_confirmed` como facts **só-logEvent**. → 1 função gigante que conhece GMV, order, ERP, cupom, e-mail.

### Alvo (coreografia)
```
L1  pagarme/mercadopago/stripe webhook
        │ emite  payment.process {payment_id, provider, store_id}          [COMMAND]
        ▼
L2  [PaymentProcessor]
        GetPaymentStatus(provider) → resolve
        UpdateCartPaymentStatus  (GUARDADO: advisory lock + WHERE not expired/cancelled)
        emite UM fact por desfecho:
          approved → cart.paid {cart_id, payment_id, amount_cents, provider}
          rejected → payment.failed
          refunded → cart.refunded
          canceled → cart.cancelled
        ▼
L3  cart.paid  ─fan-out──▶ [GMV]      ledger + meter        → gmv.recorded
                ├─────────▶ [Order]    order_events + e-mail  (interno)
                ├─────────▶ [ERP]      finaliza Tiny          → erp.order_created/finalized
                ├─────────▶ [Waitlist] fulfill                → waitlist.fulfilled
                └─────────▶ [Coupon]   confirma redemption    → coupon.confirmed
    cart.refunded ─▶ [GMV] estorno+crédito · [Order] e-mail · [ERP] cancela · [Coupon] estorna
```

**Guards preservados:** o `UpdateCartPaymentStatus` continua guard-first (advisory lock pagamento×expiração). Cada reactor é idempotente pelos guards que já existem (`order_events UNIQUE`, `billing_ledger UNIQUE`, dedup_key no outbox). `cart.paid` consumido 3× = cada serviço age uma vez.

### O que este fluxo DEPRECIA
- **`payment.succeeded`** → vira `cart.paid` (o fact canônico do fan-out). O nome "cart.paid" é melhor: é o carrinho que foi pago, e é o que os serviços reagem.
- **`payment.processing`** → sem reactor; telemetria. Deprecia (ou fica só no stream).
- **`order.payment_confirmed` / `order.cancelled` / `order.refunded`** → **interno do Order Service** (linhas em `order_events`), NÃO eventos de barramento. Deprecia do bus.
- **`checkout.initiated` / `pix.generated`** → funil; mantêm só se o Analytics exporter os usar. Senão, depreciam.

---

## 4. Os outros fluxos (esboço — expandir depois)

- **Checkout/carrinho:** `AddToCart` → `cart.item_added` (fact) → [Notification] manda DM. `FinalizeCartsByEvent` → `cart.checkout_armed` (fact) → [Notification] manda link + [scheduler] arma `cart.expire`.
- **Expiração (ETA):** `cart.expire`/`event.window_close` (commands) → executor → `cart.expired`/`post.window_closed` (facts) → [ERP] reverte, [Waitlist] promove, [Stock] devolve.
- **Live/comentário:** JÁ invertido — `comment.received` (fact-como-command) → [CommentProcessor]. Serve de referência.
- **Billing:** webhooks Stripe → `subscription.process` (command) → [SubscriptionProcessor] → `subscription.activated/past_due/...` (facts) → [Notification], [Paywall]. `trial.ending_soon` **vira command** (`trial.remind`).
- **Waitlist/estoque:** facts `stock.released` → [Waitlist] promove.

---

## 5. Framework de depreciação (o "vários eventos vão morrer")

Um evento **sobrevive** se for: **(C)** command num fluxo, **(F)** fact canônico com ≥1 reactor de domínio, ou **(A)** fact que o Analytics exporter comprometidamente consome. Todo o resto: **DEPRECIA** (telemetria órfã), **MERGE** (redundante) ou **INTERNO** (detalhe de um serviço, não do bus).

| Verdito | Eventos (dos 87) | Motivo |
|---|---|---|
| **KEEP — Command** | `payment.process`*, `subscription.process`*, `cart.expire`, `event.window_close`, `trial.remind`* (renomear) | 1 executor, imperativo (* = novos/renome) |
| **KEEP — Fact canônico** | `cart.paid`* (ex-payment.succeeded), `cart.refunded`*, `payment.failed`, `cart.cancelled`, `cart.expired`, `cart.item_added`, `cart.checkout_armed`, `comment.received`, `post.window_closed`, `event.created/ended`, `session.created/ended`, `stock.released`, `waitlist.queued/notified/fulfilled`, `subscription.*`, `gmv.recorded/refunded`, `shipment.created/status_updated/delivered`, `store.created`, `member.*`, `user.*`, `customer.upserted`, `coupon.applied/confirmed/refunded` | têm (ou terão) reactor de domínio |
| **MERGE** | `email.sent` + `whatsapp.fallback_*` → `notification.sent/failed` (canal no `Source`); `payment.chargeback` → dentro de `cart.refunded` c/ `reason` | fragmentação do mesmo fato |
| **INTERNO (tirar do bus)** | `order.payment_confirmed/cancelled/refunded/shipped/delivered` | são order_events (timeline do Order Service), não barramento |
| **DEPRECAR** | `payment.processing`, `payment.succeeded`(→cart.paid), `checkout.initiated`, `pix.generated/expired`, `cart.created`, `cart.item_qty_changed`, `cart.item_removed`, `stock.reserved`, `session.live`, `product.synced/imported`, `erp.order_initiated`, `conversion.initiated`, `coupon.created/updated/deleted/removed/redemption_expired`, `event.started`(morto), `trial.started`(morto) | telemetria órfã / alta cardinalidade / redundante / morto |

> Números aproximados: dos 87, ~**30 depreciam/viram internos**, ~**8 merge**, restando um catálogo enxuto de **commands + facts canônicos** com dono claro. A auditoria (`event-audit.md`) tem o detalhe por evento.

---

## 6. Decisões abertas (validar antes de executar)
1. **Nomear o fact de pagamento `cart.paid` vs `payment.confirmed`?** (proponho `cart.paid` — o carrinho é o agregado que os serviços reagem).
2. **Order events no bus ou internos?** (proponho internos — timeline é do Order Service; se algum dia alguém externo precisar, o Order emite seu próprio fact).
3. **Manter os facts de plataforma/coupon (grupo K) só pro stream de analytics, ou depreciar?** (depende do valor analítico — decidir no §5 do error/observability).
4. **Ordem de execução da inversão:** pagamento primeiro (mais rico, mais risco) e usar como template.

---

## 7. Plano de execução (derivado)
1. **Validar este mapa** (esp. o fluxo de pagamento) — é o contrato.
2. **Inverter pagamento:** webhook vira dispatcher (`payment.process`); criar [PaymentProcessor] + os reactors [GMV]/[Order]/[ERP]/[Waitlist]/[Coupon] consumindo `cart.paid`. Migrar `OnCartPaid` inline → reactors, um a um, com o inline como fallback até cada reactor estar validado.
3. **Depreciar** os eventos do §5 (marcar deprecated → remover após 1 ciclo sem uso).
4. **Renomear** commands/facts (P0/P1 da auditoria) — barato agora, sem consumer externo.
5. **Fase 11:** o Analytics exporter (L4) → New Relic fecha o loop dos facts que sobreviveram.
