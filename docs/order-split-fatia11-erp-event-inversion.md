# Fatia 11 — Inversão de evento + autoridade do ERP pós-venda

> Parte final do split Cart/Order, antes da Fatia 10 (D1 — drop das colunas legadas do cart).
> Status: DESENHO (aguardando implementação em loops dev-qa). Base: stg @ d95c337.

## Decisão do dono
- A **reserva pré-pagamento** (máquina CAS de estoque no ERP) **fica no cart pra sempre** — é ciclo de vida do carrinho, liberada na expiração do cart. Não vira Order (coerente com a Fatia 6: a Order só nasce imutável no `cart.paid`).
- **Tudo pós-pagamento** (finalização fiscal + confirmação + refund no ERP) vira **consumidor de eventos `order.*`** — não mais de `cart.*` — e passa a escrever **autoritativamente em `order_payments`**, não no cart.

## Estado atual (confirmado no código, não só nos docs)
- `order.paid`/`order.refunded` **NÃO existem** no barramento. WS1 (`88d6de5`) removeu os consts `order.*` do catálogo; `order_events` (tabela, UNIQUE(order_id,event_type) desde C1) é **ledger de auditoria/idempotência**, não pub/sub.
- Padrão de emissão existente: `events.EmitInternal(ctx, queries, name, dedupKey, payload)` → tabela `event_outbox` (transacional) → relay → asynq → handler registrado no composition root (`cmd/http-server/main.go`).
- Ordem de fan-out já favorável:
  - `cart.paid` (main.go ~983): `OnCartPaid` (materializa Order) **→** `ReactCartPaid` (coupon/billing/tracking/waitlist) **→** `ReactCartPaidERP` (finaliza no Tiny). Order já existe quando o ERP roda. ✓
  - `cart.refunded` (main.go ~1011): `OnCartRefunded` (flipa Order→refunded, E1) **→** `ReactCartRefunded` (coupon refund + billing credit + `RefundConvertedCartOrder` no ERP).
  - `cart.expired` (main.go ~1068): `ReactCartExpiredERP` (cancela reserva pré-paga no Tiny). **Fica como está** — pré-pagamento, sem Order.
- `ConfirmERPOrderPayment` (cart.paid) e `RefundConvertedCartOrder` (cart.refunded) já são reactors assíncronos (asynq, retry+DLQ). `CancelERPOrderForCart` rejeita explicitamente estado `confirmed` → é 100% pré-pagamento.

## Autoridade de colunas (o que a Fatia 10/D1 vai poder dropar do cart depois)
**Move pra `order_payments` (autoritativo):**
- `erp_finalisation_status`, `erp_last_error`, `erp_last_attempt_at`, `erp_attempts_count` (já existem em order_payments desde 000094, hoje só espelhados)
- `erp_payment_snapshot` (**nova coluna** em order_payments — hoje só no cart)
- `erp_invoice_id`, `erp_invoice_key`, `erp_invoice_status`, `erp_invoice_emitted_at` (já existem como `invoice_*` em order_payments)

**Fica no cart PRA SEMPRE (reserva, fora do escopo do split):**
- `erp_order_state`, `erp_stock_launched`, `erp_op_started_at` (máquina CAS de reserva)
- `external_order_id` (id do pedido Tiny, criado na reserva pré-pagamento; usado pós-pagamento como chave de resolução do webhook de NF → order_payments)

## Design

### Sub-fatia 11a — introduzir os eventos `order.*` no barramento (ADITIVO, baixo risco)
1. `internal/events/types.go`: consts `OrderPaid Name = "order.paid"`, `OrderRefunded Name = "order.refunded"` (novo grupo O, documentado).
2. Payloads:
   - `order.paid`: `{ OrderID, CartID, StoreID, GMVCents, PaymentSnapshot json.RawMessage }`
   - `order.refunded`: `{ OrderID, CartID, StoreID }`
