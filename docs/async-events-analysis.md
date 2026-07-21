# LiveCart — Arquitetura de Eventos Assíncronos: Análise & Catálogo Revisado

> **Status:** proposta / fonte-da-verdade para as issues de implementação
> **Base:** `livecart-async-events-plan.md` (rascunho original, criado fora da codebase)
> **Este doc:** confronta o plano com os fluxos reais do backend e define o catálogo
> completo de eventos, o mapa evento→infra existente e o roadmap de fases.

---

## 0. TL;DR

O plano original é um bom **esqueleto de infraestrutura** (asynq + Redis + OTEL/Datadog,
propagação de trace W3C), mas foi escrito sem olhar a codebase. Consequências:

1. **Catálogo de eventos ~30% completo.** Cobre só o funil feliz da live
   (comentário → carrinho → pagamento aprovado → pedido). Ignora 5 subsistemas
   inteiros: **billing/assinatura, notificações, ERP/Tiny, shipping e plataforma/conta**.
2. **Premissas arquiteturais furadas.** Assume "sem workers", `net/http`, módulo
   `github.com/livecart` e greenfield. A realidade é: **7 workers em goroutine** já
   rodando no processo `http-server`, **Fiber v2**, módulo `livecart`, e um conjunto
   maduro de **guardas de idempotência e locks de corrida** que uma fila ingênua
   quebraria.
3. **Já existe um "event log" de fato** em Postgres (`webhook_events`,
   `notification_logs`, `integration_logs`, `order_events`). O novo sistema deve
   **construir sobre** isso, não reinventar.

Objetivo do trabalho: transformar o sistema em event-driven **de verdade** (não só
para a live) e elevar a telemetria, **reusando** os workers e as guardas existentes.

---

## 1. Divergências técnicas do plano vs. stack real

| Item | Plano original | Realidade da codebase | Ação |
|---|---|---|---|
| Framework HTTP | `net/http` handlers | **Fiber v2** (`gofiber/fiber/v2`) | Handlers/publish via Fiber `c.UserContext()` |
| Módulo Go | `github.com/livecart/...` | `livecart` (ver `go.mod`) | Corrigir todos os imports |
| Fila | Redis + asynq (greenfield) | **Nada** hoje (SQS legado já removido) | Adotar asynq+Redis, mas ver §3 |
| Workers | Binário `cmd/worker` separado | **7 goroutines** dentro de `http-server` | Migrar incrementalmente, não recriar |
| Telemetria | OTEL + Datadog do zero | `zap` + `logger.From(ctx)`/`WithStore` (store_id/slug automáticos) | OTEL **integra** com o zap atual |
| Idempotência | `MaxRetry(3)` ingênuo | Unique constraints + advisory locks + tracking_token | Envelope de evento respeita as guardas |
| Real-time | `SESSAO_LIVE` / WebSocket | Só polling (`EventPulse`) | WebSocket é item futuro, não base |
| Infra local | Redis + Datadog agent no compose | Só Postgres (+ tunnel dev) | Adicionar serviço `redis` ao compose |

---

## 2. Premissas do plano que quebrariam produção

Estas são as guardas que **já funcionam** e que a nova fila **precisa respeitar**:

- **Corrida pagamento × expiração** — `ExpireCart` e o webhook de pagamento disputam o
  mesmo carrinho. Resolvido com **advisory lock** por carrinho
  (`AcquireCartFinalisationLock`) + guarda SQL (`WHERE status NOT IN ('expired','cancelled')`)
  + `expires_at = NULL` ao pagar. Um consumidor asynq que ignore isso reintroduz
  **oversell** e **dupla-finalização**.
- **Exactly-once de side-effects** — `order_events UNIQUE(cart_id, event_type)` garante
  1 e-mail por transição; `billing_ledger UNIQUE(cart_id,'sale')` garante 1 cobrança de
  GMV por venda; `tracking_token` (set-once) protege o e-mail de "pago". Retry de fila
  **não pode** duplicar isso.
- **Waitlist all-or-nothing** — promoção usa `FOR UPDATE SKIP LOCKED`
  (`ClaimNextWaitlistItem`) e re-securização de estoque com rollback. É correção contra
  double-claim; o evento é observabilidade, não substituição da lógica.
