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
-- O que o sistema ACREDITA sobre cada produto ligado ao ERP: o contador local e
-- quantas unidades estão seguradas por reserva ativa neste instante.
--
-- Existe porque não havia detecção nenhuma. O desvio de 12/08/2026 — uma unidade
-- inventada no Gabinete Gamer e uma perdida no Perfume — só apareceu porque o
-- lojista conferiu o Tiny na mão, e diagnosticá-lo exigiu reconstruir o razão a
-- partir de integration_logs. Bug de estoque é inevitável; ficar dias sem saber
-- não precisa ser.
--
-- Só produtos com vínculo no ERP: sem external_id não há saldo remoto com que
-- comparar.
SELECT p.id,
       p.name,
       p.external_id,
       p.stock::int AS local_stock,
       COALESCE(SUM(sr.quantity) FILTER (WHERE sr.status = 'active'), 0)::int AS held
FROM products p
LEFT JOIN stock_reservations sr ON sr.product_id = p.id
WHERE p.store_id = sqlc.arg(store_id)
  AND p.external_id IS NOT NULL
  AND p.external_id <> ''
  AND p.external_source = sqlc.arg(external_source)
GROUP BY p.id, p.name, p.external_id, p.stock
ORDER BY p.name;

-- name: HasActiveEventForProduct :one
SELECT EXISTS(
    SELECT 1 FROM stock_reservations sr
    JOIN live_events le ON le.id = sr.event_id
    WHERE sr.external_product_id = $1 AND sr.status = 'active' AND le.status = 'active'
) AS has_active;

-- name: HasStockGuardForProduct :one
-- Guard do sync de estoque vindo de webhook: TRUE quando o valor absoluto
-- reportado pelo ERP não pode sobrescrever o contador local — (a) existe
-- reserva ATIVA segurando o produto, ou (b) cart pago com finalização ERP em
-- voo ou falha recente contendo o produto: a reversão das reservas na
-- finalização infla o saldo do ERP por alguns segundos e o overwrite local
-- promoveria a waitlist contra unidades já vendidas.
SELECT (
    EXISTS(
        -- O que importa é ter SAÍDA EM VIGOR no ERP, não a live estar no ar.
        --
        -- Esta condição exigia `le.status = 'active'` também, e essa exigência
        -- abria a janela que estragou o estoque do Café em 08/08. A reserva
        -- sobrevive ao fim do evento: quando a campanha fecha, o carrinho vai
        -- para 'checkout' e continua segurando a unidade no Tiny até expirar ou
        -- ser pago. Nesse intervalo — 22 minutos, no caso — o evento já estava
        -- 'ended', o guard caía, e o saldo absoluto do Tiny (que está atrás do
        -- nosso justamente por causa da nossa saída) sobrescrevia o contador
        -- local. Às 18:38:15.693 o local subiu de 0 para 1 por esse caminho,
        -- um milissegundo antes de sairmos com mais uma reserva.
        --
        -- Enquanto a reserva está 'active' nós seguramos a peça no ERP, e o
        -- número dele não é fonte da verdade para o nosso contador. O status do
        -- evento não muda esse fato.
        SELECT 1 FROM stock_reservations sr
        JOIN products p ON p.id = sr.product_id
        WHERE sr.external_product_id = sqlc.arg(external_product_id)
          AND p.store_id = sqlc.arg(store_id)
          AND sr.status = 'active')
    OR EXISTS(
        -- Fatia 11b: finalização autoritativa em order_payments (join via Order);
        -- COALESCE('pending') cobre o cart pago cuja Order ainda materializa. As
        -- colunas de reserva (erp_order_state/erp_op_started_at) seguem no cart.
        SELECT 1 FROM carts c
        JOIN cart_items ci ON ci.cart_id = c.id
        JOIN products p2 ON p2.id = ci.product_id
        LEFT JOIN orders o          ON o.cart_id  = c.id
        LEFT JOIN order_payments op ON op.order_id = o.id
        WHERE p2.store_id = sqlc.arg(store_id)
          AND p2.external_source = sqlc.arg(external_source)
          AND p2.external_id = sqlc.arg(external_product_id)
          AND ((c.payment_status = 'paid'
                AND COALESCE(op.erp_finalisation_status, 'pending') <> 'done'
                AND (c.paid_at > now() - interval '30 minutes'
                     OR op.erp_last_attempt_at > now() - interval '30 minutes'))
               -- design C: conversão/mutação em voo também movimenta o saldo
               -- transitoriamente (estornos do ciclo) — mesma supressão
               OR (c.erp_order_state IN ('converting','mutating')
                   AND c.erp_op_started_at > now() - interval '30 minutes')))
)::bool AS guarded;

