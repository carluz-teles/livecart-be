-- Reverte a Fatia 10-a: re-adiciona a coluna pós-venda tracking_token no cart e o
-- índice único parcial (idêntico à migration 000066 original). O valor histórico
-- não é restaurado — vive em order_logistics.tracking_token.

ALTER TABLE carts
  ADD COLUMN tracking_token TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_carts_tracking_token
  ON carts (tracking_token)
  WHERE tracking_token IS NOT NULL;
