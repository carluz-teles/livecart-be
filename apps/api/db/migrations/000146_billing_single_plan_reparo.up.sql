-- REPARO da migration 000139, que nunca rodou em staging.
--
-- O que aconteceu: o número 139 foi usado por DUAS migrations diferentes em
-- branches diferentes. O commit 4bba36d ("a 139 estava duplicada entre os
-- branches") renomeou a de ERP para 000144 e deixou a de billing como 000139 —
-- consertando o conflito de ARQUIVO, mas não o de ESTADO.
--
-- golang-migrate guarda um único inteiro e só aplica versões MAIORES que ele.
-- Produção tinha rodado a 139 quando ela era a de billing, então ganhou a
-- coluna. Staging tinha rodado a 139 quando ela era a de ERP: para ele, 139 já
-- estava "aplicada", e a de billing nunca rodaria — nem agora, nem nunca.
--
-- O sintoma, medido em staging em 31/08/2026:
--
--   billing guard lookup failed (fail-open)
--   ERROR: column "billing_interval" does not exist (SQLSTATE 42703)
--
-- Centenas de linhas, em toda requisição autenticada, porque o guard de billing
-- e o `ensuring local trial` batem no banco a cada chamada. Fail-open: nada
-- quebrou visivelmente, e por isso passou dias sem ser notado.
--
-- Esta migration é IDEMPOTENTE de propósito: em produção, que já tem tudo, ela
-- não faz nada. Um ambiente novo montado do zero pelas migrations aplica a 139
-- normalmente e esta também vira no-op. Ela existe para os ambientes que já
-- passaram do ponto — e é a única forma de alcançá-los.

ALTER TABLE subscriptions
  ADD COLUMN IF NOT EXISTS billing_interval VARCHAR NOT NULL DEFAULT 'monthly';

-- Postgres não tem ADD CONSTRAINT IF NOT EXISTS; o catálogo responde por nós.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'subscriptions'::regclass
      AND conname = 'subscriptions_billing_interval_check'
  ) THEN
    ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_billing_interval_check
      CHECK (billing_interval IN ('monthly', 'semestral', 'annual'));
  END IF;
END $$;

-- O CHECK de `plan` da 139 mantém os planos antigos no banco enquanto não há
-- migração de dados. Reescrito aqui para o mesmo alvo que produção tem hoje.
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_plan_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_plan_check
  CHECK (plan IN ('pro', 'enterprise', 'start', 'grow', 'scale'));

ALTER TABLE subscriptions ALTER COLUMN plan SET DEFAULT 'pro';
