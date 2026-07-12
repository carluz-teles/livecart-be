# Mapa técnico do fluxo de live commerce

Referência consolidada (verificada no código em 2026-07) para a skill `live-flow-e2e`. Caminhos de backend relativos a `apps/api/`; frontend em `/home/carluz_teles/livecart-fe`.

## Visão geral do pipeline

```
Merchant cria evento ──► Instagram envia comentário (webhook) ──► parsing de intenção
      │                                                                 │
      ▼                                                                 ▼
live_events + live_sessions + live_session_platforms          match produto por keyword
                                                                        │
                                              reserva estoque ◄─────────┘
                                                    │
                              carts + cart_items + stock_reservations (+ waitlist_items)
                                                    │
                            comprador abre /cart/<token> ──► frete ──► PIX/cartão
                                                                        │
                          webhook do gateway ──► reconsulta provider ──► cart paid
                                                                        │
                     postcheckout.OnCartPaid: tracking_token, order_events, e-mail, GMV,
                     waitlist fulfilled ──► ERP (Tiny) ──► envio/rastreio
```

## Backend — domínios e arquivos

| Área | Arquivos |
|---|---|
| Evento/live | `internal/live/` (`handler.go`, `service.go`, `types.go`) |
| Webhook Instagram | `internal/integration/instagram_handler.go`, `instagram_types.go` |
| Parsing de comentário | `internal/integration/instagram_parser.go` (`ParsePurchaseIntent`, `ExtractPossibleKeywords`) |
| Processamento do comentário | `internal/integration/service.go` → `ProcessInstagramComment` (~linha 3952) |
| Expiração lazy | `internal/integration/service.go` → `ProcessExpiredCartsForProduct` (~linha 5555) |
| Checkout público | `internal/checkout/` (`handler.go`, `service.go`, `shipping.go`, `types.go`) |
| Pós-pagamento | `internal/postcheckout/service.go` (`OnCartPaid`, `OnShipmentPosted`, `OnDelivered`) |
| Webhooks de pagamento | `internal/integration/webhook_handler.go` (`HandleMercadoPago`, `HandlePagarme`) |
| Notificações | `internal/notification/service.go`; e-mails em `lib/email/` |
| Auth middleware | `lib/httpx/middleware.go` (`AuthMiddleware` linha ~39, bypass dev linha ~48; `StoreAccessMiddleware` linha ~124) |
| Config/env | `lib/config/config.go` (`Environment()` default `development`) |

## Rotas relevantes

### Admin (`/api/v1/stores/:storeId/...` — Clerk JWT ou bypass dev)

| Método | Rota | Ação |
|---|---|---|
| POST | `/products` | Criar produto (keyword, price em centavos, stock, shipping dims) |
| POST | `/lives` | Criar evento (sem `scheduledAt` → nasce `active` com sessão) |
| POST | `/lives/:id/start` / `/lives/:id/end` | Iniciar / encerrar (end finaliza carts `active`→`checkout`) |
| GET | `/lives/:id/comments` / `/lives/:id/carts` | Moderação / carts do evento |
| GET | `/lives/:id/pulse` | Contadores baratos (a UI faz polling disso) |
| PATCH | `/lives/:id/active-product` | Produto em destaque (fallback de parsing sem keyword) |
| PATCH | `/lives/:id/pause-processing` | Pausa o processamento de comentários |
| POST/GET | `/lives/:id/whitelist` | Produtos do evento (preço especial; obrigatório só em post-commerce) |

Bypass dev: headers `Authorization: Bearer <qualquer>` + `X-Dev-User-ID: <users.clerk_id>`; só quando `ENVIRONMENT != production`. O clerk_id precisa ter membership ativa na loja (`memberships` join `users`).

### Webhooks (públicos)

| Rota | Notas |
|---|---|
| GET/POST `/api/webhooks/instagram` | GET = verificação Meta (`hub.verify_token` = `INSTAGRAM_VERIFY_TOKEN`, default `livecart_verify_token`). POST = comentários. **Assinatura NÃO validada** (`SignatureValid: true` hardcoded) — simulável por curl. |
| POST `/api/webhooks/mercado_pago/:storeId` | Payload só dispara `ProcessPaymentNotification`, que **reconsulta a API do MP** (`GetPaymentStatus`) — não dá para forjar pagamento. |
| POST `/api/webhooks/pagarme/:storeId` | HTTP Basic Auth (credenciais do merchant, se configuradas). Mesma reconsulta ao provider. |

