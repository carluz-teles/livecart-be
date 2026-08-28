-- Juntar pedidos NO ERP, mantendo-os separados no LiveCart.
--
-- O lojista pede isto o tempo todo: a compradora já tinha um pedido em aberto —
-- comprou fora da live, pelo WhatsApp, na loja — e aí comenta na transmissão. O
-- LiveCart cria o pedido dele, e ficam dois pedidos da mesma pessoa no ERP, cada
-- um segurando peça, cada um querendo o seu frete e a sua nota.
--
-- A junção acontece só de um lado. No ERP vira UM pedido, com tudo dentro, um
-- frete e uma nota. No LiveCart os dois carrinhos continuam existindo, cada um
-- com o seu histórico, o seu link e o seu pagamento — porque foram duas compras
-- e o lojista precisa poder olhar cada uma. A tela mostra que estão juntos.
--
-- ═══ COMO O VÍNCULO FUNCIONA ═══
--
-- Um dos carrinhos é o ANFITRIÃO e é dele o pedido no ERP. Os outros apontam
-- para ele por joined_to_cart_id e deixam de ter pedido próprio — o deles é
-- cancelado no momento da junção.
--
-- A partir daí, toda operação de ERP feita sobre um carrinho juntado resolve
-- para o anfitrião: a leitura do estado, a trava de escrita, a grade que sobe.
-- Isso não é conveniência — é o que impede os dois carrinhos de escreverem a
-- mesma grade ao mesmo tempo. A trava é por linha de carrinho, e sem a
-- resolução seriam duas travas diferentes para um pedido só.

ALTER TABLE carts
    ADD COLUMN IF NOT EXISTS joined_to_cart_id UUID REFERENCES carts(id),
    -- Quando a junção foi feita e por quem, para o histórico do pedido.
    ADD COLUMN IF NOT EXISTS joined_at TIMESTAMPTZ;

-- Um carrinho não pode ser anfitrião e juntado ao mesmo tempo: cadeia de dois
-- níveis faria a resolução do anfitrião depender de quantos saltos existem, e um
-- ciclo a travaria para sempre. O serviço recusa juntar um carrinho que já é
-- anfitrião de outro; este índice é a rede embaixo disso.
CREATE INDEX IF NOT EXISTS idx_carts_joined_to ON carts (joined_to_cart_id)
    WHERE joined_to_cart_id IS NOT NULL;

-- Autorreferência é a forma mais barata de fazer um ciclo de um nó só.
ALTER TABLE carts DROP CONSTRAINT IF EXISTS carts_join_not_self;
ALTER TABLE carts ADD CONSTRAINT carts_join_not_self
    CHECK (joined_to_cart_id IS NULL OR joined_to_cart_id <> id);

COMMENT ON COLUMN carts.joined_to_cart_id IS
    'Carrinho ANFITRIÃO desta junção — é dele o pedido no ERP. NULO = carrinho independente ou anfitrião.';
