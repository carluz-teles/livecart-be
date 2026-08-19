-- Reparo dos contadores de fila fantasma.
--
-- Sair da fila em 'waiting' matava a linha em waitlist_items mas não
-- decrementava cart_items.waitlisted_quantity (corrigido junto desta migration,
-- em inventory.CancelWaitlistItem). A varredura de 19/08 achou 5 itens em
-- carrinhos vivos anunciando 15 unidades que não estão em fila nenhuma — a
-- @daianyfer via 12 unidades fantasma no próprio carrinho.
--
-- A verdade é a soma das linhas 'waiting': o contador volta a ser exatamente
-- ela. Escopado a carrinho vivo (aberto e não pago) — em carrinho morto o
-- contador não alimenta tela nem cobrança, e mexer nele só reescreveria
-- história. Idempotente por construção.
UPDATE cart_items ci
SET waitlisted_quantity = fila.total
FROM (
    SELECT ci2.id,
           COALESCE((SELECT SUM(wi.quantity) FROM waitlist_items wi
                      WHERE wi.cart_id = ci2.cart_id
                        AND wi.product_id = ci2.product_id
                        AND wi.status = 'waiting'), 0)::int AS total
    FROM cart_items ci2
    JOIN carts c ON c.id = ci2.cart_id
    WHERE c.status = 'active' AND c.payment_status = 'pending'
      AND ci2.waitlisted_quantity > 0
) fila
WHERE ci.id = fila.id
  AND ci.waitlisted_quantity <> fila.total;