### Checkout público (`/api/public/checkout/:token` — sem auth, token = segredo)

| Método | Rota | Ação |
|---|---|---|
| GET | `/` | Carrinho completo (422 `carrinho expirado` se `status='expired'`) |
| POST | `/shipping-quote` · PUT `/shipping-method` | Cotação (exige integração de frete + CEP origem + dimensões) / seleção |
| GET | `/config` | Provider de pagamento + public key (a UI usa para tokenização) |
| POST | `/pix` · `/card` | Pagamento transparente. `/card` aprovado síncrono → dispara `OnCartPaid` (checkout/service.go:817) |
| GET | `/status` | Lê o **banco** (não o provider) — polling da UI |
| POST/PATCH/DELETE | `/items[...]` | Edição de itens (ajusta reservas) |
| DELETE | `/waitlist/:id` | Sair da fila |

## Tabelas e estados

| Tabela | Campos-chave | Estados |
|---|---|---|
| `live_events` | store_id, type, platform_live_id, cart_expiration_minutes, current_active_product_id, processing_paused, total_orders | `scheduled` → `active` → `ended` |
| `live_sessions` / `live_session_platforms` | event_id; platform_live_id (roteia webhook via `GetSessionByPlatformLiveID`, exige sessão `active`/`live`) | — |
| `products` | keyword (4 chars, `UNIQUE(store_id,keyword)`), price, **stock**, dimensões de frete | — |
| `carts` | event_id, `UNIQUE(event_id, platform_user_id)`, token (32 hex), expires_at, paid_at, tracking_token, short_id | status: `active`→`checkout`/`expired`/`cancelled`; payment_status: `pending`→`paid`/`failed`/`refunded` |
| `cart_items` | cart_id, product_id, quantity, unit_price, **waitlisted_quantity** | — |
| `stock_reservations` | cart_id, product_id, event_id, quantity; `UNIQUE(cart,product,event) WHERE status='active'` | `active` → `reversed` (expirou/cancelou) / `converted` (evento fechou) |
| `waitlist_items` | event_id, product_id, cart_id, position, expires_at | `waiting` → `notified` → `fulfilled` / `expired` / `cancelled` |
| `live_comments` | comment_id (idempotência), text, result | result: `added_to_cart`, `waitlisted`, `no_intent`, `no_product`, `blocked`, `paused`... |
| `payments` | cart_id, provider, external_payment_id, idempotency_key | `pending`→`approved`/`rejected`/`refunded` |
| `order_events` | cart_id, event_type, `UNIQUE(cart_id, event_type)` | `payment_confirmed`, `shipped`, `delivered` |
| `webhook_events` | auditoria de todo webhook recebido | — |
| `notification_logs` | canal (instagram_dm/whatsapp/email), status, cooldown | — |

## Mecânicas importantes

- **Parsing** (`ParsePurchaseIntent`): negação ("não quero", "cancela") e pergunta ("quanto custa", "tem estoque") retornam nil; keyword = token de exatamente 4 alfanuméricos (case-insensitive, match em `products.keyword` da loja inteira); quantidade via `2x`/`x2`/"quero N"/"manda N"/"N unidades"; "quero" solto → qty 1 com fallback no produto em destaque do evento.
- **Estoque**: `products.stock` é decrementado na criação do cart e a reserva registrada em `stock_reservations`. Sem estoque → `waitlist_items` (parcial: reserva o disponível + fila o resto).
- **Expiração**: sem cron. `expires_at` definido na criação (evento ou default da loja). Materialização **lazy** em `ProcessExpiredCartsForProduct`, chamada no processamento de novo comentário do mesmo produto: marca `status='expired'`, devolve estoque, reverte reservas (e no ERP, se houver), promove waitlist (`waiting`→`notified` + `ExtendCartExpiration`). Consequência testável: cart vencido por tempo ainda responde 200 até a varredura rodar.
- **Pagamento**: confirmação SÓ via provider. Webhook → `ProcessPaymentNotification` → `GetPaymentStatus` na API do provider → `UpdateCartPaymentStatus` → hooks (`OnCartPaid` no paid; `OnCartCancelled`/`OnCartRefunded` nos negativos) → ERP. Caminho síncrono: `/card` aprovado chama `OnCartPaid` direto.
- **Fim do evento**: `End` marca `ended`, `FinalizeCartsByEvent` move carts `active`→`checkout`, envia links se `send_on_live_end`.
- **Workers** (nenhum expira carts): TokenRefresh (5min), TrackingPoller (6h), coupon RedemptionExpirer (5min), PostCommentPoller (pós-commerce).