- **Best-effort ERP** — falhas de Tiny são logadas e reconciliadas depois, **nunca**
  propagam erro ao webhook. Eventos ERP devem seguir o mesmo princípio (não travar o
  produtor).

> **Diretriz de design:** eventos são **camada de observabilidade + fan-out de
> side-effects secundários**, montada por cima das transições que já existem. A
> primeira migração de cada fluxo é *"emitir evento + mover o side-effect assíncrono
> atual para consumidor"*, preservando as guardas.

---

## 3. Estratégia: fila nova × workers existentes

Hoje, dentro de `cmd/http-server/main.go`, rodam como goroutines:

| Worker | Intervalo | Função |
|---|---|---|
| Token refresh | 5 min | Renova tokens de integração perto de expirar |
| Tracking poller | 6 h | Consulta transportadora, dispara `OnDelivered` |
| Post comment polling | — | Coleta comentários de posts ativos (até webhook assumir) |
| ERP order sweep + `SweepEndedTimedEvents` | 5 min | Finaliza pedidos ERP + fecha eventos post/story vencidos |
| Coupon expiry | 5 min | `ExpireStaleReservedRedemptions` |
| Cart recovery (WhatsApp) | 5 min | PRD 006 — re-securiza estoque + regenera checkout + WhatsApp |
| Cart expiry | 5 min | Expira carrinhos, libera estoque, reverte ERP, promove waitlist |

**Plano de convivência:**
- **Fase infra:** asynq/Redis entram **no mesmo processo** (`http-server`), como mais um
  conjunto de workers. Não criar `cmd/worker` separado ainda — só quando houver ganho
  real de escala/isolamento.
- **Sweeps → híbrido:** os sweeps de 5 min continuam como *safety net* (garantia de
  liveness mesmo se um evento se perder), mas passam a **emitir eventos** ao agir. Eventos
  dão reação em tempo real; o sweep garante o *floor*.
- **Outbox:** transições críticas (pagamento, GMV, order_events) publicam via **outbox
  transacional** (grava evento na mesma tx do estado) para não perder evento em crash.

---

## 4. Catálogo de eventos revisado (5 subsistemas)

Convenção de nomes: `DOMINIO_FATO` no passado. Cada evento lista: **gatilho** (onde
emitir), **side-effects atuais**, **chave de idempotência** e **worker/infra a reusar**.

### A. Live / Sessão / Evento
| Evento | Gatilho (fn/arquivo) | Side-effects hoje | Idempotência |
|---|---|---|---|
| `EVENT_CREATED` | `live.Service.Create` | INSERT live_events (+sessão/plataforma) | event_id |
| `EVENT_SCHEDULED` | Create com `ScheduledAt` | status=scheduled | event_id |
| `EVENT_STARTED` | window abre / status=active | — (status computado) | event_id |
| `EVENT_ENDED` | `live.Service.End` | finaliza carrinhos, encerra sessões, ERP async, auto-DM checkout | status=ended (guard) |
| `SESSION_CREATED` | `live.Service.CreateSession` | INSERT sessão + platform binding | session_id |
| `SESSION_LIVE` | `StartSession` | status=live, started_at | session_id |
| `SESSION_ENDED` | `EndSession` | ended_at | session_id |
| `POST_WINDOW_CLOSED` | `SweepEndedTimedEvents` / `EndPostEventByMediaID` | finaliza carrinhos post/story | event_id |

### B. Comentário (Instagram/Story/DM)
| Evento | Gatilho | Side-effects | Idempotência |
|---|---|---|---|
| `COMMENT_RECEIVED` | `ProcessInstagramComment` | INSERT live_comment, incrementa contador | platform_comment_id |
| `COMMENT_MATCHED` | intent + produto resolvido | segue p/ carrinho | platform_comment_id |
| `COMMENT_UNMATCHED` | result=`no_product`/`no_intent` | só auditoria | platform_comment_id |
| `COMMENT_PAUSED` | processing_paused | result=paused | platform_comment_id |
| `COMMENT_BLOCKED` | handle bloqueado | result=blocked | platform_comment_id |
| `COMMENT_REJECTED_WINDOW` | post não iniciado/encerrado/fora de promo | reply privado | platform_comment_id |
| `STORY_REPLY_RECEIVED` | `processStoryReply` | reusa pipeline de comentário | platform_message_id |

