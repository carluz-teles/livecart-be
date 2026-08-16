-- Valor MINIMO de uma parcela no cartao.
--
-- O checkout oferecia 1 a 12 parcelas fixas, dividindo o total por N. Numa venda
-- de R$ 60 isso vira "12x de R$ 5,00": o lojista paga a MDR de doze parcelas
-- sobre sessenta reais e nao tem como impedir.
--
-- Nao da para resolver do lado do Pagar.me. A API transparente que usamos aceita
-- apenas `installments: N` no objeto credit_card — `installments_setup`, com
-- `amount` (parcela minima) e `free_installments`, so existe no Checkout
-- hospedado e nos Links de pagamento. Quem decide quais parcelas oferecer no
-- fluxo transparente e o integrador, ou seja, nos.
--
-- Zero = sem minimo, que e exatamente o comportamento de hoje. Nenhuma loja muda
-- de comportamento ao subir esta migration.
ALTER TABLE stores
  ADD COLUMN IF NOT EXISTS min_installment_cents INT NOT NULL DEFAULT 0
    CHECK (min_installment_cents >= 0);

COMMENT ON COLUMN stores.min_installment_cents IS
  'Valor minimo de uma parcela no cartao, em centavos. 0 = sem minimo.';