## Frontend — rotas e componentes

FE em `/home/carluz_teles/livecart-fe` (Next.js App Router, porta 3000, `NEXT_PUBLIC_API_URL` → API).

### Painel (autenticado via Clerk)

| Rota | O quê | Arquivos |
|---|---|---|
| `/login` | `<SignIn/>` do Clerk | `src/app/(auth)/login/[[...sign-in]]/page.tsx` |
| `/dashboard` | Home pós-login | `src/app/(dashboard)/dashboard/page.tsx` |
| `/dashboard/products` | Listagem + sheet "Novo Produto" | `src/components/product/ProductForm/index.tsx` |
| `/dashboard/events` (alias `/dashboard/lives`) | Listagem + criação de evento | `src/components/event/EventForm.tsx` |
| `/dashboard/events/[id]` | **Página do evento**: feed de Comentários, tabela de Pedidos, Checkouts Ativos, KPIs, Funil, Sessões, Live Mode (produto em destaque + pausa), abas Produtos/Upsells/Cupons | `src/components/event/EventDetail/*.tsx` |

- A página do evento atualiza por **polling do `/pulse`** (`useEventPulse`) — mudanças aparecem em ~5–10s; recarregar acelera.
- Criação de live Instagram na UI usa dropdown "Live Ativa" (lives reais da integração) — **media id simulado não é digitável**; criar evento simulado via API.
- Login para testes: infra pronta com `@clerk/testing` (`e2e/global.setup.ts`, `e2e/auth.setup.ts`, `playwright.config.ts`; envs `E2E_CLERK_USER_USERNAME`/`E2E_CLERK_USER_PASSWORD` no `.env.local`).

### Checkout público (comprador)

| Rota | O quê |
|---|---|
| `/cart/[token]` | Wizard 4 seções (dados → endereço → frete → pagamento). Renderiza `CheckoutPaidScreen` quando pago e `CheckoutExpiredScreen` quando `status='expired'` |
| `/order/[shortId]?key=...` | Tracking pós-compra |

Arquivos: `src/app/cart/[token]/CheckoutClient.tsx`, `src/components/checkout/` (`CheckoutCardForm.tsx`, `CheckoutPixDisplay.tsx`, `CheckoutExpressPayment.tsx`, `CheckoutShippingOptions.tsx`).

Comportamentos que afetam a automação: CEP dispara ViaCEP com debounce 400ms (cidade/UF ficam disabled ao autopreencher); seção N só habilita com a N-1 completa; frete é **obrigatório**; status de pagamento tem polling de 5s; tokenização de cartão — Mercado Pago usa Secure Fields (iframes) e Pagar.me usa inputs nativos (`#pm-card-number` etc.).

## Tabela de parsing (para asserções)

| Comentário | Resultado |
|---|---|
| `9432` | cart, qty 1 |
| `9432 2x` / `2x 9432` | cart, qty 2 |
| `quero 3 9432` / `manda 3 9432` | cart, qty 3 |
| `quero` (sem keyword) | cart qty 1 **somente** se o evento tem produto em destaque |
| `não quero` / `cancela` | sem cart (`no_intent`) |
| `quanto custa` / `tem estoque` | sem cart (`no_intent` — pergunta) |
| keyword inexistente com verbo de compra | `no_product` |
| mesmo `comment_id` repetido | ignorado (idempotência) |
