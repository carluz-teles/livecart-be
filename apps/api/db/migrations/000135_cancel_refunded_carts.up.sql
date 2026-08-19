-- Reparo: carrinhos reembolsados antes do flip existir (19/08).
--
-- O reembolso passou a cancelar o carrinho (reactor de cart.refunded), mas o
-- fato dos reembolsos antigos já foi consumido — os carrinhos ficaram
-- 'active'+refunded, presos em "Precisam atenção" sem saída. Mesmo UPDATE
-- guard-first do fluxo novo, aplicado ao estoque histórico. Idempotente.
UPDATE carts
SET status = 'cancelled', cancelled_reason = 'refunded'
WHERE payment_status = 'refunded'
  AND status NOT IN ('cancelled', 'expired');
