-- Plano único self-service ("pro") com 3 intervalos de cobrança (mensal,
-- semestral, anual) via os 3 preços do produto agrupados no Customer Portal —
-- a troca de intervalo passa a ser feita 100% pelo Portal, sem endpoint novo.
-- A comissão sobre GMV é eliminada para todo mundo (fee sempre 0 a partir de
-- agora); o ledger/gmv.recorded continuam existindo só para analytics.
ALTER TABLE subscriptions
  ADD COLUMN billing_interval VARCHAR NOT NULL DEFAULT 'monthly'
  CHECK (billing_interval IN ('monthly', 'semestral', 'annual'));

-- Mantém 'start','grow','scale' no CHECK: assinaturas antigas no banco ainda
-- podem carregar esses valores até uma migração de dados separada — fora do
-- escopo deste PR, que só cuida do schema.
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_plan_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_plan_check
  CHECK (plan IN ('pro', 'enterprise', 'start', 'grow', 'scale'));

-- A coluna plan tinha DEFAULT 'grow' (plano descontinuado) — sem efeito hoje
-- (o único INSERT em subscriptions já passa plan explicitamente), mas
-- corrigido por segurança/consistência com o plano único.
ALTER TABLE subscriptions ALTER COLUMN plan SET DEFAULT 'pro';
