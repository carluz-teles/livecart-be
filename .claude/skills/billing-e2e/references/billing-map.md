# Mapa técnico — billing, trial e paywall (PRD 007)

Verificado contra `apps/api/internal/billing/*` (BE) e `src/app/paywall`, `src/app/(dashboard)/settings/billing`, `src/middleware.ts` (FE).

## Modelo de dados (tabela `subscriptions`, migration 000080)

Uma linha por loja (`UNIQUE(store_id)`). Colunas relevantes: `status`, `plan`,
`stripe_customer_id`, `stripe_subscription_id`, `trial_ends_at`,
`current_period_start/end`, `cancel_at_period_end`, `grace_until`,
`manual_override`.

- `status`: `trialing | active | past_due | paused | unpaid | canceled`
- `plan`: `start | grow | scale | enterprise`
- **A tabela local é a fonte de verdade do acesso**; webhooks da Stripe a mantêm em sincronia.

## Regra de bloqueio (`blocked()` em `billing/types.go`)

Com `PAYWALL_ENABLED=true` (`enforced`):

| status | bloqueado quando |
|---|---|
| `paused`, `unpaid`, `canceled` | sempre |
| `past_due` | só após `grace_until` (7 dias de graça) |
| `trialing` | `now() > trial_ends_at` (safety net p/ webhook atrasado) |
| `active` | nunca |
| qualquer | nunca, se `manual_override = true` |

`AccessGuard` (middleware BE) responde **402** `{"error":"subscription_required", "subscription":{...}}` em toda rota store-scoped, **exceto** paths contendo `/billing` (o lojista precisa pagar). Fail-open em erro de lookup. `IsStoreBlocked` aplica a mesma regra no pipeline de comentários (comentário em loja bloqueada → `live_comments.result='blocked'`, sem cart).

## Planos (registry `Plans()` — price IDs vêm das envs `STRIPE_PRICE_*`)

| plan | flat | taxa GMV | bps |
|---|---|---|---|
| start | R$ 147,00 (14700) | 1,8% | 180 |
| grow | R$ 297,00 (29700) | 1,3% | 130 |
| scale | R$ 697,00 (69700) | 1,0% | 100 |
| enterprise | dashboard-managed (não self-service) | — | — |

## Ciclo de vida

1. **Trial (criação da loja)** — `POST /api/v1/stores` → `EnsureTrialSubscription`: linha local `trialing`, `plan='grow'`, `trial_ends_at = now()+7d` + espelho na Stripe (customer + subscription trialing só com o price **grow-flat** e `trial_settings[end_behavior][missing_payment_method]=pause`). Falha na Stripe é não-fatal: `/users/sync` reprovisiona lazy (linha local existe mesmo sem refs Stripe).
2. **Expiração natural** — trial termina sem cartão → Stripe muda a subscription para `paused` → webhook `customer.subscription.updated` → linha local `paused` → bloqueado. (O safety net do `blocked()` bloqueia antes mesmo do webhook, se `trial_ends_at` já passou.)
3. **Conversão (pagamento)** — `POST /stores/:id/billing/checkout {plan}` → Stripe Checkout hospedado em **mode=setup** (só coleta o cartão; metadata carrega `store_id`, `plan`, `subscription_id`) → webhook `checkout.session.completed` (mode=setup) → `completeConversion`: pega o payment method do setup intent, `ActivateSubscription` (seta `default_payment_method`, `trial_end=now`, troca o item flat pro plano escolhido e **adiciona o item metered**) → primeira fatura flat cobra na hora → `applySubscription` grava local `active` (o `customer.subscription.updated` que chega em seguida é idempotente).
4. **Grace** — fatura falha → Stripe `past_due` → webhook grava `grace_until = now()+7d`; acesso segue durante a graça.
5. **Change plan** — `POST /billing/change-plan {plan}`: upgrade imediato prorateado; downgrade agendado pro fim do período. Exige status `active`/`past_due`.
6. **GMV** — cart pago → `ReportPaidGMV`: linha em `billing_ledger` (fee pelo bps do plano; `billable` só se active/past_due) + meter event `gmv_cents` (identifier `gmv-<cart_id>`).

## Endpoints BE

Store-scoped (auth; bypass dev: `-H "Authorization: Bearer dev" -H "X-Dev-User-ID: <clerk_id>"`, só com `ENVIRONMENT != production`):

