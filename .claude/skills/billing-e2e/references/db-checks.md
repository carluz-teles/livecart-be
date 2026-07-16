# Verificações SQL por fase — billing-e2e

Rodar via `$PSQL` (Fase 0). Substituir `<...>`. Não avançar de fase com estado inconsistente.

## § Fase 1 — conta e loja criadas

```sql
-- Usuário sincronizado (Clerk → users) e membership owner
SELECT u.id, u.clerk_id, u.email, m.store_id, m.role, m.status, s.name, s.slug
FROM users u
JOIN memberships m ON m.user_id = u.id
JOIN stores s ON s.id = m.store_id
WHERE u.email = '<EMAIL_E2E>';
-- role='owner', status='active'. Anotar STORE_ID e CLERK_ID.
```

## § Fase 2 — trial provisionado

```sql
SELECT status, plan, trial_ends_at, stripe_customer_id, stripe_subscription_id,
       current_period_start, current_period_end, manual_override
FROM subscriptions WHERE store_id = '<STORE_ID>';
```
Esperado: `status='trialing'`, `plan='grow'`, `trial_ends_at ≈ now()+7d`,
`stripe_customer_id` (`cus_...`) e `stripe_subscription_id` (`sub_...`)
preenchidos, `manual_override=false`. **Anotar `CUS_ID` e `SUB_ID`.**

Refs Stripe vazias = provisionamento Stripe falhou (ver logs da API; `/users/sync`
reprovisiona lazy — recarregar o dashboard e reconferir).

## § Fase 3 — trial expirado + bloqueio

Rota A (via Stripe, após o webhook chegar — dar até ~30s):
```sql
SELECT status, trial_ends_at, updated_at FROM subscriptions WHERE store_id = '<STORE_ID>';
-- status='paused' (end_behavior do trial sem cartão)
```

Rota B (fallback local):
```sql
UPDATE subscriptions SET trial_ends_at = now() - interval '1 minute'
WHERE store_id = '<STORE_ID>';
-- status continua 'trialing'; blocked() bloqueia pelo safety net
```

Bloqueio efetivo (qualquer rota):
```sql
-- live_comments de loja bloqueada (se houver evento ativo de teste)
SELECT result FROM live_comments WHERE event_id = '<EVENT_ID>' ORDER BY created_at DESC LIMIT 1;
-- 'blocked'
```

## § Fase 4 — conversão (pagamento)

Após concluir o Checkout e o webhook processar (dar até ~30s):
```sql
SELECT status, plan, trial_ends_at, current_period_start, current_period_end,
       cancel_at_period_end, updated_at
FROM subscriptions WHERE store_id = '<STORE_ID>';
```
Esperado: `status='active'`, `plan` = o escolhido no Checkout,
`current_period_start ≈ now()`, `current_period_end ≈ now()+1 mês`,
`updated_at` recente.

Se continuar `trialing`/`paused`: webhook não chegou/falhou — conferir
`scripts/stripe-test.sh events checkout.session.completed` e os logs da API
(assinatura inválida? tunnel caiu?).

## § Fase 6 — change plan (opcional)

```sql
SELECT status, plan, cancel_at_period_end FROM subscriptions WHERE store_id = '<STORE_ID>';
-- upgrade: plan muda imediatamente; downgrade: plan só muda no próximo ciclo
```

## § Ledger GMV (opcional, se rodar venda paga)

```sql
SELECT entry_type, amount_cents, fee_bps, fee_cents, billable, stripe_ref
FROM billing_ledger_entries WHERE store_id = '<STORE_ID>' ORDER BY created_at;
-- venda pós-conversão: billable=true, fee_bps do plano, stripe_ref='meter:gmv-<cart_id>'
```

## § Cleanup (só com confirmação do usuário)

Na Stripe (test mode) primeiro:
```bash
.claude/skills/billing-e2e/scripts/stripe-test.sh cancel <SUB_ID>
.claude/skills/billing-e2e/scripts/stripe-test.sh del-customer <CUS_ID>
```

No banco (ordem respeita as FKs; loja nova de E2E não tem produtos/eventos —
se tiver, limpar antes com o roteiro da live-flow-e2e):
```sql
DELETE FROM billing_ledger_entries WHERE store_id = '<STORE_ID>';
DELETE FROM subscriptions  WHERE store_id = '<STORE_ID>';
DELETE FROM memberships    WHERE store_id = '<STORE_ID>';
DELETE FROM stores         WHERE id = '<STORE_ID>' AND name LIKE '[E2E]%';
DELETE FROM users          WHERE email = '<EMAIL_E2E>';
```
O usuário no **Clerk** não é removido por aqui — apagar no dashboard do Clerk
(ou deixar; e-mails `+clerk_test` são descartáveis).
