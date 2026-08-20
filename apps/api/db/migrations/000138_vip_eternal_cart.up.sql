-- Carrinho ETERNO do cliente VIP (feature Clientes VIP).
--
-- never_expires: o carrinho nunca ganha expires_at, é excluído do
-- FinalizeCartsByEvent e o worker cart.expire se abstém. Só fecha quando pago
-- ou cancelado.
--
-- store_id: denormalizado de live_events.store_id. Necessário porque o carrinho
-- VIP é resolvido/único por (LOJA, comprador) atravessando eventos — um índice
-- não faz JOIN para chegar na loja via o evento. Preenchido na criação e
-- backfillado abaixo.
ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS never_expires BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS store_id UUID REFERENCES stores(id) ON DELETE CASCADE;

-- Backfill do store_id a partir do evento âncora. Idempotente (só onde está NULL).
UPDATE carts c
SET store_id = e.store_id
FROM live_events e
WHERE e.id = c.event_id AND c.store_id IS NULL;

-- Um carrinho eterno ABERTO por (loja, comprador), atravessando eventos. É o
-- análogo VIP do carts_one_open_per_event_buyer — mas a chave é a LOJA, não o
-- evento, porque o carrinho VIP sobrevive a qualquer fechamento de evento.
CREATE UNIQUE INDEX IF NOT EXISTS carts_one_eternal_per_store_buyer
    ON carts (store_id, platform_handle)
    WHERE never_expires
      AND status IN ('pending', 'active', 'checkout')
      AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'));
