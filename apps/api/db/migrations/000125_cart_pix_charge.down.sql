ALTER TABLE carts
  DROP COLUMN IF EXISTS pix_charge_id,
  DROP COLUMN IF EXISTS pix_amount_cents;
