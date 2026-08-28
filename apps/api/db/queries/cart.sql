-- =============================================================================
-- CARTS (belong to events, with optional session tracking)
-- =============================================================================

-- name: CreateCart :one
INSERT INTO carts (event_id, session_id, platform_user_id, platform_handle, token, status, expires_at, customer_id, short_id, store_id, never_expires)
VALUES ($1, $2, $3, $4, $5, 'active', $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetEternalCartByStoreAndHandleForUpdate :one
-- Resolução do carrinho ETERNO do VIP: por (loja, @), ATRAVESSANDO eventos.
-- É isto que faz a compra do VIP num evento novo cair no MESMO carrinho de um
-- evento anterior. FOR UPDATE serializa dois comentários concorrentes do VIP.
--
-- O carrinho PAGO continua sendo o mesmo carrinho, e essa é a regra que o
-- lojista pediu por extenso: pagou na live de segunda, pediu mais uma coisa na
-- quinta, sai numa caixa só — um frete, uma nota. Enquanto o pedido não virou
-- documento fiscal ele ainda recebe item, e o que entrou depois do pagamento
-- fica separado por cart_items.paid_at (ver migration 000140).
--
-- O FATURAMENTO é o portão. Depois dele a nota existe, e somar item seria emitir
-- nota errada — então a compra de quinta abre um pedido NOVO. Note que o ERP não
-- impõe esse limite: em 26/08/2026 ele aceitou (204) editar os itens de um
-- pedido "Faturada". A recusa é nossa.
--
-- 'preparando_envio' está na lista de fechados apesar do nome e apesar de a
-- lista do enum o colocar ANTES de 'faturado': na operação o pedido só entra em
-- preparo depois de a nota sair. Ver ERPOrderStatus.FechadoParaNovosItens.
--
-- Estornado fica de fora pelo motivo oposto: não há venda a que somar.
SELECT * FROM carts
WHERE store_id = $1 AND platform_handle = $2
  AND never_expires
  AND status IN ('pending', 'active', 'checkout', 'paid')
  AND (payment_status IS NULL OR payment_status <> 'refunded')
  AND (erp_order_status IS NULL OR erp_order_status NOT IN (
        'preparando_envio', 'faturado', 'pronto_envio', 'enviado', 'entregue',
        'nao_entregue', 'cancelado'))
ORDER BY created_at DESC
LIMIT 1
FOR UPDATE;

-- A PROMOÇÃO A VIP CONSOLIDA, NÃO MARCA EM MASSA.
--
-- ActivateEternalCartsForHandle (removida) era UM update que marcava
-- never_expires em TODOS os carrinhos abertos do @. Só que o modelo admite UM
-- carrinho eterno por comprador — carts_one_eternal_per_store_buyer, índice
-- único parcial em (store_id, platform_handle). Quem chega à promoção com dois
-- carrinhos abertos é o caso NORMAL, não o raro: antes de virar VIP o
-- comprador ganha um carrinho por evento. O update então violava o índice, e
-- como era uma instrução só, o Postgres desfazia tudo: NENHUM carrinho virava
-- eterno, o erro era engolido pelo best-effort do chamador e a promoção
-- respondia 200. Aconteceu em produção em 26/08/2026 com @eulalisueli, com
-- R$ 2.480,80 em dois carrinhos e o mais antigo a horas de expirar.
--
-- No lugar entra uma consolidação, que é o que "carrinho eterno que acumula
-- entre eventos" sempre significou: o carrinho aberto mais recente recebe os
-- itens dos outros e vira o eterno; os demais são fechados como fundidos.

-- name: ListOpenCartsForVipPromotion :many
-- Os carrinhos abertos de um @ no instante em que ele é promovido a VIP, do
-- mais novo para o mais velho e travados para a consolidação.
--
-- A ordem é a MESMA de GetEternalCartByStoreAndHandleForUpdate (created_at
-- DESC): o primeiro da lista é o que o resolvedor vai encontrar depois, então
-- é ele que tem de virar o eterno. Qualquer outro critério faria a promoção
-- eleger um carrinho e a ingestão procurar outro.
--
-- erp_order_state/external_order_id vêm junto porque um carrinho que já tem
-- pedido no ERP não pode ser esvaziado às cegas: os itens iriam embora e o
-- pedido lá dentro continuaria segurando peça. Esse fica de fora da fusão e é
-- devolvido ao chamador para decisão humana.
--
-- O filtro é o MESMO de GetEternalCartByStoreAndHandleForUpdate, e tem de
-- continuar sendo: quem a promoção elege é quem a ingestão vai procurar depois.
-- Pago fica de fora porque aquela venda está fechada — o carrinho junta até ser
-- pago ou cancelado. Faturado também: emitida a nota, o pedido não recebe mais
-- item, então esvaziá-lo seria mexer no que já virou documento fiscal.
SELECT id, status, created_at, erp_order_state, external_order_id FROM carts
WHERE store_id = $1 AND platform_handle = $2
  AND status IN ('pending', 'active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
  AND (erp_order_status IS NULL OR erp_order_status NOT IN (
        'preparando_envio', 'faturado', 'pronto_envio', 'enviado', 'entregue',
        'nao_entregue', 'cancelado'))
ORDER BY created_at DESC
FOR UPDATE;

-- name: AbsorbCartItemsIntoCart :exec
-- Move os itens de UM carrinho de origem para o destino. O mesmo produto nos
-- dois soma quantidade (é o mesmo comprador querendo mais daquilo), e o preço
-- que fica é o do destino — o carrinho vivo é o mais recente, e é o preço dele
-- que o comprador está vendo na tela. O histórico de cada adição, com o preço
-- praticado na hora, continua íntegro em cart_item_events.
--
-- session_id viaja junto na linha nova: é a atribuição de primeiro toque, e é
-- o que mantém a métrica por evento de origem correta depois da fusão.
-- paid_quantity viaja junto, e isso não é detalhe: uma unidade PAGA que chegue
-- ao destino sem a marca vira "a pagar" na hora, e o pedido no ERP passaria a
-- cobrar de novo o que a compradora já pagou. Foi o furo que a junção manual
-- revelou — a fusão do VIP não o exibia porque só junta carrinho não pago.
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity, session_id, paid_quantity)
SELECT sqlc.arg(dest_cart_id), s.product_id, s.quantity, s.unit_price, s.waitlisted_quantity, s.session_id, s.paid_quantity
FROM cart_items s
WHERE s.cart_id = sqlc.arg(source_cart_id)
ON CONFLICT (cart_id, product_id) DO UPDATE
SET quantity            = cart_items.quantity + EXCLUDED.quantity,
    waitlisted_quantity = cart_items.waitlisted_quantity + EXCLUDED.waitlisted_quantity,
    paid_quantity       = cart_items.paid_quantity + EXCLUDED.paid_quantity;

-- name: MovePaymentsToCart :exec
-- Leva o extrato de cobranças junto com os itens.
--
-- Sem isto o dinheiro ficaria no carrinho fechado: o destino mostraria os itens
-- da compra antiga como "a pagar" e a compradora seria cobrada duas vezes pela
-- mesma coisa. O checkout_id continua sendo a chave de idempotência, agora sob
-- o carrinho novo.
UPDATE cart_payments SET cart_id = sqlc.arg(dest_cart_id)
WHERE cart_id = sqlc.arg(source_cart_id);

-- name: AccumulatePaidAmountFromCart :exec
-- Soma ao destino o que a origem já tinha recebido, e zera a origem — o total
-- pago é do carrinho que sobreviveu, e contá-lo nos dois inflaria o faturamento.
WITH antes AS (
    SELECT paid_amount_cents, paid_at FROM carts WHERE id = sqlc.arg(source_cart_id)::uuid
), zera AS (
    UPDATE carts SET paid_amount_cents = 0 WHERE id = sqlc.arg(source_cart_id)::uuid RETURNING 1
)
UPDATE carts d
SET paid_amount_cents = d.paid_amount_cents + COALESCE((SELECT paid_amount_cents FROM antes), 0),
    -- A data do pagamento mais ANTIGO sobrevive: é quando o dinheiro entrou
    -- pela primeira vez neste pedido, e é o que a parcela "PAGO" carrega.
    paid_at = LEAST(d.paid_at, (SELECT paid_at FROM antes))
WHERE d.id = sqlc.arg(dest_cart_id)::uuid;

-- name: ClearCartItems :exec
DELETE FROM cart_items WHERE cart_id = $1;

-- name: MoveCartItemEventsToCart :exec
-- O log de adições acompanha os itens. É ele que diz DE QUAL sessão veio cada
-- unidade (ListCartItemEventsByEvent → AllocateBySession); deixá-lo no carrinho
-- de origem faria as peças fundidas aparecerem sem atribuição na métrica por
-- evento — exatamente o que o carrinho eterno cross-evento existe para medir.
UPDATE cart_item_events SET cart_id = sqlc.arg(dest_cart_id)
WHERE cart_id = sqlc.arg(source_cart_id);

-- name: MoveStockReservationsToCart :exec
-- A reserva segue a peça. event_id e erp_movement_id ficam como estão: a peça
-- continua reservada no ERP pelo movimento original, só mudou de carrinho.
UPDATE stock_reservations SET cart_id = sqlc.arg(dest_cart_id)
WHERE cart_id = sqlc.arg(source_cart_id);

-- name: MoveWaitlistItemsToCart :exec
UPDATE waitlist_items SET cart_id = sqlc.arg(dest_cart_id)
WHERE cart_id = sqlc.arg(source_cart_id);

-- name: CloseMergedCart :exec
-- O carrinho de origem sai de cena depois de entregar o conteúdo. Não é uma
-- expiração nem um cancelamento do lojista: o conteúdo não morreu, mudou de
-- endereço. cart_mutations e cart_initial_items ficam onde estão — descrevem o
-- que aconteceu NAQUELE carrinho.
UPDATE carts
SET status = 'cancelled', cancelled_reason = 'merged_into_vip_cart', expires_at = NULL
WHERE id = $1;

-- name: MakeCartEternal :exec
-- O sobrevivente vira eterno. expires_at NULL já basta para o worker cart.expire
-- virar no-op; never_expires é a defesa explícita nos guards.
UPDATE carts SET never_expires = true, expires_at = NULL WHERE id = $1;

-- name: ExtendCartExpiration :exec
-- Empurra expires_at para no mínimo @new_expires_at ("gordura" extra para
-- clientes promovidos da waitlist). GREATEST evita encolher o prazo se o
-- cart já tem um expires_at maior do que o pedido.
--
-- CARRINHO SEM PRAZO NÃO GANHA UM AQUI. expires_at NULL é a RN-04: o carrinho
-- não expira enquanto o evento roda. É o prazo mais LONGO que existe, não a
-- ausência de prazo — e era exatamente ao contrário que esta query o tratava.
--
-- O COALESCE que estava aqui virava NULL no valor novo, e o GREATEST comparava
-- o valor novo com ele mesmo: promover alguém da fila CRIAVA um prazo de 30
-- minutos num carrinho que tinha até o fim da campanha. Aconteceu em staging —
-- o comprador foi promovido às 14:10, nunca recebeu o aviso (a DM falhou),
-- adicionou outro produto às 14:39 e perdeu o carrinho inteiro às 14:40, com o
-- evento aberto até dois dias depois.
--
-- A intenção da regra é dar tempo A MAIS a quem esperou. Encurtar era o oposto.
-- O prazo da FILA continua existindo e continua vencendo: ele mora na linha da
-- waitlist (notified_until) e é ExpireNotifiedWaitlistItem quem o aplica —
-- devolvendo o item ao estoque, não matando o carrinho do comprador.
UPDATE carts
SET expires_at = GREATEST(expires_at, @new_expires_at::timestamptz)
WHERE id = $1
  AND expires_at IS NOT NULL;

-- name: UpdateCartShippingAddress :exec
-- Replaces the cart's shipping_address JSONB. Used by the admin's "edit
-- address" action — narrower than UpdateCartCustomerCheckout (which also
-- writes customer fields) so we don't accidentally clear contact info.
UPDATE carts
SET shipping_address = $2
WHERE id = $1;

