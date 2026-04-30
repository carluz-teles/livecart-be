-- Persist card-specific payment metadata on the cart so the public checkout
-- comprovante can render brand/last4/installments after the buyer pays.
-- These fields are populated by the transparent card flow on approval; PIX
-- carts and pre-existing paid carts will leave them NULL — the public API
-- omits them when missing and the frontend treats absence as "indisponível".

ALTER TABLE carts ADD COLUMN IF NOT EXISTS card_brand VARCHAR;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS card_last_four VARCHAR;
ALTER TABLE carts ADD COLUMN IF NOT EXISTS card_installments INT;
