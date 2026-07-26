-- Fatia C1 — order_events passa a ser keyed pela Order (order_id), não mais pelo
-- cart. A tabela nasceu (000067) com FK cart_id e UNIQUE (cart_id, event_type),
-- mas a Order já é a raiz imutável do pedido (000094): a timeline pertence a ela.
--
-- Escopo: adiciona order_id (NULLable durante a transição — a materialização da
-- Order roda antes do postcheckout no fluxo do webhook, mas o path síncrono do
-- cartão emite o hook antes da Order existir), faz o backfill via orders.cart_id
-- e MOVE a UNIQUE de (cart_id, event_type) para (order_id, event_type).
--
-- cart_id PERMANECE por enquanto (dual-key) — sua remoção é Fase F/D1, fora deste
-- escopo. Isso mantém ListOrderEventsByCart/HasOrderEvent (ainda keyed por cart)
-- funcionando durante o cutover.

ALTER TABLE order_events
    ADD COLUMN IF NOT EXISTS order_id UUID REFERENCES orders(id) ON DELETE CASCADE;

-- Backfill: liga cada evento existente à Order materializada do mesmo cart.
UPDATE order_events oe
SET order_id = o.id
FROM orders o
WHERE o.cart_id = oe.cart_id
  AND oe.order_id IS NULL;

-- FK columns não são auto-indexadas no Postgres. Índice para o join/lookup por
-- Order e para a listagem cronológica futura keyed por order_id.
CREATE INDEX IF NOT EXISTS idx_order_events_order_time
    ON order_events (order_id, occurred_at);

-- Idempotência passa a ser por Order: um evento de cada tipo por Order. O INSERT
-- do postcheckout usa ON CONFLICT (order_id, event_type) DO NOTHING contra este
-- índice para permanecer idempotente sob retry de webhook.
DROP INDEX IF EXISTS idx_order_events_cart_type_unique;
CREATE UNIQUE INDEX IF NOT EXISTS idx_order_events_order_type_unique
    ON order_events (order_id, event_type);
