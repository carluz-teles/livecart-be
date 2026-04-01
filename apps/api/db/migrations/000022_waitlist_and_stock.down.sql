-- Rollback 000022

ALTER TABLE stores DROP COLUMN IF EXISTS waitlist_expiry_minutes;
ALTER TABLE stores DROP COLUMN IF EXISTS waitlist_claim_minutes;
ALTER TABLE cart_items DROP COLUMN IF EXISTS waitlisted;

DROP TABLE IF EXISTS erp_contacts;
DROP TABLE IF EXISTS waitlist_items;
DROP TABLE IF EXISTS live_comments;
