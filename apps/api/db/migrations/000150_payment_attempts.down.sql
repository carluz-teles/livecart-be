ALTER TABLE carts DROP COLUMN pix_cancel_lease_until;
ALTER TABLE carts DROP COLUMN payment_review_required;
DROP TABLE payment_attempts;
DROP FUNCTION cart_payment_fingerprint(uuid);