### C. Carrinho
| Evento | Gatilho | Side-effects | Idempotência |
|---|---|---|---|
| `CART_CREATED` | `AddToCart`/`GetOrCreateCart` (novo) | INSERT cart, arma expires_at | cart_id |
| `CART_ITEM_ADDED` | `UpsertCartItem` | item + notificação DM (immediate/item_added) | cart_id+product_id+comment |
| `CART_ITEM_QTY_CHANGED` | mutations checkout | ajusta reserva ERP (delta), audit cart_mutations | mutation_id |
| `CART_ITEM_REMOVED` | mutation | promove waitlist do produto | mutation_id |
| `CART_CHECKOUT_ARMED` | `FinalizeCartsByEvent` (fim do evento) | status=checkout, arma expires_at | cart_id (status guard) |
| `CART_EXPIRED` | `ExpireCart` (worker 5min) | libera estoque local+ERP, promove waitlist | cart_id (guard-first flip) |
| `CART_REOPENED` | `ReopenExpiredCartForReuse` | reset itens/ERP, re-securiza estoque | cart_id |
| `CART_CANCELLED` | `CancelCartAsBlocked` | libera estoque, reverte ERP | cart_id |

### D. Estoque & Waitlist
| Evento | Gatilho | Side-effects | Idempotência |
|---|---|---|---|
| `STOCK_RESERVED` | `DecrementProductStock` (+ Tiny saída) | reserva local; design-A saída-manual | reservation_id |
| `STOCK_RELEASED` | `IncrementProductStock` (+ Tiny reversal) | expiry/cancel/remove | cart_id+product_id+op |
| `WAITLIST_QUEUED` | `CreateWaitlistItem` | status=waiting, posição | waitlist_item_id |
| `WAITLIST_NOTIFIED` | `ProcessWaitlistForProduct` | claim SKIP LOCKED, DM, estende expires_at | waitlist_item_id (status) |
| `WAITLIST_FULFILLED` | pagamento dentro da janela | marca fulfilled | waitlist_item_id |
| `WAITLIST_EXPIRED` | janela de notificação vence | volta p/ waiting ou libera | waitlist_item_id |

### E. Checkout & Pagamento
| Evento | Gatilho | Side-effects | Idempotência |
|---|---|---|---|
| `CHECKOUT_INITIATED` | `ProcessCardPayment`/`GeneratePix`/`GenerateCheckout` | salva cliente, pré-converte ERP (design-C) | cart_id |
| `PIX_GENERATED` | `GeneratePix` | QR + desconto PIX | payment_id |
| `PIX_EXPIRED` | janela vence sem pagamento | — (hoje sem notificação) | payment_id |
| `PAYMENT_PROCESSING` | webhook pending/in_process | status=pending | payment_id |
| `PAYMENT_SUCCEEDED` | approved (`UpdateCartPaymentStatus`→paid) | `OnCartPaid`: GMV, order_event, e-mail, tracking, waitlist→fulfilled; finaliza ERP | order_events(cart_id,'payment_confirmed') |
| `PAYMENT_FAILED` | rejected → status=failed | **hoje: nenhuma reação** (gap) | payment_id |
| `PAYMENT_REFUNDED` | refunded | e-mail estorno, reverte ERP, crédito de taxa (billing) | order_events(cart_id,'payment_refunded') |
| `PAYMENT_CHARGEBACK` | cancel após pago | conflado com refund hoje (distinguir) | payment_id |

### F. Pedido (timeline `order_events`)
| Evento | Gatilho | Idempotência |
|---|---|---|
| `ORDER_PAYMENT_CONFIRMED` | `OnCartPaid` | UNIQUE(cart_id,'payment_confirmed') |
| `ORDER_CANCELLED` | `OnCartCancelled` | UNIQUE(cart_id,'payment_cancelled') |
| `ORDER_REFUNDED` | `OnCartRefunded` | UNIQUE(cart_id,'payment_refunded') |
| `ORDER_SHIPPED` | `OnShipmentPosted` | UNIQUE(cart_id,'shipped') |
| `ORDER_DELIVERED` | `OnDelivered` | UNIQUE(cart_id,'delivered') |

