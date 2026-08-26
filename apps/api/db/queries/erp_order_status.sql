-- name: RecordERPOrderStatus :one
-- Registra a situação do pedido no ERP, SÓ quando ela mudou.
--
-- A condição de mudança é o que faz a dedupe: o ERP entrega o mesmo webhook até
-- dez vezes quando não recebe 200, e uma redelivery não é uma transição. Sem
-- isso o histórico encheria de linhas idênticas e "quando foi despachado?"
-- deixaria de ter resposta.
--
-- FOR UPDATE serializa duas entregas simultâneas do mesmo pedido: sem ele as
-- duas leriam o estado antigo e as duas gravariam, que é a mesma duplicata por
-- outro caminho.
--
-- Zero linhas = nada mudou. Uma linha = a transição, com o estágio anterior
-- junto, para o log dizer de onde para onde.
WITH anterior AS (
    SELECT id, erp_order_status
    FROM carts
    WHERE id = sqlc.arg(cart_id)::uuid
    FOR UPDATE
), mudou AS (
    UPDATE carts c
    SET erp_order_status    = sqlc.arg(status),
        erp_order_status_at = NOW(),
        erp_order_number    = COALESCE(NULLIF(sqlc.arg(order_number)::text, ''), c.erp_order_number)
    FROM anterior a
    WHERE c.id = a.id
      AND a.erp_order_status IS DISTINCT FROM sqlc.arg(status)
    RETURNING c.id AS cart_id, c.store_id, a.erp_order_status AS previous_status
)
INSERT INTO erp_order_status_events (
    store_id, cart_id, external_order_id, order_number,
    status, previous_status, source, payload
)
SELECT COALESCE(m.store_id, sqlc.arg(store_id)::uuid),
       m.cart_id,
       sqlc.arg(external_order_id),
       NULLIF(sqlc.arg(order_number)::text, ''),
       sqlc.arg(status),
       m.previous_status,
       sqlc.arg(source),
       sqlc.arg(payload)
FROM mudou m
RETURNING id, cart_id, previous_status, status, observed_at;

-- name: RecordUnlinkedERPOrderStatus :exec
-- Mesma coisa para o pedido que não é de nenhum carrinho nosso: o lojista criou
-- direto no ERP, ou é de outro canal de venda. Não há carrinho para atualizar,
-- então guarda-se só a passagem — e ela vale, porque é assim que se descobre
-- que o webhook está entregando (o oposto também: silêncio total é sintoma).
--
-- Sem dedupe por mudança porque não há estado anterior a comparar; a chave é o
-- webhook_events, que já dedupla por event_id antes de chegar aqui.
INSERT INTO erp_order_status_events (
    store_id, cart_id, external_order_id, order_number,
    status, previous_status, source, payload
)
VALUES (
    sqlc.arg(store_id)::uuid, NULL, sqlc.arg(external_order_id),
    NULLIF(sqlc.arg(order_number)::text, ''),
    sqlc.arg(status), NULL, sqlc.arg(source), sqlc.arg(payload)
);

-- name: ListERPOrderStatusHistory :many
-- O trajeto do pedido de um carrinho, do mais recente para o mais antigo.
SELECT id, external_order_id, order_number, status, previous_status,
       source, observed_at
FROM erp_order_status_events
WHERE cart_id = sqlc.arg(cart_id)::uuid
ORDER BY observed_at DESC, id DESC;

-- name: ListStaleERPOrderStatuses :many
-- Pedidos parados num estágio não terminal há tempo demais — os candidatos a
-- terem perdido um webhook.
--
-- O ERP desiste depois de dez tentativas, e apaga a URL depois de muitas falhas
-- seguidas. Nos dois casos o LiveCart fica mostrando um estágio que já passou, e
-- ninguém percebe: o pedido simplesmente não se mexe mais. Esta lista é o que a
-- varredura pergunta de volta.
-- A loja sai de COALESCE porque carts.store_id é denormalizado (migration
-- 000138) e o evento é a fonte original. Exigir a coluna preenchida faria a
-- varredura ignorar em silêncio justamente os carrinhos mais antigos — os que
-- têm mais chance de ter perdido um webhook.
--
-- Situação NULA entra na lista, e essa é a parte importante. O webhook de
-- inclusão do ERP chega antes de o carrinho conhecer o próprio pedido — medido
-- em ~90ms de diferença —, e naquela janela a primeira situação é arquivada sem
-- dono. Um carrinho que ficasse fora daqui por não ter situação seria justamente
-- o que perdeu o primeiro aviso, e nunca mais seria reconciliado.
--
-- A idade, nesse caso, é a do carrinho: COALESCE com created_at evita perguntar
-- por um pedido que nasceu há dois segundos.
SELECT c.id AS cart_id,
       COALESCE(c.store_id, e.store_id) AS store_id,
       c.external_order_id,
       COALESCE(c.erp_order_status, '') AS erp_order_status,
       COALESCE(c.erp_order_status_at, c.created_at) AS erp_order_status_at
FROM carts c
JOIN live_events e ON e.id = c.event_id
WHERE (c.erp_order_status IS NULL
       OR c.erp_order_status NOT IN ('entregue', 'cancelado', 'nao_entregue'))
  AND c.external_order_id IS NOT NULL
  AND c.external_order_id <> ''
  AND COALESCE(c.erp_order_status_at, c.created_at) < NOW() - sqlc.arg(stale_after)::interval
ORDER BY COALESCE(c.erp_order_status_at, c.created_at) ASC
LIMIT sqlc.arg(max_rows);

-- name: AdoptOrphanERPOrderStatusEvents :execrows
-- Vincula ao carrinho as passagens que chegaram antes de sabermos que o pedido
-- era nosso.
--
-- O ERP dispara `inclusao_pedido` no instante em que o POST /pedidos responde —
-- medido chegando ~6s ANTES de gravarmos o external_order_id no carrinho. Aquela
-- primeira passagem é verdadeira e vale guardar, mas nasce sem dono; deixá-la
-- assim faria toda venda produzir uma linha órfã, e o sinal "pedido que não é
-- nosso" — que serve para saber se a entrega de webhook está viva — viraria ruído.
--
-- Adota E projeta: a passagem adotada vira a situação ATUAL do carrinho. Sem a
-- segunda metade, o carrinho continuaria sem situação e a semente da criação
-- gravaria uma segunda linha idêntica — o trajeto abriria com
-- "aberto -> aberto", que não é uma transição, é uma duplicata.
WITH adotadas AS (
    UPDATE erp_order_status_events e
    SET cart_id = sqlc.arg(cart_id)::uuid
    WHERE e.external_order_id = sqlc.arg(external_order_id)
      AND e.cart_id IS NULL
    RETURNING e.status, e.order_number, e.observed_at
), ultima AS (
    SELECT status, order_number FROM adotadas ORDER BY observed_at DESC LIMIT 1
)
UPDATE carts c
SET erp_order_status    = u.status,
    erp_order_status_at = NOW(),
    erp_order_number    = COALESCE(u.order_number, c.erp_order_number)
FROM ultima u
WHERE c.id = sqlc.arg(cart_id)::uuid;

-- name: UpdateCartERPOrderNumber :exec
-- Grava o número humano do pedido ("37"), que vem na resposta da criação. É como
-- o lojista chama o pedido ao telefone; o id interno não serve para essa conversa.
UPDATE carts
SET erp_order_number = sqlc.arg(order_number)
WHERE id = sqlc.arg(cart_id)::uuid
  AND erp_order_number IS DISTINCT FROM sqlc.arg(order_number);
