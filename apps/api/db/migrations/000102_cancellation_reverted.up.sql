-- Cancelamento de carrinho revertido pelo pagamento (LIV-84).
--
-- Quando o lojista cancela um carrinho e o pagamento entra assim mesmo (PIX
-- pago com o QR já aberto, cartão em análise, webhook atrasado), a regra é
-- "pagamento vence": o pedido volta e segue o fluxo normal. Não há estorno
-- automático — se o lojista quiser devolver o dinheiro, faz por fora.
--
-- Duas coisas precisam ficar registradas por isso:
--   1. no PEDIDO, para o lojista ver no histórico o que aconteceu (e para dar
--      métrica de quantas vezes o caso acontece);
--   2. no SINO do painel, como aviso ativo — senão o lojista só descobre que
--      vendeu algo que julgava cancelado quando o cliente cobrar.

-- 1. Carimbo no carrinho. NULL = nunca aconteceu (a esmagadora maioria).
ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS cancellation_reverted_at TIMESTAMPTZ;

COMMENT ON COLUMN carts.cancellation_reverted_at IS
    'Quando um cancelamento manual do lojista foi revertido por um pagamento aprovado. NULL = nunca ocorreu.';

-- 2. Inbox do painel: a tabela nasceu só para as notificações de "ideias"
--    (type com CHECK de 3 valores e âncora em idea_id). Passa a aceitar também
--    fatos de PEDIDO, ancorados no cart_id.
ALTER TABLE notifications
    ADD COLUMN IF NOT EXISTS cart_id UUID REFERENCES carts(id) ON DELETE CASCADE;

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_type_check;

ALTER TABLE notifications
    ADD CONSTRAINT notifications_type_check
    CHECK (type IN (
        'idea_comment',
        'idea_reply',
        'idea_status_change',
        'order_cancellation_reverted'
    ));

-- Idempotência do aviso: o reactor roda sob entrega at-least-once do asynq, e
-- um retry não pode encher o sino do lojista com o mesmo fato. Parcial porque
-- as notificações de ideias têm cart_id NULL e podem repetir por natureza.
CREATE UNIQUE INDEX IF NOT EXISTS notifications_recipient_cart_type_uniq
    ON notifications (recipient_id, cart_id, type)
    WHERE cart_id IS NOT NULL;
