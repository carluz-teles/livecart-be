DROP INDEX IF EXISTS coupon_redemptions_one_per_buyer;

ALTER TABLE coupon_redemptions DROP COLUMN IF EXISTS platform_user_id;
