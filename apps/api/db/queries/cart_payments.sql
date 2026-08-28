-- name: ListCartPayments :many
-- O livro de pagamentos de um carrinho, na ordem em que o dinheiro entrou.
--
-- É daqui que saem as parcelas "PAGO" do pedido no ERP — uma por cobrança, com
-- data e valor —, em vez de um total mudo que não diz quando nem em quantas
-- vezes.
--
-- Soma o GRUPO: as cobranças deste carrinho e as dos que foram juntados a ele.
-- O pedido no ERP é um só e carrega o conteúdo dos dois, então o extrato dele
-- tem de mostrar todo o dinheiro que entrou por aquele conteúdo. Ler só um
-- carrinho faria a compra do outro aparecer como "a pagar" — cobrando de novo o
-- que já foi pago.
SELECT cp.id, cp.cart_id, cp.amount_cents, cp.gross_covered_cents, COALESCE(cp.method,'') AS method,
       cp.checkout_id, cp.paid_at
FROM cart_payments cp
JOIN carts c ON c.id = cp.cart_id
WHERE COALESCE(c.joined_to_cart_id, c.id) = $1
ORDER BY cp.paid_at, cp.created_at;

-- name: SumCartPayments :one
-- Quanto entrou e quanto de preço cheio isso liquidou. A diferença entre os
-- dois é o desconto concedido — cupom, PIX, ou os dois.
SELECT COALESCE(SUM(amount_cents), 0)::bigint        AS paid_cents,
       COALESCE(SUM(gross_covered_cents), 0)::bigint AS gross_covered_cents
FROM cart_payments WHERE cart_id = $1;
