-- O identificador do PIX vivo, e quanto ele cobra.
--
-- `checkout_id` nao serve para cancelar. No Pagar.me ele guarda a ORDEM
-- (`or_...`) e o cancelamento exige a COBRANCA (`ch_...`), que a resposta traz e
-- a gente descartava. No Mercado Pago os dois sao o mesmo numero, mas gravar em
-- coluna propria deixa o contrato explicito em vez de depender do provedor.
--
-- Sem isto, cada geracao de PIX abre uma cobranca nova e a anterior fica viva no
-- gateway ate expirar. O comprador que copiou o codigo antes de mexer no
-- carrinho consegue pagar o valor velho, e o webhook chega com uma quantia que
-- nao corresponde a nenhum carrinho atual.
ALTER TABLE carts
  ADD COLUMN IF NOT EXISTS pix_charge_id VARCHAR,
  ADD COLUMN IF NOT EXISTS pix_amount_cents BIGINT;
