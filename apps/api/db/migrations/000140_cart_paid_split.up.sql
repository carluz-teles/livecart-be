-- O que já foi pago e o que veio depois, separados dentro do carrinho.
--
-- O carrinho deixou de morrer no pagamento. O lojista que junta compras quer o
-- pedido aberto até faturar: a compradora pagou R$ 40 na live de segunda, pediu
-- mais uma coisa na quinta, e sai tudo numa caixa só — um frete, uma nota. Isso
-- só funciona se o carrinho souber dizer o que o dinheiro que entrou já cobriu.
--
-- A contagem é por UNIDADE, não por linha, e a razão é a chave do upsert:
-- cart_items é único por (cart_id, product_id), então pedir de novo o MESMO
-- produto soma na linha que já existe. Uma marca de "esta linha está paga" viraria
-- mentira em silêncio — a linha diria "paga" carregando uma unidade que ninguém
-- pagou. Guardando quantas unidades daquela linha o pagamento cobriu, "2 pagas e
-- 1 a pagar do mesmo produto" é uma frase que o dado consegue dizer.
--
-- Derivar de horário também não serviria: cart_items não tem carimbo de criação,
-- e "antes das 20h03" não é o mesmo que "coberto pelo PIX de R$ 40".

ALTER TABLE cart_items
    -- Quantas unidades desta linha algum pagamento já cobriu. O que falta pagar
    -- é (quantity - paid_quantity), e é sempre ≥ 0 pela regra abaixo.
    ADD COLUMN IF NOT EXISTS paid_quantity INTEGER NOT NULL DEFAULT 0;

ALTER TABLE carts
    -- Quanto entrou de fato, somando todos os pagamentos do carrinho. Não é
    -- derivável do total: depois de um item novo, o total sobe e o pago não.
    ADD COLUMN IF NOT EXISTS paid_amount_cents BIGINT NOT NULL DEFAULT 0;

-- Reduzir a quantidade abaixo do que já foi pago não pode deixar um pago maior
-- que o total da linha — isso viraria "falta pagar" negativo na tela. O ajuste é
-- aqui, e não em cada UPDATE, porque são muitos os caminhos que mexem em
-- quantity (reflexo do ERP, remoção pelo lojista, fila de espera).
CREATE OR REPLACE FUNCTION cart_items_clamp_paid_quantity()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.paid_quantity > NEW.quantity THEN
        NEW.paid_quantity := NEW.quantity;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_cart_items_clamp_paid_quantity ON cart_items;
CREATE TRIGGER trg_cart_items_clamp_paid_quantity
    BEFORE INSERT OR UPDATE ON cart_items
    FOR EACH ROW EXECUTE FUNCTION cart_items_clamp_paid_quantity();

-- O que falta pagar, numa fórmula só — irmã de cart_product_total_cents, que é
-- a fonte única do total desde a 000093. Duas grafias da mesma conta divergem;
-- uma só, não.
CREATE OR REPLACE FUNCTION cart_unpaid_total_cents(p_cart_id uuid)
RETURNS bigint LANGUAGE sql STABLE AS $$
  SELECT COALESCE(SUM((quantity - paid_quantity) * unit_price), 0)::bigint
  FROM cart_items WHERE cart_id = p_cart_id AND paid_quantity < quantity;
$$;

-- Backfill: em todo carrinho já pago, tudo que está lá dentro foi pago junto.
-- É verdade por construção — até esta migration nada podia ser acrescentado
-- depois do pagamento.
UPDATE cart_items ci
SET paid_quantity = ci.quantity
FROM carts c
WHERE ci.cart_id = c.id
  AND ci.paid_quantity = 0
  AND (c.status = 'paid' OR c.payment_status = 'paid');

UPDATE carts c
SET paid_amount_cents = cart_product_total_cents(c.id)
WHERE (c.status = 'paid' OR c.payment_status = 'paid')
  AND c.paid_amount_cents = 0;

-- A pergunta quente é "o que falta pagar neste carrinho?". Parcial porque a
-- linha totalmente paga é a maioria esmagadora e não interessa a essa varredura.
CREATE INDEX IF NOT EXISTS idx_cart_items_unpaid
    ON cart_items (cart_id)
    WHERE paid_quantity < quantity;