### G. ERP / Tiny (outbound)
| Evento | Gatilho | Notas |
|---|---|---|
| `ERP_STOCK_RESERVED` | reserva saída-manual | design-A |
| `ERP_ORDER_INITIATED` | `finalizeOrConfirmCartERP` | máquina de estados (advisory lock) |
| `ERP_ORDER_CREATED` | pedido criado no Tiny | resume-safe |
| `ERP_ORDER_FINALIZED` | estado terminal `[S4]` | idempotente |
| `ERP_ORDER_CANCELLED` | `RefundConvertedCartOrder` | situação 2 + estorno estoque |
| `ERP_FINALIZATION_FAILED` | erro Tiny | best-effort, reconciliação |
| `PRODUCT_SYNCED` / `PRODUCT_IMPORTED` | `ProductSyncer` | `FilterRegisteredExternalIDs` |

### H. Shipping / Entrega
| Evento | Gatilho | Notas |
|---|---|---|
| `SHIPMENT_CREATED` | `CreateShipment` (shipping_handler) | Melhor Envio/SmartEnvios |
| `TRACKING_CODE_GENERATED` | código retornado | dispara `OnShipmentPosted` |
| `SHIPMENT_STATUS_UPDATED` | `ApplyMelhorEnvioWebhook` | posted/in_transit/out_for_delivery/returned |
| `DELIVERY_CONFIRMED` | tracking poller | dispara `OnDelivered` |

### I. Notificações (Comms)
| Evento | Gatilho | Canais |
|---|---|---|
| `NOTIFICATION_REQUESTED` | `notification.Service.Send` | — |
| `NOTIFICATION_SENT` | reply/DM ok | instagram_dm |
| `NOTIFICATION_FAILED` | falha envio | — |
| `NOTIFICATION_SKIPPED` | desabilitado nas settings | — |
| `WHATSAPP_FALLBACK_SENT`/`FAILED` | `whatsapp_fallback` (Twilio) | whatsapp (LGPD, janela 24h) |
| `EMAIL_SENT` | `lib/email` (Resend) | email (paid/cancelled/refunded/shipped/delivered) |

> Tipos existentes: `checkout_immediate`, `item_added`, `checkout_reminder`,
> `waitlist_notified`, `cart_recovery`. Persistidos em `notification_logs`.

### J. Billing / Assinatura (Stripe, PRD 007)
| Evento | Gatilho | Notas |
|---|---|---|
| `TRIAL_STARTED` | `EnsureTrialSubscription` | trial 7d |
| `TRIAL_ENDING_SOON` | **novo** (não existe) | precisa job (X dias antes) |
| `TRIAL_EXPIRED` | `blocked()` quando trial_ends_at passa | paywall 402 |
| `CONVERSION_INITIATED` | `CreateConversionCheckout` | Stripe Checkout setup mode |
| `SUBSCRIPTION_ACTIVATED` | `completeConversion` (webhook) | plano ativo |
| `SUBSCRIPTION_PAST_DUE` | webhook updated | grace 7d |
| `SUBSCRIPTION_GRACE_EXPIRED` | grace vence | paywall |
| `SUBSCRIPTION_CANCELED` | subscription.deleted | — |
| `SUBSCRIPTION_PAUSED`/`RESUMED` | webhook paused/resumed | — |
| `GMV_RECORDED` | `ReportPaidGMV` | ledger + meter Stripe (idemp. `gmv-<cart_id>`) |
| `GMV_REFUNDED` / `FEE_CREDITED` | `OnCartRefunded` | crédito de saldo Stripe |

Webhooks Stripe consumidos: `customer.subscription.created/updated/deleted/paused/resumed`,
`checkout.session.completed` (setup mode), `invoice.payment_failed`.