-- RegenerateCartCheckout foi REMOVIDA: nunca teve chamador Go (o "regerar link"
-- do painel usa order/repository.RegenerateCheckout) e ainda devolvia o carrinho
-- para status='active' com expires_at no futuro — o estado que a RN-06 declara
-- impossível. Portá-la seria carregar código morto E errado.

-- name: IssueShortIDForEvent :one
-- Atomically issues the next short_id for the store that owns the given event.
-- On first call for a store, INSERT seeds last_value at 1000 (the chosen
-- starting number). On subsequent calls, the ON CONFLICT UPDATE bumps
-- last_value by 1 and returns the new value. Either way the RETURNING clause
-- gives the caller the short_id it should write into the cart row in the same
-- transaction. Accepts event_id so callers don't need to resolve store_id
-- themselves.
INSERT INTO store_order_counters (store_id, last_value)
SELECT e.store_id, 1000 FROM live_events e WHERE e.id = $1
ON CONFLICT (store_id) DO UPDATE SET last_value = store_order_counters.last_value + 1
RETURNING last_value;

-- name: GetCartByID :one
SELECT * FROM carts WHERE id = $1;

-- name: GetCartByToken :one
SELECT * FROM carts WHERE token = $1;

-- Fatia 10-a: o tracking_token pós-venda saiu do cart. A fonte da verdade é
-- order_logistics.tracking_token (Fatia C1); a escrita usa
-- SetOrderLogisticsTrackingToken e o lookup público, GetCartByOrderLogisticsTrackingToken
-- (ambos em order_write.sql).

