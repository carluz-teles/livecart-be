-- name: ListCartPayments :many
-- O livro de pagamentos de um carrinho, na ordem em que o dinheiro entrou.
--
-- É daqui que saem as parcelas "PAGO" do pedido no ERP — uma por cobrança, com
-- data e valor —, em vez de um total mudo que não diz quando nem em quantas
-- vezes.
SELECT id, cart_id, amount_cents, gross_covered_cents, COALESCE(method,'') AS method,
       checkout_id, paid_at
FROM cart_payments
WHERE cart_id = $1
ORDER BY paid_at, created_at;

-- name: SumCartPayments :one
-- Quanto entrou e quanto de preço cheio isso liquidou. A diferença entre os
-- dois é o desconto concedido — cupom, PIX, ou os dois.
SELECT COALESCE(SUM(amount_cents), 0)::bigint        AS paid_cents,
       COALESCE(SUM(gross_covered_cents), 0)::bigint AS gross_covered_cents
FROM cart_payments WHERE cart_id = $1;
