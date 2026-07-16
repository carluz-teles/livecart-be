---
name: billing-e2e
description: Testa a assinatura da plataforma e o paywall de ponta a ponta pela UI real com Playwright (local ou staging, Stripe test mode) — cria uma conta nova via Clerk, passa pelo onboarding da loja, confere o trial de 7 dias (banner, tela de billing, banco e Stripe), força a expiração do trial, valida o bloqueio do paywall (402 na API + redirect /paywall no FE), paga o plano no Stripe Checkout com cartão de teste e confirma a ativação da assinatura e o desbloqueio. Usar quando pedirem para testar/validar assinatura, trial, paywall, billing, conversão de plano, checkout da Stripe, ou o pipeline webhook Stripe→subscriptions.
---

# Billing E2E — assinatura, trial e paywall pela UI com Playwright

Playbook para dirigir o ciclo de vida completo da assinatura: conta nova via Clerk → loja criada no onboarding (trial de 7 dias sem cartão) → trial conferido em UI/banco/Stripe → expiração forçada → paywall bloqueando API e FE → conversão paga no Stripe Checkout (test mode) → assinatura ativa e acesso liberado.

Usar as tools do Playwright MCP (`browser_navigate`, `browser_snapshot`, `browser_fill_form`, `browser_click`, `browser_wait_for`, `browser_take_screenshot`) para tudo que é UI. O que não tem UI (estado na Stripe, expiração forçada) vai via `scripts/stripe-test.sh`; o estado local confere-se via SQL.

Arquivos de apoio (ler conforme a fase exigir):
- `references/billing-map.md` — mapa técnico BE + FE: tabela, estados, regra de bloqueio, endpoints, fluxos, rotas/seletores do FE, cartões de teste e webhooks por ambiente.
- `references/db-checks.md` — queries SQL de verificação por fase + cleanup.
- `scripts/stripe-test.sh` — helper da API Stripe **test mode** (`sub`, `expire-trial`, `invoices`, `events`, `cancel`, `del-customer`); recusa chave que não seja `sk_test_`.

## Regras invioláveis

1. **Nunca rodar contra produção — e produção agora é Stripe LIVE.** Alvos válidos: `local` e `staging`. Pré-flight obrigatório: a `STRIPE_SECRET_KEY` do alvo **tem que começar com `sk_test_`**; qualquer `sk_live_`, URL `api.livecart.com.br`/`app.livecart.com.br` ou dúvida sobre o alvo: **parar imediatamente**.
2. **Backend local sempre via `docker compose up`**; frontend local sempre via `npm run dev` (CLAUDE.md — nunca `go run`). Staging já está no ar (Railway); não fazer deploy como parte desta skill.
3. **Webhook precisa chegar no alvo** para o fluxo real: local exige o tunnel (`docker compose --profile dev up -d tunnel`); staging recebe direto. Sem webhook funcionando, só a Rota B da Fase 3 (fallback local) é viável — e a conversão da Fase 4 **não completa**.
4. **Escopo de dados**: conta/loja novas por rodada, nome da loja prefixado `[E2E]`, e-mail `billing-e2e+<timestamp>+clerk_test@teste.com`. Cleanup ao final (Fase 7) só com confirmação; não tocar em dados que a rodada não criou. Staging é compartilhado — cuidado redobrado.
5. **Cartão só de teste** (`4242 4242 4242 4242` etc. — `billing-map.md § dados de teste`). Se o Checkout abrir com cara de live mode (sem badge "TEST"), parar.
6. **Não "consertar" estado pelo banco no meio do fluxo real** — UPDATEs diretos só onde o playbook prevê (Rota B) e sempre declarados no relatório: mascarar falha de webhook invalida o teste.

## Fase 0 — Ambiente, alvo e guardas

Parâmetros usados em todas as fases: `$API_BASE`, `$FE_BASE`, `$PSQL`.

**Local:**
```bash
docker compose up -d && docker compose --profile dev up -d tunnel   # API + webhook tunnel
(cd /home/carluz_teles/livecart-fe && npm run dev &)                # FE
API_BASE="http://localhost:3001"; FE_BASE="http://localhost:3000"
PSQL() { docker compose exec -T postgres psql -U livecart -d livecart -tA -c "$1"; }
curl -s $API_BASE/health   # aguardar ok
```
⚠️ Conferir o `NEXT_PUBLIC_API_URL` do FE antes de subir — deve apontar pro alvo **com o sufixo `/api/v1`** (regra herdada da live-flow-e2e; sem o sufixo todo request vira 404).