-- name: GetCartByEventAndUser :one
-- O carrinho ABERTO do comprador neste evento.
--
-- Desde a 000107 pode existir mais de um carrinho por (evento, comprador): pagar
-- ou expirar libera a vaga e um novo nasce (RN-07/RN-08). Sem o filtro abaixo,
-- `:one` gera QueryRow do pgx, que lê a PRIMEIRA linha e descarta o resto SEM
-- erro — o item do comprador cairia num carrinho arbitrário, possivelmente o já
-- pago. O ORDER BY é a desempate determinística caso o filtro deixe passar mais
-- de um (não deveria: o índice parcial garante no máximo um).
SELECT * FROM carts
WHERE event_id = $1 AND platform_user_id = $2
  AND status IN ('pending', 'active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
ORDER BY created_at DESC
LIMIT 1;

-- name: ListCartsByCustomer :many
-- Returns all carts for a specific customer with totals
SELECT
    c.*,
    cart_product_total_cents(c.id) AS total_value,
    COALESCE(SUM(ci.quantity), 0)::int AS total_items
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id
WHERE c.customer_id = $1
GROUP BY c.id
ORDER BY c.created_at DESC
LIMIT $2 OFFSET $3;

-- name: UpdateCartCustomerID :exec
UPDATE carts SET customer_id = $2 WHERE id = $1;

-- name: GetCartTotals :one
-- Returns total items and value for a cart (for notifications)
SELECT
    COALESCE(SUM(ci.quantity), 0)::int AS total_items,
    cart_product_total_cents($1)::bigint AS total_value
FROM cart_items ci
WHERE ci.cart_id = $1;

-- name: UpdateCartStatus :one
UPDATE carts SET status = $2 WHERE id = $1 RETURNING *;

-- name: ExpireCart :one
-- Flip idempotente e guard-first do worker de expiração. O guard vive DENTRO do
-- UPDATE para fechar a corrida com o webhook de pagamento: se alguém pagou ou o
-- cart já foi expirado/cancelado no intervalo, 0 rows retornam e o caller ABORTA
-- sem devolver estoque nem tocar o ERP. Marcar 'expired' é a PRIMEIRA ação (no
-- mesmo tx da devolução de estoque local) — a ação irreversível de ERP só roda
-- depois que o cart está comprovadamente 'expired'.
--
-- Sem o sweep (expiração 100% via schedule asynq), holder e waitlister do mesmo
-- produto ganham o MESMO expires_at no finalize → duas tasks cart.expire disparam
-- concorrentes. Dois guards adicionais fecham essa corrida:
--   (a) expires_at < now(): um cart com janela ESTENDIDA no futuro (promovido da
--       fila) não pode ser expirado por uma task com snapshot velho — o WHERE
--       relê o valor commitado, então a extensão vence a task antiga (MVCC).
--   (b) NOT EXISTS(...): o ciclo de vida de um cart de waitlister é governado
--       PELA FILA, não pelo próprio timer. Abstém-se enquanto o item está
--       'waiting' (na fila) OU 'notified' dentro da janela de promoção ainda
--       vigente (wi.expires_at > now(), gravada ATOMICAMENTE no claim). Isso
--       cobre a sub-janela entre o claim (waiting→notified) e o lock do promotor:
--       no instante em que o item vira 'notified' sua janela já é futura, então
--       a task do próprio waitlister se abstém — nunca deixa um cart
--       notified+expired segurando estoque vazado. Um 'notified' com janela já
--       VENCIDA (não pagou no prazo estendido) volta a ser elegível → expira.
-- 0 rows → não-elegível; o caller (ExpireCartAndReleaseStock) trata como skip.
UPDATE carts
SET status = 'expired', cancelled_reason = 'expired'
WHERE carts.id = $1
  AND status IN ('active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
  AND NOT never_expires   -- VIP: carrinho eterno nunca expira (defesa explícita; expires_at NULL já barraria)
  AND expires_at < now()
  AND NOT EXISTS (
      SELECT 1 FROM waitlist_items wi
      WHERE wi.cart_id = carts.id
        AND (wi.status = 'waiting'
             OR (wi.status = 'notified' AND wi.expires_at > now()))
  )
RETURNING *;

-- name: CancelCart :one
-- Cancelamento MANUAL pelo lojista (LIV-84). Mesmo desenho guard-first do
-- ExpireCart: o guard vive DENTRO do UPDATE para fechar a corrida com o webhook
-- de pagamento — se o cliente pagou no intervalo, 0 rows retornam e o caller
-- ABORTA sem devolver estoque nem cancelar nada no ERP (o pagamento vence).
--
-- Diferenças propositais em relação ao ExpireCart:
--   • sem guard de expires_at: a intenção do lojista não espera prazo nenhum;
--   • sem guard de waitlist: o cart morre por decisão humana, então os itens em
--     fila DESTE cart são cancelados junto (CancelWaitlistItemsByCart) em vez de
--     manterem o cart vivo.
-- 'expired'/'cancelled' não são reelegíveis → o flip é idempotente.
UPDATE carts
SET status = 'cancelled', cancelled_reason = 'store_cancelled'
WHERE carts.id = $1
  AND status IN ('active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
RETURNING *;

-- name: RestoreCancelledCartAsPaid :one
-- O outro lado da corrida cancelamento × pagamento: o webhook confirmou o
-- pagamento DEPOIS que o lojista cancelou (PIX pago com o QR já aberto, cartão
-- em análise, webhook atrasado). Regra de negócio: **pagamento vence** — o
-- pedido volta e consta como PAGO, nunca como cancelado.
--
-- Restaura o cart para 'checkout' + pago e zera as colunas de ERP para que a
-- finalização pós-pagamento crie um pedido de venda LIMPO no Tiny (o
-- cancelamento já cancelou/estornou o anterior). O estoque local retomado fica
-- por conta do caller, na MESMA transação.
-- Guard: só restaura cart cancelado PELO LOJISTA e ainda não pago — um cart
-- expirado, bloqueado por handle ou já pago não entra por aqui.
WITH pago AS (
    UPDATE carts c
    SET status              = 'checkout',
        cancelled_reason    = NULL,
        payment_status      = $2,
        checkout_id         = $3,
        paid_at             = $4,
        payment_method      = $5,
        expires_at          = NULL,
        -- Carimbo do caso para o histórico do pedido e para o aviso no sino do
        -- painel: sem ele o lojista descobriria por acidente que vendeu algo que
        -- julgava cancelado.
        cancellation_reverted_at = now(),
        erp_order_state     = 'none',
        external_order_id   = NULL,
        erp_stock_launched  = FALSE,
        erp_op_started_at   = NULL,
        paid_amount_cents   = c.paid_amount_cents + cart_unpaid_total_cents(c.id)
    WHERE c.id = $1
      AND c.status = 'cancelled'
      AND c.cancelled_reason = 'store_cancelled'
      AND (c.payment_status IS NULL OR c.payment_status NOT IN ('paid', 'refunded'))
    RETURNING *
), carimbo AS (
    UPDATE cart_items ci
    SET paid_quantity = ci.quantity
    WHERE ci.cart_id = (SELECT p.id FROM pago p WHERE p.payment_status = 'paid')
      AND ci.paid_quantity < ci.quantity
    RETURNING 1
)
SELECT * FROM pago;

-- name: UpdateCartPayment :one
-- $3 = payment-provider ID (MP/Pagar.me). Goes to checkout_id, not
-- external_order_id — the latter is reserved for the ERP (Tiny) order ID
-- and is the idempotency key for finalizeCartERPOrder.
-- Guard (must-fix da corrida com o worker de expiração): recusa marcar
-- pagamento em cart já 'expired'/'cancelled' — 0 rows sinalizam ao caller que
-- não deve finalizar (evita o estado inconsistente pago+expirado e o cancel de
-- pedido ERP de venda paga). Ao pagar, neutraliza expires_at para que o worker
-- nunca reselcione a venda.

-- Carimbo do que ESTE pagamento cobre.
--
-- O `::varchar` nas comparações não é enfeite: sem ele o Postgres deduz `text`
-- pelo literal e `character varying` pela coluna, e recusa a instrução inteira
-- com "inconsistent types deduced for parameter $2" sempre que o cliente deixa
-- os tipos por inferir. A forma antiga tinha a mesma fragilidade latente e só
-- escapava porque o driver mandava o OID.
--
-- O carrinho não morre mais no pagamento: o lojista que junta compras soma o
-- pedido de quinta no de segunda e manda uma caixa só. Para isso o carrinho tem
-- de saber, unidade a unidade, o que o dinheiro que entrou já cobriu — o que
-- sobrar sem carimbo é o "falta pagar". Os dois UPDATE veem a MESMA fotografia,
-- então
-- as unidades que a soma contou são exatamente as que o carimbo marcou.
WITH inedito AS (
    -- Reentrega do webhook não é segundo pagamento. O ERP e o gateway
    -- reentregam o mesmo aviso até dez vezes, e o que separa uma cobrança nova
    -- de um eco é o id do gateway.
    SELECT NOT EXISTS (
        SELECT 1 FROM cart_payments cp WHERE cp.cart_id = $1 AND cp.checkout_id = $3
    ) AS primeira_vez
), coberto AS (
    -- O BRUTO que esta cobrança liquida: as unidades ainda não pagas, a preço
    -- cheio, mais o frete. É contra este número que o valor cobrado revela o
    -- desconto.
    SELECT cart_unpaid_total_cents($1) + COALESCE(
        (SELECT c.shipping_cost_cents FROM carts c WHERE c.id = $1), 0) AS bruto
), pago AS (
    UPDATE carts c
    SET payment_status = $2, checkout_id = $3, paid_at = $4, payment_method = $5,
        expires_at = CASE WHEN $2::varchar = 'paid' THEN NULL ELSE expires_at END,
        paid_amount_cents = CASE
            WHEN $2::varchar = 'paid' AND (SELECT primeira_vez FROM inedito)
            THEN c.paid_amount_cents + sqlc.arg(amount_cents)::bigint
            ELSE c.paid_amount_cents END
    WHERE c.id = $1
      AND c.status NOT IN ('expired', 'cancelled')
    RETURNING *
), carimbo AS (
    UPDATE cart_items ci
    SET paid_quantity = ci.quantity
    -- A condição "é pagamento?" vem da LINHA que acabou de ser escrita, não do
    -- parâmetro. Reusar $2 aqui faz o Postgres recusar a instrução inteira
    -- ("inconsistent types deduced for parameter $2"), porque ele já o deduziu
    -- como varchar no SET acima — e ler o resultado é mais honesto de qualquer
    -- forma: carimba quando a linha gravada diz que está paga.
    WHERE ci.cart_id = (SELECT p.id FROM pago p WHERE p.payment_status = 'paid')
      AND ci.paid_quantity < ci.quantity
    RETURNING 1
), livro AS (
    -- Uma linha por cobrança. É o que permite o pedido no ERP dizer "R$ 40 pagos
    -- em 18/08, R$ 105 em 22/08" em vez de um total mudo.
    INSERT INTO cart_payments (cart_id, amount_cents, gross_covered_cents, method, checkout_id, paid_at)
    SELECT p.id, sqlc.arg(amount_cents)::bigint, (SELECT bruto FROM coberto),
           NULLIF($5, ''), $3, COALESCE($4, NOW())
    FROM pago p WHERE p.payment_status = 'paid' AND $3 <> ''
    ON CONFLICT (cart_id, checkout_id) DO NOTHING
    RETURNING 1
)
SELECT * FROM pago;

-- name: UpdateCartNotifyStatus :one
UPDATE carts
SET notify_status = $2, notify_error = $3, notified_at = $4
WHERE id = $1
RETURNING *;

-- name: ListCartsByEvent :many
SELECT * FROM carts WHERE event_id = $1 ORDER BY created_at;

-- name: CreateCartItem :one
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity, session_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpsertCartItem :one
-- Adds quantity to existing cart item or creates new one
-- waitlisted_quantity is added to existing (not replaced)
-- session_id is first-touch: kept from the original add (COALESCE), so
-- re-adds in later sessions accumulate quantity under the first session.
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity, session_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (cart_id, product_id)
DO UPDATE SET quantity = cart_items.quantity + EXCLUDED.quantity,
             waitlisted_quantity = cart_items.waitlisted_quantity + EXCLUDED.waitlisted_quantity,
             session_id = COALESCE(cart_items.session_id, EXCLUDED.session_id)
RETURNING *;

-- name: InsertCartItemEvent :exec
-- Registra uma ADIÇÃO no log de atribuição (RN-12). Vai no mesmo tx do
-- UpsertCartItem: se um falhar, nenhum grava, e o invariante
-- SUM(log.quantity) >= cart_items.quantity nunca é violado por gravação
-- parcial. Só adições entram — remoção é tratada na alocação do selamento.
INSERT INTO cart_item_events (cart_id, product_id, session_id, quantity, unit_price)
VALUES ($1, $2, $3, $4, $5);

-- name: ListCartItemEventsForCart :many
-- O log de adições de um carrinho, em ordem cronológica. É o que o selamento
-- percorre para repartir a quantidade final entre as sessões (RN-29). O id
-- desempata adições gravadas no mesmo instante, tornando a ordem determinística.
SELECT product_id, session_id, quantity, unit_price, created_at
FROM cart_item_events
WHERE cart_id = $1
ORDER BY product_id, created_at, id;

-- name: ListCartItems :many
SELECT ci.*, p.name AS product_name, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1;

-- SessionAttributionByEvent foi REMOVIDA (Fatia 5). Nunca teve chamador Go — e
-- não era aproveitável, por três motivos que a métrica em dois níveis proíbe:
--   1. chaveava por cart_items.session_id, que é FIRST-TOUCH (o COALESCE do
--      UpsertCartItem): creditava à segunda o que foi vendido na quarta;
--   2. subtraía waitlisted_quantity, enquanto a receita do evento
--      (cart_product_total_cents / orders.total_cents) usa quantity CHEIO — a
--      soma das sessões daria SEMPRE menos que o total do evento;
--   3. recalculava dos cart_items vivos filtrando payment_status, em vez de ler
--      o congelado (RN-14).
-- Quem responde "quanto a transmissão de terça faturou" agora é o par
-- ListSessionConfirmedRevenueByEvent (confirmado, de order_items) +
-- ListOpenCartItemsByEvent/ListCartItemEventsByEvent (projetado, alocado sobre
-- o log pelo MESMO AllocateBySession do selamento).

-- name: FinalizeCartsByEvent :many
-- RN-06: ao encerrar o EVENTO, o carrinho sai de 'active' ("pode pagar, sem
-- prazo") para 'checkout' ("prazo correndo") e ganha expires_at. Encerrar uma
-- SESSÃO não passa por aqui — sessão não mexe em carrinho.
--
-- O carrinho PAGO não transiciona (A10). Antes, o filtro de payment_status
-- incidia só no CASE do expires_at, nunca no WHERE: o carrinho pago virava
-- 'checkout' — que no vocabulário novo significa "prazo correndo" — e ainda
-- gerava um cart.checkout_armed inútil, que virava um ScheduleExpiry no-op.
-- Com a decisão 7 (pagar durante o evento), isso deixou de ser detalhe.
--
-- O prazo NÃO é resolvido aqui: chega pronto em $2, vindo de
-- GetEventCartSettings — a fonte única que já aplica a RN-34 (curto x
-- estendido, conforme close_cart_on_event_end) e o fallback para a loja. O
-- COALESCE inline que existia aqui era a terceira cópia da mesma regra.
--
-- QUEM ESTÁ NA FILA GANHA O PRAZO EXTRA DO EVENTO.
--
-- Todo carrinho recebia o MESMO expires_at — um UPDATE, um now() — então os
-- três vencidos em 04/08 tinham 18:38:51.703178 idêntico ao microssegundo. Quem
-- esperava um produto morria no mesmo instante de quem o segurava, e a promoção
-- da fila não tinha intervalo nenhum para acontecer: as três linhas terminaram
-- 'expired' com notified_at NULL. Ninguém foi avisado.
--
-- O extra é `waitlist_notified_ttl_minutes` do EVENTO — a mesma configuração
-- que o lojista já preenche para responder "quanto tempo A MAIS quem espera
-- tem". Não é número novo nem regra nova: é a regra dele, aplicada onde
-- finalmente importa. Sem isso ela só valia depois da promoção, e a promoção
-- nunca chegava.
--
-- O critério é ter item AGUARDANDO ou PROMOVIDO com janela viva. Item já
-- 'expired' ou 'fulfilled' não estende nada — quem não espera mais não precisa
-- de prazo maior.
--
-- Retorna os ids finalizados para emitir cart.checkout_armed por carrinho.
UPDATE carts c
SET status = 'checkout',
    expires_at = now() + make_interval(mins =>
        sqlc.arg(expiration_minutes)::int
        + CASE WHEN EXISTS (
              SELECT 1 FROM waitlist_items wi
              WHERE wi.cart_id = c.id
                AND (wi.status = 'waiting'
                     OR (wi.status = 'notified' AND wi.expires_at > now()))
          ) THEN sqlc.arg(waitlist_extra_minutes)::int ELSE 0 END)
WHERE c.event_id = $1
  AND c.status = 'active'
  AND c.payment_status IS DISTINCT FROM 'paid'
  AND NOT c.never_expires   -- VIP: carrinho eterno nunca ganha prazo no fechamento
RETURNING c.id;

-- name: ShiftOpenCartExpirations :many
-- Propagação da edição de prazo do evento para quem JÁ está com o relógio
-- correndo: desloca expires_at pelo delta entre o prazo efetivo novo e o
-- antigo. DESLOCA, não recalcula — recalcular do zero apagaria as extensões
-- individuais (prazo extra da fila no finalize, GREATEST do reopen RN-10).
--
-- Quem fica de fora, e por quê:
--   • expires_at IS NULL — RN-04: evento ativo não tem relógio; o valor novo
--     passa a valer sozinho no fechamento (FinalizeCartsByEvent lê da fonte
--     única GetEventCartSettings);
--   • pago — A10: pagamento neutraliza o prazo, nada a deslocar;
--   • terminal (expired/cancelled) — o desfecho já aconteceu; "reviver" um
--     carrinho expirado por edição de configuração seria decisão de negócio
--     nova, não propagação.
-- O deslocamento pode cair no passado (lojista ENCURTOU dias depois): correto —
-- o cart.expire re-armado dispara na hora e o guard decide, como sempre.
UPDATE carts
SET expires_at = expires_at + make_interval(mins => sqlc.arg(delta_minutes)::int)
WHERE event_id = $1
  AND status IN ('active', 'checkout')
  AND payment_status IS DISTINCT FROM 'paid'
  AND expires_at IS NOT NULL
RETURNING id;

-- name: CountCartsByEvent :one
SELECT COUNT(*)::int as count FROM carts WHERE event_id = $1 AND status = 'active';

-- name: UpdateCartItem :one
UPDATE cart_items
SET quantity = $2
WHERE id = $1
RETURNING *;

-- name: DeleteCartItem :exec
DELETE FROM cart_items WHERE id = $1;

-- name: GetCartItem :one
SELECT * FROM cart_items WHERE id = $1;

-- =============================================================================
-- EVENT DETAILS - Stats and Cart Listing
-- =============================================================================

-- name: GetEventStats :one
-- Returns stats for an event: comments, carts, revenue, products sold, funnel metrics
--
-- ⓥ ATRIBUIÇÃO POR EVENTO DE ORIGEM (Clientes VIP / F3). As métricas de VENDA
-- (total_products_sold, confirmed_revenue) e a quebra por transmissão
-- (ListSessionConfirmedRevenueByEvent, ListProductsByEvent) creditam cada
-- unidade ao evento da SESSÃO que a vendeu (order_items.session_id ->
-- live_sessions.event_id), não ao evento âncora do carrinho. Para carrinho
-- normal (1 evento) o COALESCE cai em o.event_id e o número é IDÊNTICO ao antigo
-- — provado em produção: 0 order_items com ls.event_id != o.event_id e
-- SUM(oi.quantity*oi.unit_price)==orders.total_cents para todo pedido pago. Só o
-- carrinho VIP cross-evento se reparte. Os CONTADORES de funil (total_carts,
-- open_carts, checkout_carts, paid_carts) e o PROJETADO seguem por carts.event_id
-- (âncora): repartir carrinho aberto por evento exige AllocateBySession e fica
-- para a próxima fatia; a venda confirmada é a que o cliente pediu separada.
--
-- ⚠️ O PREDICADO DO CARRINHO ABERTO ("ainda em aberto") aparece aqui em
-- open_carts e projected_revenue e é repetido, ao pé da letra, em
-- ListOpenCartItemsByEvent e ListCartItemEventsByEvent. É esse par que faz a
-- soma das transmissões fechar com o total do evento; mudar um lado só quebra
-- o invariante da Fatia 5 em silêncio.
--
-- Ele exclui o carrinho JÁ PAGO. O carrinho não muda de status ao ser pago
-- (UpdateCartPayment mexe só em payment_status), então ele fica em 'checkout'
-- para sempre — e sem esta exclusão o mesmo dinheiro era contado duas vezes na
-- mesma tela: uma em confirmed_revenue (a venda) e outra em projected_revenue
-- ("expectativa"). A tooltip do painel promete o contrário ("carrinhos abertos
-- que ainda não foram pagos"). 'refunded' entra na exclusão pelo mesmo motivo:
-- estorno não devolve o carrinho para a expectativa de venda.
-- COALESCE porque payment_status é NULLable desde a 000001 e `NULL NOT IN (…)`
-- é NULL — sem ele o carrinho antigo sumiria do projetado.
SELECT
    -- Funnel metrics
    COALESCE((SELECT SUM(ls.total_comments) FROM live_sessions ls WHERE ls.event_id = $1), 0)::int AS total_comments,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1), 0)::int AS total_carts,
    COALESCE((
        SELECT COUNT(*) FROM carts ct
        WHERE ct.status IN ('active', 'checkout')
          AND COALESCE(ct.payment_status, '') NOT IN ('paid', 'refunded')
          AND (
            ct.event_id = $1
            OR (ct.never_expires AND EXISTS (
                SELECT 1 FROM cart_item_events cie2
                JOIN live_sessions ls2 ON ls2.id = cie2.session_id
                WHERE cie2.cart_id = ct.id AND ls2.event_id = $1))
          )
    ), 0)::int AS open_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND (ct.status = 'checkout' OR ct.checkout_url IS NOT NULL)), 0)::int AS checkout_carts,
    COALESCE((SELECT COUNT(*) FROM carts ct WHERE ct.event_id = $1 AND ct.payment_status = 'paid'), 0)::int AS paid_carts,
    -- Product metrics — "unidades vendidas" sai de order_items de pedido PAGO,
    -- a MESMA fonte de ListProductsByEvent (Top Produtos) e de
    -- ListSessionConfirmedRevenueByEvent (a quebra por transmissão). Antes era
    -- SUM(cart_items.quantity) de todo carrinho não expirado, ou seja: carrinho
    -- aberto, pendente, cancelado e até a fila de espera contavam como venda —
    -- três definições de "vendido" na mesma tela, e a tooltip prometia a mais
    -- restrita ("unidades efetivamente vendidas").
    COALESCE((
        SELECT SUM(oi.quantity)
        FROM order_items oi
        JOIN orders o ON o.id = oi.order_id
        LEFT JOIN live_sessions ls ON ls.id = oi.session_id
        WHERE COALESCE(ls.event_id, o.event_id) = $1 AND o.status = 'paid'
    ), 0)::int AS total_products_sold,
    -- Revenue metrics (Grupo C: projected stays cart-based; Grupo A: confirmed reads from sealed orders)
    COALESCE((
        SELECT SUM(cart_product_total_cents(ct.id))
        FROM carts ct
        WHERE ct.event_id = $1
          AND ct.status IN ('active', 'checkout')
          AND COALESCE(ct.payment_status, '') NOT IN ('paid', 'refunded')
    ), 0)::bigint AS projected_revenue,
    COALESCE((
        SELECT SUM(oi.quantity * oi.unit_price)
        FROM order_items oi
        JOIN orders o ON o.id = oi.order_id
        LEFT JOIN live_sessions ls ON ls.id = oi.session_id
        WHERE COALESCE(ls.event_id, o.event_id) = $1 AND o.status = 'paid'
    ), 0)::bigint AS confirmed_revenue;

-- name: ListCartsWithTotalByEvent :many
-- Returns carts for an event with total value and item count (available vs waitlisted)
SELECT
    c.id,
    c.event_id,
    c.session_id,
    c.platform_user_id,
    c.platform_handle,
    c.token,
    c.status,
    c.payment_status,
    c.created_at,
    c.expires_at,
    COALESCE(SUM((ci.quantity - ci.waitlisted_quantity) * ci.unit_price), 0)::bigint AS total_value,
    COALESCE(SUM(ci.quantity), 0)::int AS total_items,
    COALESCE(SUM(ci.quantity - ci.waitlisted_quantity), 0)::int AS available_items,
    COALESCE(SUM(ci.waitlisted_quantity), 0)::int AS waitlisted_items
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id
WHERE c.event_id = $1
GROUP BY c.id
ORDER BY c.created_at DESC;

-- name: ListProductsByEvent :many
-- Returns products sold in an event with quantity and revenue (paid orders only)
SELECT
    p.id,
    p.name,
    p.image_url,
    p.keyword,
    COALESCE(SUM(oi.quantity), 0)::int AS total_quantity,
    COALESCE(SUM(oi.quantity * oi.unit_price), 0)::bigint AS total_revenue
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
JOIN products p ON p.id = oi.product_id
LEFT JOIN live_sessions ls ON ls.id = oi.session_id
WHERE COALESCE(ls.event_id, o.event_id) = $1 AND o.status = 'paid'
GROUP BY p.id, p.name, p.image_url, p.keyword
ORDER BY total_quantity DESC;

-- =============================================================================
-- MÉTRICA EM DOIS NÍVEIS — por SESSÃO e agregada por EVENTO (Fatia 5)
--
-- GetSessionStats foi REMOVIDA. Ela agrupava por carts.session_id, que é a
-- sessão em que o carrinho NASCEU (gravada uma única vez no CreateCart e nunca
-- mais tocada). Com o carrinho unificado da campanha (RN-02) um carrinho vive a
-- semana inteira, então o carrinho INTEIRO — inclusive o que foi comprado na
-- quinta — era creditado à transmissão de segunda. Além disso ela era chamada
-- uma vez POR SESSÃO dentro do laço de GetEventWithSessions (N+1) para alimentar
-- campos que a tela sequer renderizava.
--
-- No lugar entram três queries de EVENTO (uma chamada, não N):
--   • confirmado  ← order_items.session_id, o congelado do pedido pago;
--   • projetado   ← cart_items (quantidade final) + cart_item_events (de onde
--                   veio cada unidade), repartido em Go por AllocateBySession —
--                   a MESMA função que o selamento usa. Duas implementações da
--                   mesma regra seria exatamente a divergência que a métrica em
--                   dois níveis não pode ter.
--
-- O INVARIANTE (inegociável): a soma das sessões bate exatamente com o total do
-- evento em GetEventStats. Por isso cada query abaixo repete, ao pé da letra, o
-- predicado do seu par lá em cima — e por isso o balde session_id IS NULL ("sem
-- transmissão") é devolvido junto: sem ele a soma não fecha.
-- =============================================================================

-- name: ListSessionConfirmedRevenueByEvent :many
-- Receita CONFIRMADA por transmissão: o congelado do pedido, repartido pela
-- sessão que vendeu cada unidade (RN-13). Uma linha por sessão + a linha
-- session_id NULL (adição sem transmissão, ou pedido anterior ao log).
--
-- Fecha com GetEventStats.confirmed_revenue (SUM(orders.total_cents)) por
-- construção: o selamento grava order_items com o unit_price do carrinho e
-- AllocateBySession garante SUM(order_items.quantity) == cart_items.quantity,
-- logo SUM(quantity*unit_price) == cart_product_total_cents == total_cents.
SELECT
    oi.session_id,
    COALESCE(SUM(oi.quantity), 0)::int                    AS sold_units,
    COALESCE(SUM(oi.quantity * oi.unit_price), 0)::bigint AS revenue_cents,
    COUNT(DISTINCT o.cart_id)::int                        AS paid_carts
FROM order_items oi
JOIN orders o ON o.id = oi.order_id
LEFT JOIN live_sessions ls ON ls.id = oi.session_id
WHERE COALESCE(ls.event_id, o.event_id) = $1 AND o.status = 'paid'
GROUP BY oi.session_id;

-- name: ListOpenCartItemsByEvent :many
-- A quantidade FINAL de cada produto nos carrinhos que entram na projeção.
--
-- O predicado e a quantidade CHEIA são cópia literal de
-- GetEventStats.projected_revenue — inclusive a exclusão do carrinho já pago
-- ou estornado, que entrou junto nos dois lados. O que a Fatia 5 garante é que
-- os dois níveis usem O MESMO predicado; qualquer mudança aqui tem de acontecer
-- lá em cima na mesma edição, senão a soma das sessões deixa de fechar.
SELECT
    ci.cart_id,
    ci.product_id,
    ci.quantity,
    ci.unit_price,
    ct.event_id AS cart_event_id
FROM carts ct
JOIN cart_items ci ON ci.cart_id = ct.id
WHERE ct.status IN ('active', 'checkout')
  AND COALESCE(ct.payment_status, '') NOT IN ('paid', 'refunded')
  AND (
    ct.event_id = $1
    -- Carrinho VIP eterno ancorado em OUTRO evento, mas que vendeu neste: entra
    -- inteiro na projeção; ProjectBySessionForEvent fica só com a fatia de $1.
    OR (ct.never_expires AND EXISTS (
        SELECT 1 FROM cart_item_events cie2
        JOIN live_sessions ls2 ON ls2.id = cie2.session_id
        WHERE cie2.cart_id = ct.id AND ls2.event_id = $1))
  )
ORDER BY ci.cart_id, ci.product_id;

-- name: ListCartItemEventsByEvent :many
-- O log de adições dos MESMOS carrinhos da query acima, na ordem que
-- AllocateBySession exige (cronológica por produto, id desempatando).
-- Ele diz de onde veio cada unidade; a quantidade final continua sendo a de
-- cart_items.
SELECT
    cie.cart_id,
    cie.product_id,
    cie.session_id,
    cie.quantity,
    cie.unit_price,
    ls.event_id AS session_event_id
FROM cart_item_events cie
JOIN carts ct ON ct.id = cie.cart_id
LEFT JOIN live_sessions ls ON ls.id = cie.session_id
WHERE ct.status IN ('active', 'checkout')
  AND COALESCE(ct.payment_status, '') NOT IN ('paid', 'refunded')
  AND (
    ct.event_id = $1
    OR (ct.never_expires AND EXISTS (
        SELECT 1 FROM cart_item_events cie2
        JOIN live_sessions ls2 ON ls2.id = cie2.session_id
        WHERE cie2.cart_id = ct.id AND ls2.event_id = $1))
  )
ORDER BY cie.cart_id, cie.product_id, cie.created_at, cie.id;

-- =============================================================================
-- ERP SYNC & EXPIRATION
-- =============================================================================

-- name: UpdateCartExternalOrderID :exec
UPDATE carts SET external_order_id = $2 WHERE id = $1;

-- Fatia 10-b: as queries pós-venda de finalização/NF do ERP saíram daqui. A
-- Fatia 11b tornou order_payments a fonte autoritativa e repontou os wrappers Go
-- (MarkCartERPFinalisation*/GetCartERPFinalisationStatus/Get|UpsertCartERPInvoice
-- em repository.go) para as queries Order* em order_write.sql. As colunas
-- pós-venda correspondentes foram dropadas do cart (migration 000101). O que fica
-- no cart é só reserva (erp_order_state/erp_stock_launched/erp_op_started_at) e
-- external_order_id.

-- name: FindCartByExternalOrderID :one
-- Resolve a cart from a Tiny pedido id (or any ERP external order id) for a
-- given store. Used by the Tiny webhook handler when only the pedido id is
-- available on the payload.
SELECT c.*, le.store_id
FROM carts c
JOIN live_events le ON le.id = c.event_id
WHERE c.external_order_id = $1
  AND le.store_id = $2
ORDER BY c.created_at DESC
LIMIT 1;

-- name: ListNonWaitlistedCartItems :many
-- Returns cart items that have available (non-waitlisted) quantity, with product external_id for ERP sync
-- Returns available_quantity = quantity - waitlisted_quantity
SELECT ci.id, ci.cart_id, ci.product_id,
       (ci.quantity - ci.waitlisted_quantity) AS quantity,
       ci.unit_price, ci.waitlisted_quantity,
       p.name AS product_name, p.external_id AS product_external_id,
       p.keyword AS product_keyword, p.image_url AS product_image_url
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1 AND ci.quantity > ci.waitlisted_quantity;

-- name: ListExpiredCartsByEventAndProduct :many
-- Returns expired carts for a specific event that contain a specific product (with available qty)
SELECT DISTINCT c.*, le.store_id
FROM carts c
JOIN live_events le ON le.id = c.event_id
JOIN cart_items ci ON ci.cart_id = c.id
WHERE c.event_id = $1
  AND c.status = 'active'
  AND c.expires_at IS NOT NULL
  AND c.expires_at < now()
  AND ci.product_id = $2
  AND ci.quantity > ci.waitlisted_quantity;

-- name: DeleteCartItemByCartAndProduct :exec
DELETE FROM cart_items WHERE cart_id = $1 AND product_id = $2;

-- name: DeleteCartItemsByCart :exec
-- Apaga todos os itens de um cart. Usado ao reabrir um cart expirado/cancelado
-- para reuso (os itens antigos já tiveram o estoque devolvido pela expiração).
DELETE FROM cart_items WHERE cart_id = $1;

-- name: DecrementCartItemQuantity :one
-- Subtrai @delta da quantidade do item; se zerar, retorna a row mas a
-- limpeza fica a cargo do caller (delete em outro statement). Não permite
-- ir negativo.
UPDATE cart_items
SET quantity = GREATEST(quantity - @delta::int, 0)
WHERE cart_id = $1 AND product_id = $2
RETURNING quantity;

-- name: UpdateCartItemWaitlistedQuantity :exec
UPDATE cart_items SET waitlisted_quantity = $3 WHERE cart_id = $1 AND product_id = $2;

-- name: GetCartByEventAndUserForUpdate :one
-- O carrinho ABERTO, travado para escrita. Esta é a do caminho quente
-- (GetOrCreateCart). Mesmo filtro do GetCartByEventAndUser: sem ele, o FOR
-- UPDATE poderia travar a linha errada — por exemplo a de um carrinho já pago.
SELECT * FROM carts
WHERE event_id = $1 AND platform_user_id = $2
  AND status IN ('pending', 'active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
ORDER BY created_at DESC
LIMIT 1
FOR UPDATE;

-- name: GetProductQuantityInUserCart :one
-- Quantidade de um produto no carrinho ABERTO do comprador neste evento.
--
-- Alimenta o teto cart_max_quantity_per_item. Sem o filtro, com mais de um
-- carrinho a leitura vira não-determinística e o teto passa a valer sobre um
-- carrinho arbitrário — inclusive um já pago, o que bloquearia compra legítima.
SELECT COALESCE(ci.quantity, 0)::INT AS quantity
FROM carts c
LEFT JOIN cart_items ci ON ci.cart_id = c.id AND ci.product_id = $3
WHERE c.event_id = $1 AND c.platform_user_id = $2
  AND c.status IN ('pending', 'active', 'checkout')
  AND (c.payment_status IS NULL OR c.payment_status NOT IN ('paid', 'refunded'))
ORDER BY c.created_at DESC
LIMIT 1;

-- =============================================================================
-- PUBLIC CHECKOUT - Cart page for customers
-- =============================================================================

-- name: GetCartByTokenWithDetails :one
-- Returns cart with event info for public checkout page
SELECT
    c.id,
    c.event_id,
    c.platform_user_id,
    c.platform_handle,
    c.token,
    c.status,
    c.checkout_url,
    c.checkout_id,
    c.checkout_expires_at,
    c.customer_email,
    c.payment_status,
    c.paid_at,
    c.payment_integration_id,
    c.created_at,
    c.expires_at,
    c.initial_snapshot_taken_at,
    c.initial_subtotal_cents,
    c.coupon_id,
    c.coupon_code,
    c.coupon_discount_cents,
    c.cancelled_reason,
    le.title AS event_title,
    le.store_id,
    s.slug AS store_slug,
    s.name AS store_name,
    s.logo_url AS store_logo_url,
    s.cart_allow_edit AS allow_edit,
    s.cart_max_quantity_per_item AS max_quantity_per_item,
    -- Parcela minima da loja: a regra de quantas parcelas cabem no total mora no
    -- servidor (checkout.MaxInstallmentsFor), e ela precisa deste numero tanto
    -- para montar a lista que o comprador ve quanto para recusar um POST que
    -- peca mais parcelas do que a loja permite.
    s.min_installment_cents AS min_installment_cents
FROM carts c
JOIN live_events le ON le.id = c.event_id
JOIN stores s ON s.id = le.store_id
WHERE c.token = $1;

-- name: BindCartPaymentIntegration :exec
-- Pins the cart to a specific payment integration the moment GetCheckoutConfig
-- successfully instantiates a provider. Subsequent ProcessCardPayment /
-- GeneratePixPayment calls use this binding instead of re-resolving the
-- store's primary, so a FE that tokenized with provider X always reaches
-- provider X server-side (card tokens are provider-bound).
UPDATE carts
SET payment_integration_id = $2
WHERE id = $1;

-- name: ListCartItemsForCheckout :many
-- Returns cart items with product details for checkout page.
-- product_stock is exposed so the public checkout can disable the "+" button
-- when the buyer is about to exceed the available stock for the SKU.
SELECT
    ci.id,
    ci.cart_id,
    ci.product_id,
    ci.quantity,
    ci.unit_price,
    ci.waitlisted_quantity,
    p.name AS product_name,
    p.image_url AS product_image_url,
    p.keyword AS product_keyword,
    p.stock AS product_stock
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
WHERE ci.cart_id = $1
ORDER BY ci.id;

-- name: UpdateCartCustomerEmail :one
UPDATE carts
SET customer_email = $2
WHERE token = $1
RETURNING *;

-- name: UpdateCartCustomerCheckout :one
-- Persists full customer + shipping data entered in the checkout form.
-- Called right before a card/pix payment request so the webhook handler
-- (and ERP order creation on paid) has complete info regardless of provider.
UPDATE carts
SET customer_email    = $2,
    customer_name     = $3,
    customer_document = $4,
    customer_phone    = $5,
    shipping_address  = $6,
    whatsapp_consent  = $7,
    whatsapp_consent_at = CASE
      WHEN $7 = TRUE AND whatsapp_consent = FALSE THEN NOW()
      ELSE whatsapp_consent_at
    END
WHERE id = $1
RETURNING *;

-- name: UpdateCartCheckoutInfo :one
-- Updates checkout URL and ID after generating payment link
UPDATE carts
SET checkout_url = $2, checkout_id = $3, checkout_expires_at = $4
WHERE id = $1
RETURNING *;

-- name: GetCartByCheckoutID :one
-- Used by webhook to find cart when payment is confirmed
SELECT * FROM carts WHERE checkout_id = $1;

-- name: UpdateCartPaymentByCheckoutID :one
-- Updates payment status when webhook confirms payment

-- Carimbo do que ESTE pagamento cobre.
--
-- O carrinho não morre mais no pagamento: o lojista que junta compras soma o
-- pedido de quinta no de segunda e manda uma caixa só. Para isso o carrinho tem
-- de saber, unidade a unidade, o que o dinheiro que entrou já cobriu — o que
-- sobrar sem carimbo é o "falta pagar". Os dois UPDATE veem a MESMA fotografia,
-- então
-- as unidades que a soma contou são exatamente as que o carimbo marcou.
WITH pago AS (
    UPDATE carts c
    SET payment_status = $2, paid_at = $3,
        paid_amount_cents = CASE WHEN $2::varchar = 'paid'
            THEN c.paid_amount_cents + cart_unpaid_total_cents(c.id)
            ELSE c.paid_amount_cents END
    WHERE c.checkout_id = $1
    RETURNING *
), carimbo AS (
    UPDATE cart_items ci
    SET paid_quantity = ci.quantity
    -- A condição "é pagamento?" vem da LINHA que acabou de ser escrita, não do
    -- parâmetro. Reusar $2 aqui faz o Postgres recusar a instrução inteira
    -- ("inconsistent types deduced for parameter $2"), porque ele já o deduziu
    -- como varchar no SET acima — e ler o resultado é mais honesto de qualquer
    -- forma: carimba quando a linha gravada diz que está paga.
    WHERE ci.cart_id = (SELECT p.id FROM pago p WHERE p.payment_status = 'paid')
      AND ci.paid_quantity < ci.quantity
    RETURNING 1
)
SELECT * FROM pago;

-- name: UpdateCartPaymentStatus :one
-- Updates payment status directly by cart ID (for transparent checkout)
-- Uses checkout_id to store the payment ID from the provider.
-- Mesmo guard de UpdateCartPayment: não marca pagamento em cart expirado/
-- cancelado (0 rows = caller não finaliza) e neutraliza expires_at ao pagar.

-- Carimbo do que ESTE pagamento cobre.
--
-- O carrinho não morre mais no pagamento: o lojista que junta compras soma o
-- pedido de quinta no de segunda e manda uma caixa só. Para isso o carrinho tem
-- de saber, unidade a unidade, o que o dinheiro que entrou já cobriu — o que
-- sobrar sem carimbo é o "falta pagar". Os dois UPDATE veem a MESMA fotografia,
-- então
-- as unidades que a soma contou são exatamente as que o carimbo marcou.
WITH pago AS (
    UPDATE carts c
    SET payment_status = $2, checkout_id = $3, paid_at = $4,
        expires_at = CASE WHEN $2::varchar = 'paid' THEN NULL ELSE expires_at END,
        paid_amount_cents = CASE WHEN $2::varchar = 'paid'
            THEN c.paid_amount_cents + cart_unpaid_total_cents(c.id)
            ELSE c.paid_amount_cents END
    WHERE c.id = $1
      AND c.status NOT IN ('expired', 'cancelled')
    RETURNING *
), carimbo AS (
    UPDATE cart_items ci
    SET paid_quantity = ci.quantity
    -- A condição "é pagamento?" vem da LINHA que acabou de ser escrita, não do
    -- parâmetro. Reusar $2 aqui faz o Postgres recusar a instrução inteira
    -- ("inconsistent types deduced for parameter $2"), porque ele já o deduziu
    -- como varchar no SET acima — e ler o resultado é mais honesto de qualquer
    -- forma: carimba quando a linha gravada diz que está paga.
    WHERE ci.cart_id = (SELECT p.id FROM pago p WHERE p.payment_status = 'paid')
      AND ci.paid_quantity < ci.quantity
    RETURNING 1
)
SELECT * FROM pago;

-- name: GetStorePaymentIntegration :one
-- Returns the highest-priority active payment integration for a store.
-- Lower `priority` wins; created_at tie-breaks (oldest first within a tie).
-- See ListStorePaymentIntegrations for the fallback chain.
SELECT i.*
FROM integrations i
WHERE i.store_id = $1
  AND i.type = 'payment'
  AND i.status = 'active'
ORDER BY i.priority ASC, i.created_at ASC
LIMIT 1;

-- name: ListStorePaymentIntegrations :many
-- Returns all active payment integrations for a store in priority order.
-- The checkout layer walks this list as a fallback chain: if the primary
-- can't be instantiated (credentials corrupted, provider unsupported, etc.),
-- the next one is tried.
SELECT i.*
FROM integrations i
WHERE i.store_id = $1
  AND i.type = 'payment'
  AND i.status = 'active'
ORDER BY i.priority ASC, i.created_at ASC;

-- name: HasInFlightFinalisationForProduct :one
-- Defesa em profundidade do backstop de waitlist: TRUE se existe cart pago
-- com finalização ERP em andamento (ou falha recente, ainda retomável via
-- retry) contendo o produto. Enquanto verdadeiro, promoção disparada por
-- webhook de estoque é adiada — a DM de promoção é irreversível.
-- Fatia 11b: o estado de finalização é autoritativo em order_payments (join via
-- Order). COALESCE('pending') cobre o cart pago cuja Order ainda materializa. As
-- colunas de reserva (erp_order_state/erp_op_started_at) seguem no cart.
SELECT EXISTS(
    SELECT 1 FROM carts c
    JOIN cart_items ci ON ci.cart_id = c.id
    LEFT JOIN orders o          ON o.cart_id  = c.id
    LEFT JOIN order_payments op ON op.order_id = o.id
    WHERE ci.product_id = $1
      AND ((c.payment_status = 'paid'
            AND COALESCE(op.erp_finalisation_status, 'pending') <> 'done'
            AND (c.paid_at > now() - interval '30 minutes'
                 OR op.erp_last_attempt_at > now() - interval '30 minutes'))
           -- design C: ciclo de conversão/mutação em voo — adia a promoção
           OR (c.erp_order_state IN ('converting','mutating')
               AND c.erp_op_started_at > now() - interval '30 minutes'))
)::bool AS in_flight;

-- name: GetCartItemAvailableQty :one
-- Unidades não-waitlisted de um item (o que foi decrementado do estoque
-- local no add e deve voltar quando o cart expira).
SELECT (quantity - waitlisted_quantity)::int AS available
FROM cart_items
WHERE cart_id = $1 AND product_id = $2;

-- Fatia 10-b: MarkCartERPFinalisationAttempt (S1 da finalização retomável) saiu
-- daqui — o wrapper Go repontou para MarkOrderERPFinalisationAttempt
-- (order_write.sql) na Fatia 11b e as colunas erp_payment_snapshot/
-- erp_last_attempt_at foram dropadas do cart (migration 000101).

-- ============================================================================
-- DESIGN C — pedido Tiny como reserva a partir da iniciação do pagamento
-- ============================================================================

-- name: TransitionCartERPOrderState :execrows
-- CAS de transição da máquina de estados do pedido-como-reserva. rows=0 quando
-- o estado atual não é o esperado — single-flight de conversão/mutação.
--
-- Resolve para o ANFITRIÃO, igual à leitura do estado, e por um motivo que é a
-- razão de a junção funcionar: a trava é por LINHA de carrinho. Sem resolver,
-- dois carrinhos juntados tomariam duas travas diferentes para o MESMO pedido e
-- poderiam escrever a grade ao mesmo tempo — que é exatamente como se corrompe
-- um pedido no ERP, já que a escrita é substituição da grade inteira.
UPDATE carts
SET erp_order_state = sqlc.arg(to_state)::varchar,
    erp_op_started_at = CASE WHEN sqlc.arg(to_state)::varchar IN ('converting','mutating') THEN now() ELSE erp_op_started_at END
WHERE carts.id = (SELECT COALESCE(j.joined_to_cart_id, j.id) FROM carts j WHERE j.id = sqlc.arg(cart_id))
  AND carts.erp_order_state = sqlc.arg(from_state);

-- name: GetCartERPOrderState :one
-- A situação do pedido e o quanto já foi pago vêm na MESMA linha, de propósito.
-- A situação diz se o pedido ainda recebe item (pago, não faturado) ou se já
-- virou nota; o valor pago é o que separa, nas parcelas do ERP, o que entrou do
-- que falta. Buscá-los à parte custaria duas leituras a mais no caminho mais
-- quente da live.
--
-- Resolve para o ANFITRIÃO quando o carrinho foi juntado a outro: o pedido é
-- dele, e é o estado dele que decide o que pode ser escrito. Sem isto, um
-- carrinho juntado leria o próprio estado — vazio, sem pedido — e tentaria
-- criar um segundo pedido para o mesmo conteúdo.
SELECT c.erp_order_state, c.erp_stock_launched, COALESCE(c.external_order_id,'') AS external_order_id,
       COALESCE(c.erp_order_status,'') AS erp_order_status,
       c.paid_amount_cents,
       COALESCE(c.paid_at, c.created_at) AS paid_at
FROM carts orig
JOIN carts c ON c.id = COALESCE(orig.joined_to_cart_id, orig.id)
WHERE orig.id = $1;

-- name: SetCartERPStockLaunched :exec
UPDATE carts SET erp_stock_launched = $2 WHERE id = $1;

-- name: ListStuckERPOrderOps :many
-- Sweep: conversões/mutações em voo há mais tempo que o limiar — o processo
-- morreu no meio; o sweep reconcilia (adota pedido via marcador ou re-roda o
-- ciclo). NUNCA resetar para 'none' (a chamada em voo pode ter sucedido
-- server-side e o caminho legado criaria pedido duplicado).
SELECT c.id, c.erp_order_state, c.erp_op_started_at, COALESCE(c.external_order_id,'') AS external_order_id,
       le.store_id
FROM carts c
JOIN live_events le ON le.id = c.event_id
WHERE c.erp_order_state IN ('converting','mutating')
  AND c.erp_op_started_at < now() - make_interval(secs => sqlc.arg(older_than_seconds)::int);

-- name: GetCartGMVCents :one
-- GMV de um cart: delega à função canônica cart_product_total_cents (migration 000093).
SELECT cart_product_total_cents(sqlc.arg(cart_id))::bigint AS gmv_cents;

-- name: GetCartCommissionBaseCents :one
-- Base da COMISSÃO (taxa de sucesso): o valor LÍQUIDO dos produtos que o cliente
-- de fato pagou — o bruto menos os descontos (cupom + PIX), SEM frete. A loja
-- recebe o valor com desconto, então a taxa incide sobre isso, não sobre o preço
-- cheio. Para PIX o valor real cobrado está em pix_amount_cents (já com desconto),
-- e tiramos o frete; para cartão/sem PIX é bruto - cupom. Nunca negativo.
SELECT GREATEST(
  CASE
    WHEN c.payment_method = 'pix' AND c.pix_amount_cents IS NOT NULL
      THEN c.pix_amount_cents - COALESCE(c.shipping_cost_cents, 0)
    ELSE cart_product_total_cents(c.id) - c.coupon_discount_cents
  END, 0)::bigint AS base_cents
FROM carts c WHERE c.id = sqlc.arg(cart_id);

-- name: SetCartItemSplitIfUnchanged :execrows
-- Escreve a quantidade TOTAL e a parte em FILA de uma vez, e só se ninguém
-- tiver mexido na linha desde que a lemos.
--
-- Duas correções numa query, porque as duas vivem no mesmo UPDATE:
--
-- 1) TRAVA OTIMISTA. O UpdateCartItem antigo era `SET quantity = $2 WHERE
--    id = $1` — absoluto e sem guarda. Entre a leitura e a escrita, o PATCH do
--    checkout faz uma chamada HTTP ao Tiny que passa pelo limitador de ~1 req/s:
--    a janela dura SEGUNDOS. Em 05/08 dois escritores leram quantity=2 com
--    3.070 ms de diferença; o segundo calculou o delta contra um valor já
--    obsoleto, debitou 2 e a linha só andou 1. Uma unidade sumiu do LiveCart e
--    uma saída a mais foi lançada no Tiny. `AND quantity = @expected_quantity`
--    faz o perdedor afetar ZERO linhas, e o chamador devolve conflito em vez de
--    corromper a conta.
--
-- 2) A PARTE EM FILA ACOMPANHA. `waitlisted_quantity` é a parcela SEM estoque;
--    o que está segurado é `quantity - waitlisted_quantity`. O update antigo
--    mexia só no total, então baixar de 5 (com 3 em fila, 2 segurados) para 2
--    creditava 3 unidades ao estoque quando só 2 haviam sido tiradas — e ainda
--    deixava a linha com disponível NEGATIVO (2 - 3). Escrever as duas colunas
--    juntas é o que mantém a identidade `total = segurado + em fila`.
UPDATE cart_items
SET quantity = @quantity::int,
    waitlisted_quantity = @waitlisted_quantity::int
WHERE id = @id
  AND quantity = @expected_quantity::int;

-- name: FindOpenCartUserIDByHandle :one
-- O platform_user_id do carrinho ABERTO deste comprador no evento, procurado
-- pelo @ em vez do id.
--
-- Existe porque o MESMO comprador chega com DOIS ids diferentes conforme o
-- caminho: o webhook traz `from.self_ig_scoped_id` (o IGSID, único aceito pela
-- API de mensagens como destinatário) e a aresta `/{media}/comments` do polling
-- não devolve esse campo — só o `from.id` cru.
--
-- Em 05/08 isso deu DOIS carrinhos para @englivecart no mesmo evento: um com
-- 1498886768484002 (webhook) e outro com 17841439350112281 (polling). A unique
-- parcial por (evento, platform_user_id) não viu violação nenhuma — os ids são
-- diferentes de verdade. O índice fez o trabalho dele; a entrada é que chegou
-- com duas identidades para a mesma pessoa.
--
-- Só bate em quem já tem carrinho aberto no evento: o @ é estável dentro de uma
-- transmissão e é por ele que o lojista reconhece o comprador.
SELECT platform_user_id FROM carts
WHERE event_id = $1
  AND platform_handle = $2
  AND status IN ('pending', 'active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
ORDER BY created_at ASC
LIMIT 1;

-- name: SetCartPixCharge :exec
-- Guarda a cobranca PIX viva do carrinho: o id que da para cancelar e o valor
-- que o QR na mao do comprador cobra.
UPDATE carts
SET pix_charge_id = sqlc.arg(pix_charge_id),
    pix_amount_cents = sqlc.arg(pix_amount_cents)
WHERE id = sqlc.arg(id);

-- name: ClearCartPixCharge :exec
-- Esquece a cobranca PIX. Chamado depois de cancelar no gateway; separado da
-- escrita para o cancelamento poder falhar sem deixar um id fantasma aqui.
UPDATE carts
SET pix_charge_id = NULL,
    pix_amount_cents = NULL
WHERE id = sqlc.arg(id);

-- name: TakeCartPixCharge :one
-- Le e LIMPA a cobranca viva na mesma instrucao.
--
-- Duas mutacoes simultaneas no mesmo carrinho tentariam cancelar a mesma
-- cobranca duas vezes; quem le aqui e o unico que recebe o id, entao o
-- cancelamento no gateway sai uma vez so.
--
-- O valor devolvido tem de ser o ANTIGO. `RETURNING` num UPDATE entrega a linha
-- DEPOIS da escrita, entao a forma direta devolveria os NULL que acabamos de
-- gravar. A CTE le antes, e o `FOR UPDATE` nela serializa os concorrentes: o
-- segundo a chegar espera, reavalia o predicado com a coluna ja nula, nao
-- encontra linha e sai sem id — que e exatamente o vencedor unico que se quer.
--
-- Evita de proposito o `RETURNING OLD.*` do Postgres 18: a query passaria a
-- depender da versao do servidor, e o custo aqui e uma CTE.
WITH tomada AS (
    SELECT carts.id AS cart_id, carts.pix_charge_id, carts.pix_amount_cents
    FROM carts
    WHERE carts.id = sqlc.arg(id) AND carts.pix_charge_id IS NOT NULL
    FOR UPDATE
)
UPDATE carts c
SET pix_charge_id = NULL,
    pix_amount_cents = NULL
FROM tomada
WHERE c.id = tomada.cart_id
RETURNING tomada.pix_charge_id, tomada.pix_amount_cents;

-- name: CancelCartOnRefund :execrows
-- O reembolso mata a venda, e o carrinho precisa morrer junto.
--
-- Até 19/08 o estorno mexia em tudo (cobrança, cupom, e-mail, pedido no ERP)
-- MENOS no status do carrinho: ele ficava 'active' + payment 'refunded' para
-- sempre. Consequência dupla no painel: nunca aparecia em "Cancelados" (a aba
-- filtra por status do carrinho, de propósito) e ficava em "Precisam atenção"
-- eternamente, porque o matcher casa payment_status='refunded' e não existia
-- ação que o tirasse de lá — o cancelamento manual recusa carrinho não-pendente
-- ("pagamento vence"). Dois pedidos de teste reembolsados em 16/08 provaram o
-- beco: três dias parados na aba de triagem sem saída.
--
-- Guard-first e idempotente: só age sobre carrinho reembolsado ainda não
-- terminal. NÃO devolve estoque local nem toca fila — o fan-out do reembolso
-- (ReactOrderRefundedERP cancela o pedido no Tiny, que devolve o estoque lá; o
-- espelho traz o número de volta) é quem cuida disso.
UPDATE carts
SET status = 'cancelled', cancelled_reason = 'refunded'
WHERE id = $1
  AND payment_status = 'refunded'
  AND status NOT IN ('cancelled', 'expired');

-- name: GetCartERPOpAge :one
-- Há quanto tempo a operação ERP em curso começou, em segundos.
--
-- Separa "criação em voo agora" de "criação que morreu no meio" — no estado as
-- duas são idênticas ('converting' sem pedido), e só o relógio as distingue.
-- Zero quando não há marca, que é o caso de quem nunca começou.
SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - erp_op_started_at)), 0)::float8
FROM carts WHERE id = $1;

