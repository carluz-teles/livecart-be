-- Cupom vale uma vez por COMPRADOR dentro da campanha — RN-33.
--
-- A única trava anti-empilhamento hoje é UNIQUE (cart_id) em
-- coupon_redemptions: um resgate por carrinho. Isso bastava enquanto existia no
-- máximo um carrinho por (evento, comprador). A 000105 acabou com essa
-- garantia: pagar abre outro carrinho na mesma campanha (RN-07), então o mesmo
-- cupom passaria a ser resgatável a cada compra da semana. Um "PRIMEIRACOMPRA"
-- de 20% viraria 20% em toda compra, consumindo o max_uses do lojista com um
-- desconto que ele não pretendia dar.
--
-- O comprador mora no carrinho, não no resgate, e índice único não enxerga
-- coluna de outra tabela. Por isso platform_user_id é denormalizado aqui —
-- derivado do cart no momento do INSERT, então não há como divergir.
--
-- Status que ocupam a vaga: 'reserved' e 'confirmed'. 'expired' e 'refunded'
-- liberam de propósito — em nenhum dos dois o comprador ficou com o benefício,
-- então ele pode usar o cupom de novo no ciclo seguinte.

ALTER TABLE coupon_redemptions
    ADD COLUMN IF NOT EXISTS platform_user_id VARCHAR;

-- Backfill a partir do carrinho. No-op numa base recém-resetada; existe para a
-- migration ser correta também onde já houver resgate. Não há órfão possível:
-- coupon_redemptions.cart_id é FK com ON DELETE CASCADE.
UPDATE coupon_redemptions r
SET platform_user_id = c.platform_user_id
FROM carts c
WHERE c.id = r.cart_id AND r.platform_user_id IS NULL;

ALTER TABLE coupon_redemptions
    ALTER COLUMN platform_user_id SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS coupon_redemptions_one_per_buyer
    ON coupon_redemptions (coupon_id, platform_user_id)
    WHERE status IN ('reserved', 'confirmed');

COMMENT ON COLUMN coupon_redemptions.platform_user_id IS
    'RN-33: comprador do carrinho, denormalizado para o índice único poder travar "um resgate por comprador por cupom". Preenchido a partir de carts no INSERT.';

COMMENT ON INDEX coupon_redemptions_one_per_buyer IS
    'RN-33: um resgate vivo por (cupom, comprador). Como coupons.event_id é NOT NULL, isso equivale a "uma vez por comprador na campanha". expired/refunded liberam a vaga.';
