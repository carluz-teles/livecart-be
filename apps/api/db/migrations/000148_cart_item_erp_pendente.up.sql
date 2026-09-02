-- A LINHA QUE O ERP NÃO CONHECE.
--
-- O reflexo do pedido apaga do carrinho toda linha que não está no ERP, e a
-- premissa está escrita nele: "é o lojista removendo o item". Existe um segundo
-- jeito de uma linha não estar lá — a NOSSA escrita ter falhado.
--
-- Produção, 01/09/2026, @dany.lifestyle: ela comentou 2091, o item entrou no
-- carrinho, ela recebeu a DM ("Novo item adicionado: Pote com Tampa Pinha –
-- 11cm"), e a escrita no Tiny morreu no meio da tempestade de 429 daquela live.
-- Um dia depois alguém editou o pedido no Tiny, o reflexo rodou — 8 mudanças —
-- e apagou a linha, achando que o lojista a tinha removido. A compradora tinha
-- sido avisada de que comprou; a loja não via o item; nada registrou a remoção.
--
-- `erp_pending_since` é a diferença entre as duas histórias. NULL é o estado
-- normal e o padrão de toda linha existente, então nada muda para o que já
-- está no banco: só quem falhou é marcado, e só quem está marcado é protegido.
ALTER TABLE cart_items
    ADD COLUMN IF NOT EXISTS erp_pending_since TIMESTAMPTZ;

COMMENT ON COLUMN cart_items.erp_pending_since IS
    'Quando a escrita desta linha no ERP falhou. NULL = o ERP conhece a linha (ou nunca precisou conhecer). Não-NULL = a linha existe só aqui, e o reflexo NÃO pode apagá-la — ela precisa ser reenviada.';

-- Índice parcial: a varredura de reenvio pergunta "quem está pendente?", e a
-- resposta normal é ninguém. Índice sobre a exceção, não sobre a tabela.
CREATE INDEX IF NOT EXISTS idx_cart_items_erp_pendente
    ON cart_items (erp_pending_since)
    WHERE erp_pending_since IS NOT NULL;