**Staging (Railway):** obter URLs e `DATABASE_URL` via `railway` CLI (`railway variables --environment staging --service livecart-be --kv`; se o shim quebrar, usar o binário direto em `~/.asdf/installs/nodejs/*/bin/railway`). Confirmar com o usuário que são de staging antes de qualquer escrita.

**Guardas obrigatórias (parar se falhar):**
```bash
# 1. Stripe do alvo é TEST mode?
#    local:   grep '^STRIPE_SECRET_KEY=' .env | cut -c1-25       → sk_test_
#    staging: railway variables ... | grep STRIPE_SECRET_KEY     → sk_test_
# 2. Paywall ligado? PAYWALL_ENABLED=true (senão nada bloqueia — enforced=false)
# 3. Webhook chegando? scripts/stripe-test.sh events  → eventos recentes; e o
#    STRIPE_WEBHOOK_SECRET do alvo é o do endpoint certo (billing-map.md § webhooks)
```

**Clerk**: o cadastro usa e-mail `*+clerk_test@...` com código `424242` — só funciona em **instância dev** do Clerk. Conferir a publishable key do FE do alvo (`pk_test_`); se for instância de produção do Clerk, combinar com o usuário como criar a conta.

## Fase 1 — Conta nova + onboarding (UI)

1. `browser_navigate` → `$FE_BASE/register` (Clerk `<SignUp/>`, heading "Crie sua conta", box "✨ 7 dias grátis, sem cartão").
2. Cadastrar com `billing-e2e+<timestamp>+clerk_test@teste.com` + senha forte; código de verificação `424242`.
3. Após o cadastro o middleware detecta `state === "no_store"` e manda para `/onboarding` ("Vamos montar sua loja 🚀"). Preencher o wizard de 4 passos ("Sobre você" → "Sua loja" → "Endereço da loja" → "Contato da loja") com loja `[E2E] Billing <timestamp>` e slug único `e2e-billing-<timestamp>`.
4. Finalizar → toast **"Loja criada! Seus 7 dias grátis começaram. 🎉"** → `/dashboard`. Screenshot.
5. Verificar banco (`db-checks.md § Fase 1`) e **anotar `STORE_ID`, `CLERK_ID`, `EMAIL_E2E`**.

Se o Clerk bloquear com captcha: tentar o harness `@clerk/testing` do FE (`e2e/` no repo FE); em último caso, pedir ao usuário para criar a conta manualmente e seguir da Fase 2 (a skill continua válida — o trial nasce na criação da loja).

## Fase 2 — Conferir o trial (a data que o usuário pediu para checar)

1. **UI**: no dashboard, o TrialBanner mostra **"7 dias de teste grátis restantes — termina em [DATA]"**. Em `/settings/billing`: badge **"Período de teste"**, plano "Teste grátis", texto "Seu teste grátis termina em [DATA] (7 dias)". Screenshot.
2. **API**: `GET /api/v1/stores/$STORE_ID/billing/subscription` (bypass dev) → `status=trialing`, `plan=grow`, `trialDaysLeft=7`, `blocked=false`, `enforced=true`.
3. **Banco** (`db-checks.md § Fase 2`): linha `trialing/grow`, `trial_ends_at ≈ now()+7d`, refs Stripe preenchidas. **Anotar `SUB_ID` e `CUS_ID`.**
4. **Stripe**: `scripts/stripe-test.sh sub $SUB_ID` → `status=trialing`, `trial_end` batendo com o banco (±1min), **1 item só** (grow-flat — o metered entra na conversão).

As três fontes (UI, banco, Stripe) devem contar a mesma história — divergência aqui é achado de bug, não ruído.

## Fase 3 — Forçar a expiração do trial e validar o paywall

**Rota A (padrão — fluxo real via Stripe + webhook):**
1. `scripts/stripe-test.sh expire-trial $SUB_ID` (`trial_end=now`; sem cartão o `end_behavior` pausa) → resposta `-> paused`.
2. Aguardar o webhook `customer.subscription.updated` (~5–30s) e conferir no banco: `status='paused'` (`db-checks.md § Fase 3`). Se não virar: debugar o webhook (`events`, logs da API) — **não** mascarar com UPDATE; se o webhook estiver quebrado no alvo, isso é um achado. Só então cair para a Rota B para seguir o roteiro.

**Rota B (fallback local, sem Stripe):** `UPDATE subscriptions SET trial_ends_at = now() - interval '1 minute' ...` — bloqueia pelo safety net (`status` segue `trialing`). Declarar no relatório que a Stripe ficou divergente e o pipeline de webhook não foi exercitado.