-- name: ReverseReservationByID :exec
-- Marcação per-row da finalização retomável: cada reserva só vira 'reversed'
-- depois que o Tiny confirmou a entrada E correspondente — o resume estorna
-- apenas as que continuam 'active'.
UPDATE stock_reservations SET status = 'reversed', reversed_at = now()
WHERE id = $1 AND status = 'active';

-- name: HasPendingCartReversalForProduct :one
-- TRUE quando existe unidade JÁ CREDITADA no estoque local cujo estorno no ERP
-- ainda não terminou. Nessa janela o sync tem de ser SUPRIMIDO por inteiro —
-- não "só reduções".
--
-- Por que a distinção importa. Cancelar carrinho credita o local de uma vez,
-- dentro de uma transação; o estorno no Tiny sai um a um, por HTTP, fora dela.
-- Cada estorno dispara um webhook que nos faz reler o saldo ABSOLUTO do ERP —
-- que naquele instante está atrasado em relação a nós, e atrasado por nossa
-- causa. O `downgrade_only` lê "ERP menor que o local" como redução legítima do
-- lojista e grava o valor do ERP.
--
-- Foi assim que 1001 e 1004 caíram de 5 para 4 em staging (03/08): dois
-- estornos pendentes cada, o primeiro webhook chegou com o Tiny uma unidade
-- atrás e cravou o valor. E a mesma regra depois IMPEDE a autocorreção: quando
-- o Tiny alcança 5, `5 >= 4` preserva o 4 errado. O erro é gravado por uma
-- regra e protegido por ela.
--
-- Só cart em estado TERMINAL entra: em cart vivo a reserva ativa é normal e já
-- é coberta pelo primeiro EXISTS de HasStockGuardForProduct.
--
-- A JANELA NÃO FECHA NA MARCAÇÃO `reversed` — ela fecha alguns minutos DEPOIS.
--
-- Marcar a reserva como estornada só diz que a chamada ao ERP retornou. O
-- webhook que o Tiny dispara por causa daquele movimento chega DEPOIS, e é ele
-- que carrega o saldo. Em staging (04/08) a diferença foi de 27 segundos:
--
--   18:39:14  últimos estornos terminam, reservas marcadas → guarda solta
--   18:39:41  webhook do Tiny chega e sobrescreve estoque local 5 → 2, 9, 7, 6
--
-- O log provou que o nosso número estava CERTO (local_stock=5 nos quatro) e que
-- foi o eco atrasado do nosso próprio estorno que o destruiu. Fechar a janela
-- na marcação era fechá-la exatamente antes da parte que importa.
--
-- 3 minutos é folgado para a fila de webhooks do Tiny e curto perto de qualquer
-- alteração real de estoque feita pelo lojista, que continua chegando depois.
SELECT EXISTS(
    SELECT 1 FROM stock_reservations sr
    JOIN carts c ON c.id = sr.cart_id
    JOIN products p ON p.id = sr.product_id
    WHERE sr.external_product_id = sqlc.arg(external_product_id)
      AND p.store_id = sqlc.arg(store_id)
      AND c.status IN ('cancelled', 'expired')
      AND (
            -- Estorno ainda pendente, mas com TETO. Sem ele, uma reserva que
            -- nunca fecha (estorno falhou, ctx morreu) desliga o sync daquele
            -- produto PARA SEMPRE — foi o que aconteceu em 05/08: 7 reservas
            -- presas em carrinhos cancelados suprimiram o sync de 6 produtos
            -- indefinidamente, e a correção que o lojista fizesse no Tiny nunca
            -- chegaria até nós. A trava de segurança não pode ser eterna.
            (sr.status = 'active'
             AND sr.created_at > now() - interval '30 minutes')
            -- Estorno concluído: segura pelo eco atrasado do Tiny.
         OR sr.reversed_at > now() - interval '3 minutes'
      )
) AS has_pending;

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
