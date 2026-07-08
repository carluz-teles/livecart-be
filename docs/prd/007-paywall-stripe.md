# PRD 007 - Paywall e Assinaturas via Stripe

**Status:** 🟢 Em implementacao
**Prioridade:** P0
**Estimativa:** 4 sprints

---

## 1. Visao Geral

### Problema
O produto nao cobra. Nao existe trial, paywall, nem estrutura de assinatura —
qualquer conta criada tem acesso ilimitado para sempre.

### Solucao
Assinaturas via Stripe com trial de 7 dias sem cartao, alinhadas a
precificacao publica da LP: mensalidade fixa + % sobre pedidos pagos (GMV),
todos os recursos em todos os planos (sem feature walls).

### Resultado Esperado
- Trial automatico no onboarding (zero fricao, sem cartao)
- Conversao via Stripe Checkout na escolha do plano
- Corte de acesso automatico e reversivel para inadimplentes
- Fatura mensal com mensalidade + taxa sobre GMV do periodo

---

## 2. Decisoes de Produto (tomadas em jul/2026)

| Decisao | Escolha |
|---------|---------|
| Cartao no trial | ❌ Sem cartao (honra a LP "Sem cartao de credito"); trial nasce automatico no onboarding |
| Escopo do corte | Painel + criacao de novos carrinhos. Checkouts ja emitidos e webhooks de pagamento CONTINUAM (protege consumidor final e dinheiro do lojista) |
| Escolha do plano | Na conversao (trial internamente ancorado no Grow; usuario decide plano ao adicionar cartao) |
| Taxa sobre GMV | Billing Meter nativo da Stripe (meter event enviado no webhook de pagamento, identifier = cart_id) |
| Upgrade/downgrade | Upgrade imediato com proration da mensalidade; downgrade agendado para o proximo ciclo |
| Enterprise | Subscription manual no dashboard Stripe + manual_override no banco |

## 3. Planos (da LP)

| Plano | Mensalidade | % sobre pedidos pagos |
|-------|-------------|----------------------|
| Start | R$ 147 | 1,8% |
| Grow | R$ 297 | 1,3% |
| Scale | R$ 697 | 1,0% |
| Enterprise | negociado | negociado |

Estrutura Stripe: 1 subscription com 2 items por plano — price fixo mensal
(BRL) + price metered (Billing Meter `gmv_cents`). Price IDs por env:
`STRIPE_PRICE_{START|GROW|SCALE}_{FLAT|METERED}`.

⚠️ Constraint da Stripe (verificada ao vivo): subscription com
`trial_settings.end_behavior.missing_payment_method=pause` NAO pode conter
item metered. Portanto o TRIAL carrega apenas o item flat; o item metered de
GMV e adicionado na conversao (quando o cartao chega). GMV durante o trial e
gratis de qualquer forma.

---

## 4. Maquina de Estados

```
trialing --cartao+fim trial--> active --falha cobranca--> past_due --retries--> unpaid/canceled
    |                                                        | (grace 7d, banner)     |
    +--sem cartao (pause)----> PAYWALL <---------------------+------------------------+
```

- Fonte da verdade: webhooks Stripe atualizam a tabela local. Acesso decidido
  lendo o NOSSO banco, nunca a API da Stripe em request-time.
- Trial sem cartao: `trial_settings.end_behavior.missing_payment_method: pause`.
- Grace period de past_due: 7 dias com acesso + banner, depois bloqueia.

## 5. Enforcement (retirada de acesso)

1. **BE (dura)**: middleware nas rotas store-scoped → status bloqueado = 402.
   Allowlist: endpoints de billing, `/users/sync`, webhooks publicos.
2. **FE (UX)**: `/users/sync` ganha `subscriptionState`; middleware do FE
   redireciona para o paywall quando bloqueado.
3. **Workers**: processamento de comentarios (novos carrinhos) e recovery
   worker pausam para lojas bloqueadas.
4. **Nao corta**: checkout publico de carrinhos ja emitidos, webhooks de
   pagamento, tracking de pedidos.

## 6. Estrutura de Dados

Remodela a tabela `subscriptions` vestigial (init migration, nunca usada):

```sql
subscriptions:
  store_id (unique), stripe_customer_id, stripe_subscription_id,
  plan ('start'|'grow'|'scale'|'enterprise'), status,
  trial_ends_at, current_period_end, cancel_at_period_end,
  grace_until, manual_override
```

Novo dominio `internal/billing/` (handler/service/repository/types, padrao da
casa). Config: `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, price IDs.

## 7. Fluxos

### Onboarding
CreateStore → cria Stripe Customer + Subscription trialing (7d, price Grow,
sem payment method) → usuario cai no dashboard com banner "X dias restantes".

### Conversao
Banner/paywall → `/settings/billing` → escolhe plano → Stripe Checkout
(mode=subscription update / payment method) → webhook ativa → acesso segue.

### Cobranca da taxa GMV
Webhook de pagamento (cart paid) → meter event `gmv_cents` (identifier =
cart_id, idempotente) → Stripe soma no ciclo → linha na fatura.

### Telas
- `/settings/billing`: plano atual, status, trial, uso do periodo (GMV x taxa),
  cards de troca de plano, Customer Portal (cartao/faturas/cancelamento)
- Paywall full-screen (sem sidebar) para paused/unpaid
- Banner de trial no layout do dashboard

## 8. Webhooks Stripe

`POST /api/webhooks/stripe` (assinatura via `STRIPE_WEBHOOK_SECRET`):
- `checkout.session.completed` — cartao adicionado / plano escolhido
- `customer.subscription.created|updated|deleted` — status/plano/periodo
- `invoice.paid`, `invoice.payment_failed` — ciclo de cobranca e grace

---

## 9. Implementacao

| Sprint | Entrega |
|--------|---------|
| 1 | Migration + internal/billing + customer/trial no onboarding + webhook Stripe + subscriptionState no /users/sync |
| 2 | Enforcement (402 + pausas de worker) + paywall screen + banner de trial |
| 3 | Tela de billing + Checkout + Customer Portal + mudanca de plano |
| 4 | Meter de GMV no webhook de pagamento + uso do periodo + e-mails de trial (D-2, D0) |

## 10. Dependencias Externas

- [ ] Conta Stripe (modo test) — `STRIPE_SECRET_KEY` (sk_test_...) no .env
- [ ] Webhook secret (`whsec_...`) apos criar o endpoint
- [ ] Products/Prices criados (posso criar via API com a secret key)
- [ ] Ativacao do faturamento em BRL na conta