-- name: SumPromisedWithoutERPOrder :one
-- Unidades que a live JÁ prometeu e que o ERP ainda não conhece.
--
-- O saldo disponível do ERP é verdadeiro para o ERP e incompleto para nós
-- durante alguns segundos: entre o comentário baixar o contador local e o pedido
-- de venda existir lá, aquela unidade não aparece em `disponivel`. Gravar o
-- disponível cru nesse intervalo REABASTECE o portão com estoque que já tem
-- dono — e a unidade é oferecida duas vezes.
--
-- Só carrinhos VIVOS e ainda SEM pedido entram na conta: assim que o pedido
-- existe, o `disponivel` do ERP já o desconta, e somá-lo aqui seria descontar
-- duas vezes.
--
-- Carrinho PAGO sem pedido conta, e é o caso mais importante de todos: a venda
-- aconteceu, aquelas unidades têm dono, e o pedido só vai nascer quando a
-- confirmação rodar. Excluí-lo devolveria ao portão estoque já vendido.
--
-- Substitui a soma sobre erp_stock_movements, que media a mesma coisa num mundo
-- onde a reserva era um lançamento manual de estoque.
SELECT COALESCE(SUM(ci.quantity - ci.waitlisted_quantity), 0)::int
FROM cart_items ci
JOIN carts c ON c.id = ci.cart_id
JOIN products p ON p.id = ci.product_id
WHERE p.external_id = sqlc.arg(external_product_id)
  AND p.external_source = 'tiny'
  AND (c.external_order_id IS NULL OR c.external_order_id = '')
  AND c.status NOT IN ('expired', 'cancelled')
  AND (c.payment_status IS NULL OR c.payment_status <> 'refunded')
  AND ci.quantity > ci.waitlisted_quantity;

