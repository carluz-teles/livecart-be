-- D11 — a fila e do EVENTO e nao admite duplicidade: um comprador entra UMA vez
-- por produto por evento, enquanto a entrada estiver VIVA.
--
-- waitlist_items nao tem NENHUM UNIQUE hoje: a 000026 cria so dois indices
-- comuns (idx_waitlist_event_product, idx_waitlist_status) e a 000073 so
-- adiciona colunas e o CHECK de status. A unica protecao contra duplicidade e
-- uma leitura FORA de transacao e SEM lock, cujo erro ainda e descartado
-- (GetWaitlistItemByEventUserProduct em integration/service.go). Dois
-- comentarios simultaneos do mesmo comprador passam pelos dois lados — da para
-- furar fila entrando varias vezes.
--
-- A14: esta e a unica migration do epico que pode FALHAR por dado
-- pre-existente. Rodar ANTES, e conversar com o lojista se devolver linha:
--   SELECT event_id, product_id, platform_user_id, count(*)
--   FROM waitlist_items WHERE status IN ('waiting','notified')
--   GROUP BY 1,2,3 HAVING count(*) > 1;
-- Cada linha ai e posicao de fila de um comprador real que vai sumir.
--
-- O predicado e EXATAMENTE ('waiting','notified'). Incluir expired/fulfilled/
-- cancelled proibiria o comprador de voltar para a fila depois — comportamento
-- legitimo hoje.

-- ---------------------------------------------------------------------------
-- 1. Colapsa duplicatas vivas, mantendo a MELHOR posicao de cada comprador.
--    Mesma tecnica da 000091 (dedup antes do unique).
-- ---------------------------------------------------------------------------
WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY event_id, product_id, platform_user_id
               ORDER BY position, created_at, id
           ) AS rn
    FROM waitlist_items
    WHERE status IN ('waiting', 'notified')
)
UPDATE waitlist_items wi
SET status = 'cancelled',
    cancelled_at = COALESCE(wi.cancelled_at, now())
FROM ranked r
WHERE wi.id = r.id AND r.rn > 1;

-- ---------------------------------------------------------------------------
-- 2. A constraint.
-- ---------------------------------------------------------------------------
CREATE UNIQUE INDEX IF NOT EXISTS uq_waitlist_live_entry
    ON waitlist_items (event_id, product_id, platform_user_id)
    WHERE status IN ('waiting', 'notified');

COMMENT ON INDEX uq_waitlist_live_entry IS
    'D11: um comprador so pode ter UMA entrada viva na fila por (evento, produto). Entradas expired/fulfilled/cancelled ficam fora do predicado, entao voltar para a fila continua permitido.';