3. Emissão transacional (mesmo tx da materialização/flip, via outbox → exactly-once):
   - `order/listeners/on_cart_paid.go`: após `InsertOrder`, `EmitInternal(..., OrderPaid, "order.paid:"+orderID, payload)`.
   - `order/listeners/on_cart_refunded.go`: após flip, `EmitInternal(..., OrderRefunded, "order.refunded:"+orderID, payload)`.
   - Listeners já têm `*sqlc.Queries`; só adicionar o emit.
4. Nesta sub-fatia NÃO há consumidor ainda (evento emitido sem consumer é inócuo). Teste: evento aparece no `event_outbox` com dedup por order_id, exactly-once em replay.

### Sub-fatia 11b — migrar ERP pós-pagamento pra consumir `order.*` + escrever em order_payments (RISCO ALTO)
1. **order_payments ganha `erp_payment_snapshot JSONB`** (migration) + backfill do cart pras orders pagas existentes.
2. **Novo handler `order.paid`** no composition root (`main.go`), registrando `ReactOrderPaidERP(ctx, orderID, cartID, storeID, paymentSnapshot)`:
   - Reusa a lógica de `ReactCartPaidERP`/`finalizeOrConfirmCartERP`/`ConfirmERPOrderPayment`, MAS as marcas de finalização (`MarkCartERPFinalisationDone/Failed`) e o snapshot passam a escrever em `order_payments` (resolvendo order_id — já vem no payload).
   - Remover a chamada `ReactCartPaidERP` do handler de `cart.paid` (main.go ~1009).
3. **Novo handler `order.refunded`** no composition root, registrando a parte ERP do refund (`RefundConvertedCartOrder`) — remover essa chamada de dentro de `ReactCartRefunded` (que mantém coupon refund + billing credit em `cart.refunded`).
   - Nota: o refund no ERP é majoritariamente reserva-state (CAS confirmed→cancelled, fica no cart); o que muda aqui é a FONTE do trigger (order.refunded) e qualquer escrita de finalização vai pra order_payments.
4. **Webhook de nota fiscal** (`webhook_handler.go`, `nota_fiscal`): resolve `external_order_id → order_payments` (via order) e escreve `invoice_*` autoritativamente em order_payments (não mais no cart). `external_order_id` continua no cart como origem; a resolução usa o valor espelhado/o GetOrderIDByCartID.
5. **Retry admin** (`RetryERPFinalisation`) e **dashboard/detalhe** passam a ler `erp_finalisation_*`/`erp_payment_snapshot`/`invoice_*` de order_payments (dashboard já lê do mirror — vira autoritativo, sem mudança).
6. **Mirror** (`MirrorCartERPToOrder`): as projeções de finalização/invoice viram redundantes (agora escrita direta) → remover essas do mirror; manter só a projeção de reserva-state se ainda for útil ao detalhe.

### Fatia 10 (D1) — depois de 11b estável
Dropar do cart: `erp_finalisation_status`, `erp_last_error`, `erp_last_attempt_at`, `erp_attempts_count`, `erp_payment_snapshot`, `erp_invoice_*`, **+** `customer_*`, `shipping_*`, `tracking_token` (já cutover em B1/C1). Manter reserva + `external_order_id`.

## Fora de escopo (deferrals explícitos)
- Inversão dos OUTROS consumidores de cart.* (coupon, billing, notification, waitlist) — continuam em cart.paid/cart.refunded. Só o ERP migra pra order.*.
- `ReactCartExpiredERP` continua em `cart.expired` (não existe `order.expired`; Order não expira).
- Ownership da máquina de reserva (flip de posse do CAS) — decidido: NUNCA sai do cart.

## Riscos
- Desacoplar o ERP do task de `cart.paid` muda semântica de retry (agora task separado `order.paid`). Mitigação: emissão via outbox no mesmo tx da materialização = exactly-once garantido; o ERP retenta isolado sem re-rodar billing/coupon.
- Webhook de NF resolvendo por `external_order_id`: garantir que a order já exista (invoice é sempre pós-confirmação, logo pós-pagamento → order existe). Cobrir com teste o caso de NF chegando pra cart sem order (skip benigno/log).
- Backfill de `erp_payment_snapshot`: cart pode não ter snapshot (só grava em falha de finalização) → NULL ok.