-- name: SetCartItemQuantityFromERP :exec
-- Ajusta a quantidade de um item do carrinho para refletir o pedido no ERP.
--
-- É o caminho de volta: o lojista mexeu no pedido pelo painel e o carrinho tem
-- de seguir, porque é o carrinho que a compradora vê e paga. Preserva a parcela
-- em fila de espera — ela não está no pedido e não é do lojista mexer.
--
-- Sem ON CONFLICT DO UPDATE isto seria um par ler-decidir-gravar, e duas
-- reflexões simultâneas do mesmo pedido se atropelariam.
INSERT INTO cart_items (cart_id, product_id, quantity, unit_price, waitlisted_quantity)
VALUES (sqlc.arg(cart_id)::uuid, sqlc.arg(product_id)::uuid, sqlc.arg(quantity), sqlc.arg(unit_price), 0)
ON CONFLICT (cart_id, product_id) DO UPDATE
SET quantity   = EXCLUDED.quantity + cart_items.waitlisted_quantity,
    unit_price = EXCLUDED.unit_price;

-- name: RemoveCartItemFromERP :exec
-- Tira do carrinho o item que o lojista apagou do pedido.
--
-- Só quando NÃO há parcela em fila: uma linha com fila representa gente
-- esperando, e apagá-la por causa de uma edição no painel do ERP mataria a
-- espera de alguém que nunca chegou a estar no pedido.
DELETE FROM cart_items
WHERE cart_id = sqlc.arg(cart_id)::uuid
  AND product_id = sqlc.arg(product_id)::uuid
  AND waitlisted_quantity = 0;

