ALTER TABLE carts DROP COLUMN IF EXISTS coupon_discount_cents;
ALTER TABLE carts DROP COLUMN IF EXISTS coupon_code;
ALTER TABLE carts DROP COLUMN IF EXISTS coupon_id;

DROP TABLE IF EXISTS coupon_redemptions;
DROP TABLE IF EXISTS coupons;
