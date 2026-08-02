-- Drop the vestigial `payments` table (Bloco B1f — fecha extração Payment).
-- The 1:N payments table (added in 000043) is dead: it has zero Go callers —
-- all of its sqlc queries (CreatePayment/GetPayment*/ListPayments*/UpdatePayment*/…)
-- were unused, and payment state lives on `carts.payment_*` (checkout/order) and,
-- for the Order module, on `order_payments`. Neither of those is touched here.
DROP TABLE IF EXISTS payments;
