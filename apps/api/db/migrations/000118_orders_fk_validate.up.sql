-- D23, fase 2 — varre orders inteira confirmando que todo event_id e todo
-- store_id apontam para linha existente.
--
-- VALIDATE CONSTRAINT pega SHARE UPDATE EXCLUSIVE: nao bloqueia SELECT, INSERT,
-- UPDATE nem DELETE em orders. E por isso que ela mora num arquivo separado da
-- 000117 — junto com o ADD, o lock forte do ALTER seguraria a tabela durante a
-- varredura inteira.
--
-- Se ESTA migration falhar, existe pedido orfao. NAO corrija apagando: pedido
-- orfao e venda real. Levante os short_id e decida caso a caso:
--
--   SELECT o.short_id, o.event_id, o.store_id, o.paid_at
--   FROM orders o
--   LEFT JOIN live_events e ON e.id = o.event_id
--   LEFT JOIN stores s ON s.id = o.store_id
--   WHERE e.id IS NULL OR s.id IS NULL;
--
-- O esperado e ZERO: orders.cart_id (NO ACTION) ja bloqueava o CASCADE que
-- criaria o orfao.

ALTER TABLE orders VALIDATE CONSTRAINT orders_event_id_fkey;
ALTER TABLE orders VALIDATE CONSTRAINT orders_store_id_fkey;