- `GET  /api/v1/stores/:storeId/billing/subscription` → `SubscriptionState`
- `POST /api/v1/stores/:storeId/billing/checkout` `{"plan":"start|grow|scale"}` → `{url}` (Checkout hospedado)
- `POST /api/v1/stores/:storeId/billing/portal` → `{url}` (Customer Portal)
- `POST /api/v1/stores/:storeId/billing/change-plan` `{"plan":...}` → `SubscriptionState`
- `GET  /api/v1/stores/:storeId/billing/usage` · `GET .../billing/statement?page=&limit=`
- `POST /api/v1/stores` `{"name","slug"}` — cria loja + trial (1 loja por usuário)
- `POST /api/v1/users/sync` — retorna `{state, subscription, ...}` (o FE middleware lê daqui)
- `POST /api/webhooks/stripe` — público, assinatura HMAC verificada (`STRIPE_WEBHOOK_SECRET`); 500 → Stripe faz retry

`SubscriptionState` (JSON): `status`, `plan`, `trialEndsAt`, `trialDaysLeft`, `currentPeriodEnd`, `cancelAtPeriodEnd`, `graceUntil`, `hasPaymentMethod` (true p/ active/past_due), `blocked`, `enforced`.

## Rotas e comportamento do FE

- `/register` — Clerk `<SignUp/>`; heading "Crie sua conta"; box "✨ 7 dias grátis, sem cartão".
- `/onboarding` — wizard 4 passos ("Sobre você" → "Sua loja" → "Endereço da loja" → "Contato da loja"); ao final chama `POST /stores`, `POST /users/me/select-store`, `PUT /stores/me`; toast **"Loja criada! Seus 7 dias grátis começaram. 🎉"** → `/dashboard`. Middleware manda pra cá quando `state === "no_store"`.
- **Middleware paywall** (`src/middleware.ts:68-75`): `subscription.blocked === true` → redirect `/paywall`; em `/paywall` com `status === "active"` e não bloqueado → redirect `/dashboard`.
- `/paywall` — heading **"Seu período de teste terminou 👋"** (trialing/paused) ou "Escolha um plano pra continuar"; cards Start/Grow/Scale com botões **"Assinar Start|Grow|Scale"**; botão "Sair da conta".
- `/settings/billing` — seção **"Assinatura"** (badge de status: "Período de teste", "Ativa", "Pagamento pendente", "Pausada", "Inadimplente", "Cancelada"; trial: "Seu teste grátis termina em [DATA] ([X dias])"; ativa: "Próxima cobrança em [DATA]"; botão **"Gerenciar pagamento e faturas"** quando `hasPaymentMethod`) + seção **"Planos"** (botões "Assinar [Plano]" / "Plano atual" / "Fazer upgrade" / "Mudar no próximo ciclo"). Retorno da Stripe: `?billing=success` → toast "Pagamento configurado! Sua assinatura está sendo ativada."; `?billing=cancelled` → toast de cancelamento.
- **TrialBanner** (topo do dashboard): >2 dias → "[X] dias de teste grátis restantes — termina em [DATA]" (link "Escolher plano"); ≤2 dias → âmbar "Seu teste grátis está terminando..." (link "Assinar agora"); past_due → vermelho "Não conseguimos cobrar seu cartão..." (link "Resolver agora"). `enforced=false` esconde tudo.

## Stripe test mode — dados de teste

- **Cartão OK**: `4242 4242 4242 4242`, validade futura (ex. `12/34`), CVC `123`, qualquer nome.
- **Cartão 3DS** (se o Checkout pedir autenticação): `4000 0027 6000 3184` → aprovar no modal.
- **Cartão recusado** (teste negativo opcional): `4000 0000 0000 0002`.
- E-mail Clerk dev: `qualquer+clerk_test@exemplo.com` com código de verificação `424242` (só em instância dev do Clerk — conferir no alvo).
- Helper: `scripts/stripe-test.sh` (`sub`, `expire-trial`, `invoices`, `events`, `cancel`, `del-customer`) — recusa chave não-`sk_test_`.

## Webhooks por ambiente (test mode)

| alvo | como chega | endpoint Stripe |
|---|---|---|
| local | tunnel `docker compose --profile dev up -d tunnel` → `https://livecart-api.loca.lt` | `we_1TqjqbDglAyChO94Og7gtYWm` |
| staging | direto (Railway) | `we_1Tqul2DglAyChO94xU0xDIYO` |

O `STRIPE_WEBHOOK_SECRET` do alvo deve ser o do endpoint correspondente — assinatura inválida = 401 no log. Debug: `scripts/stripe-test.sh events customer.subscription.updated` e logs da API (`docker compose logs -f api` / `railway logs`).

⚠️ **Produção agora roda Stripe LIVE** (desde 2026-07-16). Qualquer sinal de `sk_live_`/URL de produção durante a rodada: parar imediatamente.
