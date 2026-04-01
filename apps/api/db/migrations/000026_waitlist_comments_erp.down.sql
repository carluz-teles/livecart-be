-- Drop tables in reverse order of creation
DROP TABLE IF EXISTS erp_contacts;
DROP TABLE IF EXISTS live_comments;
DROP TABLE IF EXISTS waitlist_items;
ALTER TABLE cart_items DROP COLUMN IF EXISTS waitlisted;
