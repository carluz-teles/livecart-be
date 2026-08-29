-- name: CreateStockReservation :one
INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, erp_movement_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListActiveReservationsByEvent :many
SELECT * FROM stock_reservations WHERE event_id = $1 AND status = 'active' ORDER BY created_at ASC;

-- name: ListActiveReservationsByCart :many
SELECT * FROM stock_reservations WHERE cart_id = $1 AND status = 'active' ORDER BY created_at ASC;

-- name: ListActiveReservationsByCartAndProduct :many
SELECT * FROM stock_reservations WHERE cart_id = $1 AND product_id = $2 AND status = 'active' ORDER BY created_at ASC;

-- name: ReverseReservationsByCart :exec
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE cart_id = $1 AND status = 'active';

-- name: ReverseReservationsByCartAndProduct :exec
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE cart_id = $1 AND product_id = $2 AND status = 'active';

-- name: ConvertReservationsByEvent :exec
UPDATE stock_reservations SET status = 'converted', reversed_at = now()
WHERE event_id = $1 AND status = 'active';

-- name: AdjustActiveReservationQuantity :one
-- Atomically increase or decrease the active reservation quantity for a
-- (cart, product) pair. delta_qty can be negative. Returns the row whose
-- quantity was adjusted (callers can flip status when it hits zero).
UPDATE stock_reservations
SET quantity = quantity + sqlc.arg(delta_qty)::int,
    erp_movement_id = COALESCE(NULLIF(sqlc.arg(erp_movement_id)::text, ''), erp_movement_id)
WHERE cart_id = sqlc.arg(cart_id) AND product_id = sqlc.arg(product_id) AND status = 'active'
RETURNING *;

-- name: UpsertActiveReservationQuantity :one
-- Soma `inc_qty` à reserva ativa deste (carrinho, produto, evento), criando a
-- linha se ela ainda não existir. Uma chamada, uma decisão, sem leitura prévia.
--
-- Substitui o par "listar reservas ativas / decidir entre CREATE e ADJUST", que
-- é uma corrida: a lista é lida antes da chamada ao ERP (~1s pelo limitador) e
-- decide um ramo que já não vale quando a gravação acontece. Os dois desfechos
-- apareceram em produção em 12/08/2026, no mesmo teste:
--
--   "no rows in result set"  -> leu reserva ativa, ela foi reversada no meio,
--                               o ADJUST não achou linha
--   "duplicate key ... uq_stock_reservations_active" -> leu vazio, outra
--                               requisição criou a linha, o CREATE colidiu
--
-- Nos dois casos o movimento JÁ tinha ido ao Tiny, e o comprador levou 422
-- depois de o estoque ter se mexido. Clicando rápido no "+", ele tentava de
-- novo, e cada tentativa repetia o ciclo.
--
-- O ON CONFLICT usa o índice parcial uq_stock_reservations_active
-- (cart_id, product_id, event_id) WHERE status = 'active'.
INSERT INTO stock_reservations (event_id, cart_id, product_id, external_product_id, quantity, erp_movement_id)
VALUES (sqlc.arg(event_id), sqlc.arg(cart_id), sqlc.arg(product_id),
        sqlc.arg(external_product_id), sqlc.arg(inc_qty)::int, sqlc.arg(erp_movement_id))
ON CONFLICT (cart_id, product_id, event_id) WHERE status = 'active'
DO UPDATE SET quantity = stock_reservations.quantity + sqlc.arg(inc_qty)::int,
              erp_movement_id = COALESCE(NULLIF(sqlc.arg(erp_movement_id)::text, ''), stock_reservations.erp_movement_id)
RETURNING *;

-- name: DecrementActiveReservationQuantity :many
-- Baixa `dec_qty` unidades da reserva ativa, mas SÓ se ela tiver esse tanto.
-- Zero linhas significa "não pude" — leitura obsoleta, reserva menor que o
-- pedido, ou outra requisição chegou antes.
--
-- Existe porque decidir o ramo (reversão total × decremento parcial) a partir
-- de uma leitura anterior é uma corrida. Em 12/08/2026 um PATCH e um DELETE do
-- mesmo item se cruzaram: o DELETE leu `cart_items` já atualizado (1) e
-- `stock_reservations` ainda desatualizado (2), concluiu que sobraria 1 unidade,
-- mandou a entrada ao Tiny e só então tentou `1 + (-1) = 0` — que bate no
-- CHECK (quantity > 0) da migration 000030. O movimento já estava no Tiny e
-- ninguém o compensou: +1 unidade fantasma no Gabinete Gamer.
--
-- Com o guard `quantity >= dec_qty` dentro do próprio UPDATE, quem decide é o
-- banco, e a decisão já vem aplicada. O chamador só chama o ERP depois.
-- Os dois desfechos numa tacada só. Baixa parcial quando sobra unidade; quando
-- a baixa consome a reserva inteira, a linha sai de 'active' com a quantidade
-- INTACTA — zerar em vigor violaria o CHECK (quantity > 0) da migration 000030,
-- que é exatamente a pedra em que o fluxo antigo tropeçou.
--
-- Separar em duas queries traria de volta a corrida: entre decidir qual usar e
-- executá-la, outra requisição muda a reserva.
UPDATE stock_reservations
SET quantity = CASE WHEN quantity > sqlc.arg(dec_qty)::int
                    THEN quantity - sqlc.arg(dec_qty)::int
                    ELSE quantity END,
    status = CASE WHEN quantity <= sqlc.arg(dec_qty)::int THEN 'reversed' ELSE status END,
    reversed_at = CASE WHEN quantity <= sqlc.arg(dec_qty)::int THEN now() ELSE reversed_at END,
    erp_movement_id = COALESCE(NULLIF(sqlc.arg(erp_movement_id)::text, ''), erp_movement_id)