### K. Plataforma / Conta
| Evento | Gatilho | Notas |
|---|---|---|
| `STORE_CREATED` | `store.Service.Create` | cria membership + trial |
| `STORE_SETTINGS_UPDATED` | `Update`/`UpdateCartSettings`/`UpdateShippingDefaults`/`UpdateLogoURL` | granular |
| `MEMBER_INVITED` | `invitation.Service.Create` | e-mail |
| `MEMBER_INVITE_ACCEPTED` | `Accept` | swap atômico de loja |
| `MEMBER_INVITE_REVOKED`/`RESENT` | `Revoke`/`Resend` | — |
| `MEMBER_ROLE_CHANGED` / `MEMBER_REMOVED` | `member.Service.UpdateRole`/`Remove` | — |
| `USER_SIGNED_UP` / `USER_UPDATED` / `USER_DELETED` | webhooks Clerk (`user/webhook.go`) | — |
| `CUSTOMER_UPSERTED` | `customer.Service.Upsert` | via comentário live |
| `COUPON_CREATED`/`UPDATED`/`DELETED` | `coupon.Service.*` | — |
| `COUPON_APPLIED` / `COUPON_REMOVED` | `ApplyToCart`/`RemoveFromCart` | redemptions reserved |
| `COUPON_CONFIRMED` / `COUPON_REFUNDED` | `ConfirmRedemption`/`RefundRedemption` | via pagamento |
| `COUPON_REDEMPTION_EXPIRED` | `ExpireStaleReservedRedemptions` (worker 5min) | — |

---

## 5. Roadmap de fases (→ issues Linear)

Ordem cronológica, com dependências. Cada fase = 1 issue.

1. **Infra base de eventos + Redis/asynq** — client/server no `http-server`, filas
   (`fast-track`/`normal`/`batch`), envelope de evento com `event_id`/`trace_id`/dedup key,
   serviço `redis` no compose. *Sem migrar fluxo ainda.*
2. **Telemetria OTEL + integração com zap** — tracer/meter provider, propagação W3C,
   middleware Fiber, `logger.From` enriquecido com trace_id, exporter OTLP→Datadog.
3. **Outbox transacional + idempotência** — tabela outbox, publisher que grava evento na
   mesma tx do estado, reaproveitando unique constraints existentes.
4. **Core comercial — live/comentário/carrinho** — emitir eventos A/B/C reusando os
   handlers de webhook e o cart expiry worker.
5. **Estoque & waitlist** — eventos D; sweeps passam a emitir; consumidores de observabilidade.
6. **Pagamento & pedido (todos os desfechos)** — eventos E/F, incluindo os gaps
   `PAYMENT_FAILED` e `PIX_EXPIRED`; respeitar advisory lock.
7. **Billing/assinatura** — eventos J; novo job `TRIAL_ENDING_SOON`.
8. **Notificações como consumidores** — eventos I; mover envio DM/WhatsApp/e-mail para
   consumers idempotentes.
9. **ERP/Tiny + Shipping** — eventos G/H; best-effort + reconciliação.
10. **Plataforma/Conta** — eventos K (store/member/user/coupon/customer).
11. **Observabilidade final** — dashboards Datadog, `/metrics` Prometheus, SLOs e alertas.

---

## 6. Referências de código (âncoras)

- Workers no processo único: `apps/api/cmd/http-server/main.go`
- Corrida pagamento×expiração: `apps/api/internal/integration/service.go` (`ExpireCart`,
  `AcquireCartFinalisationLock`), `apps/api/internal/checkout/service.go` (`UpdateCartPayment`)
- Cart expiry worker: `apps/api/internal/integration/expiry_worker.go`
- Recovery worker: `apps/api/internal/recovery/worker.go`
- Post-checkout (e-mails/tracking/GMV): `apps/api/internal/postcheckout/service.go`
- Billing/Stripe: `apps/api/internal/billing/service.go`
- Notificações: `apps/api/internal/notification/service.go`, `whatsapp_fallback.go`
- ERP/Tiny + shipping: `apps/api/internal/integration/service.go`, `shipping_handler.go`,
  `tracking_poller.go`
- Logger/contexto: `apps/api/lib/logger/logger.go`, `context.go`
- Audit/event log existente: tabelas `webhook_events`, `notification_logs`,
  `integration_logs`, `order_events`