**Validar o bloqueio (ambas as rotas):**
- **API**: rota store-scoped qualquer (ex. `GET /api/v1/stores/$STORE_ID/products`) → **402** `{"error":"subscription_required"}`; e `GET .../billing/subscription` → **200** com `blocked=true` (o allowlist de `/billing` existe pro lojista conseguir pagar).
- **FE**: navegar para `$FE_BASE/dashboard` → middleware redireciona para **`/paywall`** — heading **"Seu período de teste terminou 👋"**, cards Start/Grow/Scale. Screenshot.
- **Opcional (profundidade)**: com um evento ativo de teste na loja, um comentário simulado vira `live_comments.result='blocked'` sem criar cart (roteiro de comentário na skill `live-flow-e2e`).

## Fase 4 — Pagar o plano (Stripe Checkout, test mode)

1. Na tela `/paywall` (ou `/settings/billing`), clicar **"Assinar Start"** (Start = fatura de R$147 — mais barato de conferir; qualquer plano self-service vale).
2. O FE chama `POST /billing/checkout` e redireciona para `checkout.stripe.com` (**mode=setup** — só coleta o cartão). Conferir o badge de **test mode** na página da Stripe.
3. Preencher: cartão `4242 4242 4242 4242`, validade `12/34`, CVC `123`, nome qualquer → submeter. (Se pedir 3DS, usar o cartão 3DS do `billing-map.md` e aprovar no modal.)
4. Retorno em `/settings/billing?billing=success` → toast **"Pagamento configurado! Sua assinatura está sendo ativada."** Screenshot.
5. A ativação é assíncrona (webhook `checkout.session.completed` → troca de items + `trial_end=now` → fatura flat imediata). Aguardar/recarregar até a UI virar (~5–30s).

**Verificações:**
- **Banco** (`db-checks.md § Fase 4`): `status='active'`, `plan='start'`, período corrente preenchido.
- **Stripe**: `sub $SUB_ID` → `status=active`, **2 items** (start-flat + start-gmv), `default_payment_method` setado; `invoices $CUS_ID` → fatura `paid` de `14700` (`billing_reason=subscription_update`).
- **UI**: `/settings/billing` com badge **"Ativa"**, "Próxima cobrança em [DATA]", botão **"Gerenciar pagamento e faturas"** visível, botão do plano atual como "Plano atual"/upgrade. TrialBanner sumiu. Screenshot.

## Fase 5 — Paywall desbloqueado

- **API**: a mesma rota que respondeu 402 na Fase 3 agora responde **200**.
- **FE**: navegar para `/paywall` → middleware redireciona de volta para `/dashboard` (status `active` + não bloqueado); navegação normal por produtos/eventos/configurações.
- `GET /billing/subscription` → `blocked=false`, `hasPaymentMethod=true`.

## Fase 6 — Extras (opcionais, se o usuário pedir profundidade)

- **Portal**: em `/settings/billing`, **"Gerenciar pagamento e faturas"** → URL `billing.stripe.com` abre com cartão e fatura listados. (Exige a config default do Customer Portal salva em test mode no dashboard da Stripe.)
- **Change plan**: botão de upgrade (ex. Start→Grow) → aplica na hora (banco muda `plan`); downgrade → "Mudar no próximo ciclo" (banco só muda `cancel_at_period_end`/fase futura). `db-checks.md § Fase 6`.
- **GMV billable**: rodar uma venda paga (skill `live-flow-e2e`, Fases 2–6) nesta loja já ativa → `billing_ledger_entries` com `billable=true` e `fee_bps=180`, meter event na Stripe (`stripe_ref='meter:gmv-<cart_id>'`).

## Fase 7 — Relatório e cleanup

**Relatório final**, por fase: o que foi executado, o que a **UI mostrou** (screenshots: onboarding, banner de trial, /paywall bloqueado, Checkout test mode, badge "Ativa"), o que **banco e Stripe** confirmaram (status/plan/datas/fatura), qual rota da Fase 3 foi usada e o que ficou fora (ex.: Rota B não exercita webhook; grace/past_due não coberto — exigiria falha de cobrança real).

**Cleanup (só com confirmação do usuário)** — roteiro completo em `db-checks.md § Cleanup`: cancelar a subscription e apagar o customer na Stripe (test mode), apagar ledger/subscription/membership/store/user no banco; usuário do Clerk fica (apagar no dashboard do Clerk se quiserem). Em staging, propor o cleanup por padrão.
