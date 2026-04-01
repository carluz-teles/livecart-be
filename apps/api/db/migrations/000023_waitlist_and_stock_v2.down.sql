-- Rollback 000023
DROP TABLE IF EXISTS erp_contacts;
DROP TABLE IF EXISTS waitlist_items;
DROP TABLE IF EXISTS live_comments;

ALTER TABLE cart_items DROP COLUMN IF EXISTS waitlisted;
ALTER TABLE stores DROP COLUMN IF EXISTS waitlist_claim_minutes;
ALTER TABLE stores DROP COLUMN IF EXISTS waitlist_expiry_minutes;