-- name: CartIsTerminated :one
-- O carrinho chegou a um fim de onde não sai sozinho.
--
-- Serve para reconhecer o pedido que ressuscitou no ERP com o carrinho morto
-- aqui — o que deixa uma unidade reservada sem ninguém para reclamá-la.
SELECT status IN ('cancelled', 'expired') AS terminado
FROM carts WHERE id = sqlc.arg(cart_id)::uuid;

-- name: ReopenCancelledCart :one
-- Traz de volta o carrinho que o lojista reabriu no ERP.
--
-- Espelho do CancelCart, e a simetria é proposital: aquele mata por decisão
-- humana, este ressuscita por decisão humana. O que ele NÃO faz é mexer no
-- pedido do ERP — o pedido já está vivo, foi o lojista quem o reabriu, e é
-- justamente isso que estamos seguindo.
--
-- Volta para 'checkout' e não para 'active' porque o carrinho cancelado já
-- tinha link: reativá-lo é o ponto, e é o que faz a compradora conseguir pagar.
--
-- Guarda: só carrinho morto por DECISÃO HUMANA — cancelado pelo lojista aqui
-- (`store_cancelled`) ou cancelado por ele no ERP (`erp_cancelled`). Os dois são
-- o mesmo gesto, de lados diferentes, e os dois se desfazem do mesmo jeito.
--
-- Aceitar `erp_cancelled` não é detalhe: sem isso a viagem de ida e volta pelo
-- Tiny quebrava na volta. Cancelar lá gravava `erp_cancelled`, e reabrir lá
-- encontrava um motivo que esta query não reconhecia — o pedido voltava a viver
-- no ERP e o carrinho ficava morto para sempre, que é exatamente a peça presa
-- sem dono que a reabertura existe para evitar.
--
-- Expirado continua de fora: prazo vencido é regra, não engano, e um clique no
-- ERP não passa por cima dela. Pago/estornado também não — aquela venda já teve
-- desfecho.
UPDATE carts
SET status                       = 'checkout',
    cancelled_reason             = NULL,
    cancellation_reverted_at     = now(),
    cancellation_reverted_reason = 'erp_reopened',
    -- O estado da máquina volta para 'open': o pedido existe e está vivo lá,
    -- então o próximo comentário muta a grade em vez de criar um pedido novo.
    erp_order_state              = 'open',
    erp_op_started_at            = NULL,
    -- Prazo re-armado a partir de AGORA. O antigo já passou (ou passaria em
    -- instantes), e devolver um carrinho que expira em seguida seria devolver
    -- nada.
    expires_at = CASE WHEN never_expires THEN NULL
                      ELSE now() + (sqlc.arg(minutos_de_prazo)::int || ' minutes')::interval END