WHERE cart_id = sqlc.arg(cart_id)
  AND product_id = sqlc.arg(product_id)
  AND status = 'active'
  AND quantity >= sqlc.arg(dec_qty)::int
RETURNING *;

-- name: RestoreReservationQuantityByID :execrows
-- Desfaz DecrementActiveReservationQuantity quando o ERP recusa DEPOIS do
-- decremento local. Sem isso o banco diria que a unidade está livre e o Tiny
-- diria que está reservada, e nada reconciliaria as duas versões.
--
-- Por ID, e não por (cart, produto), porque o desfazer tem de acertar
-- exatamente a linha que acabou de ser mexida — filtrar por par resgataria
-- reservas reversadas em outro momento.
--
-- Espelha os dois desfechos do decremento: se a linha saiu de 'active' com a
-- quantidade intacta (baixa total), só o status volta; se foi baixa parcial,
-- as unidades voltam.
UPDATE stock_reservations
SET quantity = CASE WHEN status = 'reversed' THEN quantity ELSE quantity + sqlc.arg(inc_qty)::int END,
    status = 'active',
    reversed_at = NULL
WHERE id = sqlc.arg(id);

-- name: ListStockPositionsForReconciliation :many
-- O contador local de cada produto ligado ao ERP — o lado de cá da comparação.
--
-- Havia uma segunda coluna aqui, `held`, somando as reservas ativas para que a
-- conta fosse `local − held = saldo remoto`. Ela saiu junto com as reservas
-- manuais: hoje quem segura a peça é o próprio pedido de venda, e o `disponivel`
-- que o ERP devolve já vem com essas unidades descontadas. Os dois lados passaram
-- a medir a mesma coisa, e a igualdade ficou direta — `local == disponivel`.
--
-- Existe porque não havia detecção nenhuma. O desvio de 12/08/2026 — uma unidade
-- inventada no Gabinete Gamer e uma perdida no Perfume — só apareceu porque o
-- lojista conferiu o ERP na mão, e diagnosticá-lo exigiu reconstruir o razão a
-- partir de integration_logs. Bug de estoque é inevitável; ficar dias sem saber
-- não precisa ser.
--
-- Só produtos com vínculo no ERP: sem external_id não há saldo remoto com que
-- comparar.
SELECT p.id,
       p.name,
       p.external_id,
       p.stock::int AS local_stock
FROM products p
WHERE p.store_id = sqlc.arg(store_id)
  AND p.external_id IS NOT NULL
  AND p.external_id <> ''
  AND p.external_source = sqlc.arg(external_source)
ORDER BY p.name;
-- name: ReverseReservationByID :exec
-- Marcação per-row da finalização retomável: cada reserva só vira 'reversed'
-- depois que o Tiny confirmou a entrada E correspondente — o resume estorna
-- apenas as que continuam 'active'.
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE id = $1 AND status = 'active';
-- name: ClaimReservationForReversal :execrows
-- Reivindica a reserva ANTES de mandar o estorno ao ERP. Devolve 1 para quem
-- ganhou a corrida e 0 para todo o resto.
--
-- Inverte a ordem que produziu estoque fantasma em 08/08. Antes era: estorna no
-- Tiny, depois marca 'reversed'. Quando a marcação falhava — "context deadline
-- exceeded" às 15:29:28 — a reserva continuava 'active', o handler devolvia
-- erro, a asynq retentava (max_retry 3), e a retentativa via a reserva ainda
-- ativa e mandava a MESMA entrada de novo. O Tiny registrou duas linhas
-- idênticas para a reserva f4590b1f, 12:29 e 12:30, e o produto terminou com
-- 7 unidades onde deviam existir 5.
--
-- Reivindicando primeiro, a segunda tentativa recebe 0 e não chama o ERP. O
-- estorno duplo deixa de ser possível por construção, e não por o UPDATE ter
-- dado certo a tempo.
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE id = @id AND status = 'active';

-- name: RestoreReservationToActive :execrows
-- Devolve a reserva reivindicada ao estado ativo quando o ERP recusou o
-- estorno. Sem isto a reivindicação seria uma via de mão única: a unidade
-- ficaria presa no Tiny para sempre, porque nenhuma tentativa futura a veria.
--
-- Só desfaz o que ESTA execução reivindicou (status ainda 'reversed' e sem
-- movimento gravado). O que já foi confirmado no ERP nunca volta.
UPDATE stock_reservations SET status = 'active', reversed_at = NULL
WHERE id = @id AND status = 'reversed';

-- name: ListCartsWithActiveReservations :many
-- Carrinhos da loja que ainda seguram peça por reserva MANUAL — a lista de
-- trabalho da drenagem única.
--
-- Ordena do mais novo para o mais antigo de propósito: os carrinhos recentes são
-- os de uma live em andamento, e são eles que não podem ficar um segundo sem
-- estoque segurado. Os antigos já estão parados há dias e podem esperar.
SELECT c.id::text AS cart_id,
       COALESCE(c.store_id, e.store_id)::text AS store_id,
       c.status,
       COALESCE(c.payment_status, '') AS payment_status,
       COALESCE(c.external_order_id, '') AS external_order_id,
       e.status AS event_status,
       COUNT(sr.id)::int AS reservation_rows,
       COALESCE(SUM(sr.quantity), 0)::int AS reserved_units
FROM carts c
JOIN live_events e ON e.id = c.event_id
JOIN stock_reservations sr ON sr.cart_id = c.id AND sr.status = 'active'
WHERE COALESCE(c.store_id, e.store_id) = sqlc.arg(store_id)::uuid
GROUP BY c.id, c.store_id, e.store_id, c.status, c.payment_status, c.external_order_id, e.status, c.created_at
ORDER BY c.created_at DESC;
