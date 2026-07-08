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

---

## 11. Status da Implementacao (jul/2026)

Sprints 1-4 implementadas. Commits BE: 8fef93f (S1), 8cefa6f (S2, inclui
conversao antecipada), a7acfed (S3), S4 no commit seguinte. FE: f827a10 (S2),
25abac9 (S3).

O que esta no ar:
- Trial 7d sem cartao no CreateStore + lazy no /users/sync (cobre legado)
- Enforcement: 402 no BE (fail-open), redirect /paywall no FE, workers pausam
- Conversao: paywall/billing -> Checkout (setup) -> webhook ativa plano
  escolhido (flat + metered) e encerra o trial
- Portal do cliente, upgrade imediato com proracao, downgrade via schedule
- GMV metered: OnCartPaid (ponto unico de "pago") envia meter event
  idempotente (identifier = gmv-<cart_id>; guard de tracking token evita
  duplicata local)

Follow-ups conhecidos:
- Exibir uso do periodo (GMV acumulado x taxa) na tela de billing
- E-mails de trial D-2/D0 (infra de email existe; falta scheduler)
- Configurar STRIPE_WEBHOOK_SECRET (dashboard -> Webhooks -> endpoint
  /api/webhooks/stripe; local: stripe listen --forward-to)
- Reconciliacao periodica local<->Stripe (hoje so webhooks)
- Enterprise: fluxo manual documentado, sem UI dedicada


---

## 12. Fase 2 — Cobranca manual (decisao jul/2026)

Alguns lojistas preferem nao deixar a cobranca no cartao (limite, fluxo de
caixa, contabilidade). Mapeamento direto no `collection_method` da Stripe:

- **automatic** (default): `charge_automatically` — cartao salvo, debita na
  virada. Comportamento atual.
- **manual** (APENAS mediante solicitacao, flag interna sem toggle
  self-service): `send_invoice` + `days_until_due=7` — a Stripe envia a
  fatura hospedada (boleto/PIX/cartao) por e-mail e o lojista paga ativamente.
  Fatura unica (mensalidade + taxa GMV juntas). Vencida -> past_due ->
  grace 7d -> paywall (mesma maquina de estados).

Implementacao (~1-2 dias quando solicitado): coluna `collection_mode` na
tabela subscriptions, aplicacao do collection_method na subscription Stripe,
branch da conversao sem Checkout no modo manual. Pre-requisito: ativar
boleto/PIX como payment methods na conta Stripe.


---

## 13. Ledger Financeiro de GMV (append-only) — decisao jul/2026

Tabela `billing_ledger_entries` (migration 000082), append-only: cada pedido
pago gera uma entrada `sale` (+valor, +taxa, snapshot de plano/bps/billable);
estorno gera `refund_credit` (−valor, −taxa) — nada e editado, trilha de
auditoria completa.

Papeis do ledger:
1. Billing: transparencia da taxa + estorno devolvido na taxa DA VENDA (mesmo
   em ciclo posterior, via credito de Customer Balance que abate a proxima
   fatura)
2. Lojista: pagina FINANCEIRO (menu principal) — hero com vendas do ciclo,
   "gerado pelo LiveCart" (ROI/receita recuperada) e composicao da proxima
   fatura + extrato linha a linha
3. Plataforma: GMV/take-rate por loja direto do ledger
4. Reconciliacao: stripe_ref por entrada; linha sem ref = pendencia visivel

Framing de produto (anti-"prejuizo"): vendas como numero-heroi, "taxa de
sucesso" como vocabulario, ROI justaposto, estorno celebrado ("taxa devolvida
automaticamente"), composicao da fatura antes de cobrar. Paginas separadas:
Settings > Plano e cobranca (config da assinatura) vs Financeiro (dinheiro do
lojista); fusao com o Dashboard avaliada depois.

Regras: venda em trial = billable=false (estorno nao credita, taxa nunca foi
cobrada); teto de 1 estorno por carrinho (parcial e follow-up); meter Stripe
permanece bruto — o acerto financeiro e via credito.