WHERE id = sqlc.arg(cart_id)::uuid
  AND status = 'cancelled'
  AND cancelled_reason IN ('store_cancelled', 'erp_cancelled')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
RETURNING *;

-- name: TakeStockForReopen :one
-- Retoma o estoque de um item, aceitando levar MENOS do que pediu.
--
-- É a diferença entre este caminho e uma compra normal: na compra, o que não
-- tem estoque vira fila na hora do comentário. Aqui o carrinho já existia, as
-- unidades foram devolvidas no cancelamento, e no meio-tempo outra pessoa pode
-- ter levado. Recusar tudo por causa de uma unidade jogaria fora o carrinho
-- inteiro; levar o que há e mandar o resto para a fila devolve o máximo
-- possível — que é o que o lojista quer ao reabrir.
--
-- Devolve quanto CONSEGUIU tirar.
-- O RETURNING de um UPDATE enxerga o valor NOVO, então "quanto saiu" não sai
-- dele: precisa ser calculado ANTES de escrever. Por isso a leitura travada
-- vem primeiro, numa CTE, e a escrita usa o número que ela computou.
WITH atual AS (
    SELECT id, stock FROM products WHERE id = sqlc.arg(product_id)::uuid FOR UPDATE
), tomado AS (
    SELECT id, LEAST(GREATEST(stock, 0), sqlc.arg(desejado)::int) AS qtd FROM atual
), aplicado AS (
    UPDATE products p SET stock = p.stock - t.qtd
    FROM tomado t WHERE p.id = t.id
    RETURNING 1
)
SELECT qtd::int AS obtido FROM tomado;

