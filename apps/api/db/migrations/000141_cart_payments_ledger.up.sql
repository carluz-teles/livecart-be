-- Um pedido pode receber vários pagamentos.
--
-- O carrinho deixou de morrer no pagamento: ele recebe item até virar nota, e
-- cada rodada de compra pode ser paga na hora. "Pagou R$ 40 na segunda, R$ 105
-- na quinta" é uma frase que precisa caber no dado — e carts.paid_amount_cents,
-- que é um total só, não a comporta: ele diz quanto entrou, nunca em quantas
-- vezes nem quando.
--
-- ═══ POR QUE DUAS COLUNAS DE VALOR ═══
--
-- amount_cents é o que o gateway confirmou: dinheiro que existe.
-- gross_covered_cents é quanto de PREÇO CHEIO aquela cobrança liquidou.
--
-- Os dois diferem sempre que há desconto de cupom ou de PIX, e a diferença é
-- justamente o desconto. Guardar as duas observações, em vez de uma observação
-- e um desconto inferido, é o que permite conferir a conta depois: sem isso,
-- uma cobrança a menor (erro de gateway, cobrança parcial) fica indistinguível
-- de um desconto legítimo, e o pedido no ERP passaria a afirmar que a
-- compradora ainda deve um valor que ela nunca foi cobrada — ou o contrário.
--
-- Isso importa porque as parcelas do ERP são FORÇADAS a somar o total do
-- pedido, e o total do ERP é o preço CHEIO (valorDesconto é gravável só na
-- criação — medido em 26/08/2026: PUT /pedidos com valorDesconto devolve 204 e
-- ignora o campo; linha com valor negativo é recusada com "Este valor deve ser
-- maior que 0"). Então o desconto não pode sumir da conta: ele vira uma parcela
-- explícita, "DESCONTO PIX", e é isso que faz a soma fechar sem inventar um
-- saldo a pagar que não existe.

CREATE TABLE IF NOT EXISTS cart_payments (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cart_id            UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    -- O que ENTROU. É o número que vira a parcela "PAGO" no ERP.
    amount_cents       BIGINT NOT NULL,
    -- Quanto de preço cheio esta cobrança liquidou, frete incluído.
    -- (gross_covered_cents - amount_cents) é o desconto daquela cobrança.
    gross_covered_cents BIGINT NOT NULL,
    method             VARCHAR(32),
    -- Id do gateway. É a chave de idempotência: o webhook reentrega o mesmo
    -- pagamento até dez vezes, e reentrega não é segundo pagamento.
    checkout_id        VARCHAR(128) NOT NULL,
    paid_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (cart_id, checkout_id)
);

CREATE INDEX IF NOT EXISTS idx_cart_payments_cart ON cart_payments (cart_id, paid_at);

-- Backfill: todo carrinho já pago tem exatamente um pagamento, o que a coluna
-- acumulada registrou. O bruto coberto é o total do carrinho mais o frete — era
-- tudo que existia lá dentro quando ele foi pago.
INSERT INTO cart_payments (cart_id, amount_cents, gross_covered_cents, method, checkout_id, paid_at)
SELECT c.id,
       c.paid_amount_cents,
       cart_product_total_cents(c.id) + COALESCE(c.shipping_cost_cents, 0),
       c.payment_method,
       COALESCE(NULLIF(c.checkout_id, ''), 'backfill-' || c.id::text),
       COALESCE(c.paid_at, c.created_at, NOW())
FROM carts c
WHERE c.paid_amount_cents > 0
ON CONFLICT (cart_id, checkout_id) DO NOTHING;