-- name: SetCartItemWaitlistedOnReopen :exec
-- Marca, na linha do carrinho, quantas unidades ficaram esperando.
UPDATE cart_items
SET waitlisted_quantity = sqlc.arg(waitlisted)::int
WHERE cart_id = sqlc.arg(cart_id)::uuid AND product_id = sqlc.arg(product_id)::uuid;

-- name: GetCartEventID :one
SELECT event_id FROM carts WHERE id = $1;

-- name: CancelCartFromERPStatus :one
-- O carrinho segue o pedido que foi cancelado no ERP.
--
-- Igual ao CancelCart, com o motivo trocado: `erp_cancelled` em vez de
-- `store_cancelled`. A troca não é cosmética — o reactor que estorna no ERP só
-- reage a `store_cancelled`, e mandá-lo cancelar um pedido que já está
-- cancelado gastaria escrita do teto da conta para pedir o que o Tiny acabou
-- de contar.
--
-- Pago fica de fora: cancelar um pedido pago no ERP é decisão sobre dinheiro
-- que já entrou, e o estorno é do gateway. Esse caso vai para a triagem humana.
UPDATE carts
SET status = 'cancelled', cancelled_reason = 'erp_cancelled'
WHERE carts.id = $1
  AND status IN ('pending', 'active', 'checkout')
  AND (payment_status IS NULL OR payment_status NOT IN ('paid', 'refunded'))
RETURNING *;


-- name: ListCartGridItems :many
-- A grade que sobe para o ERP: os itens deste carrinho MAIS os de todos os
-- carrinhos juntados a ele.
--
-- Existe separada de ListNonWaitlistedCartItems de propósito. Aquela é usada
-- também pelo cancelamento e pela expiração, que devolvem estoque — e devolver
-- o estoque do carrinho VIZINHO ao cancelar este seria roubar a compra de outra
-- pessoa. A união vale só para a grade.
--
-- O mesmo produto pedido nos dois carrinhos vira UMA linha somada: o ERP aceita
-- um produto por linha, e mandar duas faria a segunda substituir a primeira.
SELECT p.external_id AS product_external_id,
       MIN(p.name)::text AS product_name,
       MIN(p.keyword)::text AS product_keyword,
       SUM(ci.quantity - ci.waitlisted_quantity)::int AS quantity,
       MAX(ci.unit_price)::bigint AS unit_price
FROM cart_items ci
JOIN products p ON p.id = ci.product_id
JOIN carts c ON c.id = ci.cart_id
WHERE COALESCE(c.joined_to_cart_id, c.id) = sqlc.arg(cart_id)::uuid
  AND ci.quantity > ci.waitlisted_quantity
  AND p.external_id IS NOT NULL AND p.external_id <> ''
GROUP BY p.external_id;

-- name: ListJoinedCartIDs :many
-- Os carrinhos que foram juntados a este. Vazio quando ele é independente.
SELECT id FROM carts WHERE joined_to_cart_id = sqlc.arg(cart_id)::uuid;

-- name: GetCartJoinHost :one
-- O anfitrião deste carrinho — ele mesmo quando não foi juntado a ninguém.
SELECT COALESCE(joined_to_cart_id, id) AS host_id FROM carts WHERE id = $1;

-- name: JoinCartIntoHost :execrows
-- Prende o carrinho ao anfitrião e o deixa sem pedido próprio.
--
-- O pedido dele foi cancelado no ERP logo antes — daqui em diante toda operação
-- de ERP deste carrinho resolve para o anfitrião, e é o pedido do anfitrião que
-- carrega o conteúdo dos dois.
--
-- Guards: nem anfitrião nem juntado pode já estar em outra junção. Cadeia de
-- dois níveis faria a resolução depender de quantos saltos existem.
UPDATE carts
SET joined_to_cart_id = sqlc.arg(host_id)::uuid,
    joined_at         = now(),
    external_order_id = NULL,
    erp_order_state   = 'none',
    erp_op_started_at = NULL
WHERE id = sqlc.arg(cart_id)::uuid
  AND joined_to_cart_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM carts o WHERE o.joined_to_cart_id = sqlc.arg(cart_id)::uuid)
  AND EXISTS (SELECT 1 FROM carts h WHERE h.id = sqlc.arg(host_id)::uuid AND h.joined_to_cart_id IS NULL);

-- name: GetCartForJoin :one
-- Tudo que a decisão de junção precisa saber de um pedido, numa leitura.
SELECT c.id, c.platform_user_id, c.platform_handle,
       COALESCE(c.external_order_id,'') AS external_order_id,
       c.created_at,
       (c.payment_status = 'paid')     AS paid,
       (c.payment_status = 'refunded') AS refunded,
       (c.status IN ('cancelled','expired')) AS terminated,
       COALESCE(c.erp_order_status,'') AS erp_order_status,
       (c.joined_to_cart_id IS NOT NULL
        OR EXISTS (SELECT 1 FROM carts o WHERE o.joined_to_cart_id = c.id)) AS already_joined
FROM carts c
JOIN live_events e ON e.id = c.event_id
WHERE c.id = sqlc.arg(cart_id)::uuid
  AND COALESCE(c.store_id, e.store_id) = sqlc.arg(store_id)::uuid;

-- name: ListJoinCandidates :many
-- Os pedidos que PODEM ser juntados a este.
--
-- Mesma loja, mesmo comprador, vivos, não faturados e fora de qualquer junção.
-- Compradores diferentes ficam de fora da lista de propósito: juntar a compra
-- de duas pessoas é possível, mas exige confirmação explícita do lojista — e
-- oferecê-la numa lista faria o clique errado parecer normal.
--
-- Evento DIFERENTE não é filtro, é consequência: carts_one_open_per_event_buyer
-- já impede dois carrinhos abertos do mesmo comprador na mesma campanha.
SELECT c.id, c.short_id, c.created_at, c.status,
       COALESCE(c.payment_status,'') AS payment_status,
       COALESCE(c.erp_order_number,'') AS erp_order_number,
       e.title AS event_title,
       cart_product_total_cents(c.id)::bigint AS total_cents,
       (SELECT COUNT(*) FROM cart_items ci WHERE ci.cart_id = c.id)::int AS item_count
FROM carts c
JOIN live_events e ON e.id = c.event_id
WHERE COALESCE(c.store_id, e.store_id) = sqlc.arg(store_id)::uuid
  AND c.id <> sqlc.arg(cart_id)::uuid
  AND c.platform_user_id = (SELECT platform_user_id FROM carts WHERE id = sqlc.arg(cart_id)::uuid)
  AND c.status IN ('pending','active','checkout','paid')
  AND (c.payment_status IS NULL OR c.payment_status <> 'refunded')
  AND (c.erp_order_status IS NULL OR c.erp_order_status NOT IN (
        'preparando_envio','faturado','pronto_envio','enviado','entregue','nao_entregue','cancelado'))
  AND c.joined_to_cart_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM carts o WHERE o.joined_to_cart_id = c.id)
ORDER BY c.created_at DESC
LIMIT 20;

-- name: GetCartJoinLink :one
-- O vínculo de junção deste pedido, para a tela mostrar.
SELECT
    COALESCE(c.joined_to_cart_id::text,'')::text AS joined_to_cart_id,
    COALESCE((SELECT h.short_id::text FROM carts h WHERE h.id = c.joined_to_cart_id),'')::text AS host_short_id,
    COALESCE((SELECT string_agg(o.short_id::text, ',') FROM carts o WHERE o.joined_to_cart_id = c.id),'')::text AS joined_short_ids,
    COALESCE((SELECT string_agg(o.id::text, ',') FROM carts o WHERE o.joined_to_cart_id = c.id),'')::text AS joined_cart_ids,
    c.joined_at
FROM carts c WHERE c.id = sqlc.arg(cart_id)::uuid;
